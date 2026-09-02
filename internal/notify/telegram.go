package notify

import (
	"context"
	"fmt"
	"log"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// levelEmoji prefixes a Notify message body per SPEC.md Bölüm 6.8's event
// list — LevelCritical (breaker trip, data verify failure) gets the loudest
// marker so it is never mistaken for routine chatter in a busy chat.
func levelEmoji(l Level) string {
	switch l {
	case LevelCritical:
		return "🔴 KRİTİK"
	case LevelWarning:
		return "⚠️"
	default:
		return "ℹ️"
	}
}

// TelegramNotifier implements notify.Notifier over the Telegram Bot API
// (github.com/go-telegram/bot), per SPEC.md Bölüm 6.8. It is the ONLY
// place in the codebase that accepts an inbound command: every other
// package (panel included, per SPEC.md Bölüm 7.1 "panel hiçbir yazma
// işlemi yapmaz") is read-only. That makes the allowed_chat_id check in
// handleCallback the single security boundary a bad actor who somehow
// learns the bot's username would have to get through — SPEC.md Bölüm
// 6.8: "Bot yalnızca konfigürasyondaki telegram.allowed_chat_id'den gelen
// komutları kabul eder. Başka chat'ten gelen her şey sessizce yok
// sayılır ve loglanır."
type TelegramNotifier struct {
	bot           *tgbot.Bot
	allowedChatID int64
	approvals     chan Decision
	logf          func(format string, args ...any)
}

// Option configures NewTelegramNotifier.
type Option func(*telegramConfig)

type telegramConfig struct {
	httpClient  tgbot.HttpClient
	pollTimeout time.Duration
	logf        func(format string, args ...any)
	approvalCap int
}

// WithHTTPClient overrides the HTTP client the underlying bot.Bot uses —
// tests use this to inject a stub that never touches the network.
func WithHTTPClient(pollTimeout time.Duration, client tgbot.HttpClient) Option {
	return func(c *telegramConfig) {
		c.httpClient = client
		c.pollTimeout = pollTimeout
	}
}

// WithLogf overrides where "ignored, not from allowed_chat_id" and other
// operational lines are logged (default: the standard library logger).
func WithLogf(logf func(format string, args ...any)) Option {
	return func(c *telegramConfig) { c.logf = logf }
}

// NewTelegramNotifier constructs a TelegramNotifier bound to token
// (secrets.TELEGRAM_BOT_TOKEN) and allowedChatID (config.yaml
// telegram.allowed_chat_id, overridden by secrets.TELEGRAM_CHAT_ID per
// SPEC.md Bölüm 8 — internal/config already resolves that override, this
// constructor just takes the final value).
//
// Construction never calls the Telegram API (bot.WithSkipGetMe()): a token
// typo surfaces on the first real Notify/ProposeTrade call instead of at
// startup, which keeps this constructor usable in tests without a live
// network call and keeps `swingbot paper start` from refusing to boot on a
// transient Telegram outage before it has even read today's data.
func NewTelegramNotifier(token string, allowedChatID int64, opts ...Option) (*TelegramNotifier, error) {
	if token == "" {
		return nil, fmt.Errorf("notify: telegram bot token boş (SPEC.md Bölüm 8: .env TELEGRAM_BOT_TOKEN)")
	}
	if allowedChatID == 0 {
		return nil, fmt.Errorf("notify: telegram.allowed_chat_id ayarlanmamış (SPEC.md Bölüm 8)")
	}

	cfg := telegramConfig{logf: log.Printf, approvalCap: 32}
	for _, o := range opts {
		o(&cfg)
	}

	n := &TelegramNotifier{
		allowedChatID: allowedChatID,
		approvals:     make(chan Decision, cfg.approvalCap),
		logf:          cfg.logf,
	}

	botOpts := []tgbot.Option{
		tgbot.WithSkipGetMe(),
		tgbot.WithCallbackQueryDataHandler(CallbackApprove+":", tgbot.MatchTypePrefix, n.handleDecision(true)),
		tgbot.WithCallbackQueryDataHandler(CallbackReject+":", tgbot.MatchTypePrefix, n.handleDecision(false)),
		tgbot.WithCallbackQueryDataHandler("detay:", tgbot.MatchTypePrefix, n.handleDetail),
	}
	if cfg.httpClient != nil {
		botOpts = append(botOpts, tgbot.WithHTTPClient(cfg.pollTimeout, cfg.httpClient))
	}

	b, err := tgbot.New(token, botOpts...)
	if err != nil {
		return nil, fmt.Errorf("notify: telegram bot oluşturulamadı: %w", err)
	}
	n.bot = b
	return n, nil
}

// Start begins long-polling Telegram for updates (approve/reject button
// presses). It blocks until ctx is canceled — callers run it in a
// goroutine (see cmd/swingbot's `paper start`).
func (n *TelegramNotifier) Start(ctx context.Context) {
	n.bot.Start(ctx)
}

// Approvals implements notify.Notifier.
func (n *TelegramNotifier) Approvals() <-chan Decision {
	return n.approvals
}

// ProposeTrade implements notify.Notifier: sends SPEC.md Bölüm 6.8's entry
// proposal template with the Onayla/Reddet/Detay inline keyboard.
func (n *TelegramNotifier) ProposeTrade(ctx context.Context, p Proposal) error {
	kb := models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{
				{Text: "✅ Onayla", CallbackData: EncodeCallback(CallbackApprove, p.ID)},
				{Text: "❌ Reddet", CallbackData: EncodeCallback(CallbackReject, p.ID)},
				{Text: "📊 Detay", CallbackData: EncodeCallback("detay", p.ID)},
			},
		},
	}
	_, err := n.bot.SendMessage(ctx, &tgbot.SendMessageParams{
		ChatID:      n.allowedChatID,
		Text:        FormatProposalMessage(p),
		ReplyMarkup: kb,
	})
	if err != nil {
		return fmt.Errorf("notify: proposeTrade %s: %w", p.Symbol, err)
	}
	return nil
}

// Notify implements notify.Notifier: a plain, level-prefixed message with
// no keyboard — used for every event SPEC.md Bölüm 6.8 requires reporting
// besides an entry proposal (stop tetiklenmesi, devre kesici, veri kalitesi
// hatası, emir hatası, günlük özet).
func (n *TelegramNotifier) Notify(ctx context.Context, level Level, title, body string) error {
	text := fmt.Sprintf("%s %s\n\n%s", levelEmoji(level), title, body)
	_, err := n.bot.SendMessage(ctx, &tgbot.SendMessageParams{
		ChatID: n.allowedChatID,
		Text:   text,
	})
	if err != nil {
		return fmt.Errorf("notify: notify %q: %w", title, err)
	}
	return nil
}

// handleDecision returns a bot.HandlerFunc that parses an
// "approve:<id>"/"reject:<id>" callback, enforces the allowed_chat_id
// check, acknowledges the tap (AnswerCallbackQuery — Telegram shows a
// loading spinner on the button until this is called) and, only for an
// allowed chat, publishes the Decision.
func (n *TelegramNotifier) handleDecision(approved bool) tgbot.HandlerFunc {
	return func(ctx context.Context, b *tgbot.Bot, update *models.Update) {
		cq := update.CallbackQuery
		if cq == nil {
			return
		}
		_, proposalID, ok := ParseCallback(cq.Data)
		if !ok {
			return
		}

		if !n.chatAllowed(cq) {
			n.logf("notify: allowed_chat_id olmayan bir chat'ten karar denemesi yok sayıldı (proposal=%s)", proposalID)
			n.ackCallback(ctx, cq.ID, "")
			return
		}

		n.approvals <- Decision{ProposalID: proposalID, Approved: approved, At: time.Now().UTC()}

		ackText := "Reddedildi"
		if approved {
			ackText = "Onaylandı"
		}
		n.ackCallback(ctx, cq.ID, ackText)
	}
}

// handleDetail answers a "detay:<id>" tap with a short alert instead of a
// Decision — SPEC.md Bölüm 6.8's "[ 📊 Detay ]" button is informational
// only, it never changes a proposal's status.
func (n *TelegramNotifier) handleDetail(ctx context.Context, b *tgbot.Bot, update *models.Update) {
	cq := update.CallbackQuery
	if cq == nil {
		return
	}
	if !n.chatAllowed(cq) {
		n.logf("notify: allowed_chat_id olmayan bir chat'ten detay denemesi yok sayıldı")
		n.ackCallback(ctx, cq.ID, "")
		return
	}
	n.ackCallback(ctx, cq.ID, "Detaylar panelde: swingbot serve")
}

// chatAllowed implements SPEC.md Bölüm 6.8's security rule: only
// allowedChatID may issue a decision. The chat ID is read from the
// callback's own message when available (a private chat's message.chat.id
// equals the user's own ID) and falls back to the tapping user's ID for an
// inaccessible message, so an expired/removed message never bypasses the
// check just because Telegram can no longer return its Chat.
func (n *TelegramNotifier) chatAllowed(cq *models.CallbackQuery) bool {
	if cq.Message.Message != nil {
		return cq.Message.Message.Chat.ID == n.allowedChatID
	}
	if cq.Message.InaccessibleMessage != nil {
		return cq.Message.InaccessibleMessage.Chat.ID == n.allowedChatID
	}
	return cq.From.ID == n.allowedChatID
}

// ackCallback answers a callback query so Telegram stops showing the
// button's loading spinner. Errors are logged, not returned — a failed ack
// must never prevent the Decision that was already published above from
// being processed.
func (n *TelegramNotifier) ackCallback(ctx context.Context, callbackQueryID, text string) {
	params := &tgbot.AnswerCallbackQueryParams{CallbackQueryID: callbackQueryID}
	if text != "" {
		params.Text = text
	}
	if _, err := n.bot.AnswerCallbackQuery(ctx, params); err != nil {
		n.logf("notify: answerCallbackQuery hata: %v", err)
	}
}
