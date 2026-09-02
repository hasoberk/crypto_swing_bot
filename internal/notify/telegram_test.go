package notify

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
)

// fakeTelegramAPI is a stub HttpClient (github.com/go-telegram/bot's
// bot.HttpClient interface: Do(*http.Request) (*http.Response, error)) that
// never touches the network. It records every call so tests can assert on
// what TelegramNotifier sent, and lets a test script a canned response per
// Telegram method (sendMessage, answerCallbackQuery, getUpdates, ...).
//
// Requests are multipart/form-data (that is how github.com/go-telegram/bot
// encodes every call, see its rawRequest/buildRequestForm) — each field
// (chat_id, text, reply_markup, ...) is decoded back to a plain string here,
// which is enough for this suite's assertions.
type fakeTelegramAPI struct {
	mu    sync.Mutex
	calls []call
	// responses maps a Telegram Bot API method name (the last path segment,
	// e.g. "sendMessage") to the raw JSON `result` field to reply with.
	// Missing entries default to `true`, which satisfies every method this
	// test suite calls.
	responses map[string]string
}

type call struct {
	method string
	form   map[string]string
}

func newFakeTelegramAPI() *fakeTelegramAPI {
	return &fakeTelegramAPI{responses: map[string]string{}}
}

func (f *fakeTelegramAPI) Do(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	parts := strings.Split(req.URL.Path, "/")
	method := parts[len(parts)-1]

	form := map[string]string{}
	if err := req.ParseMultipartForm(10 << 20); err == nil && req.MultipartForm != nil {
		for k, vs := range req.MultipartForm.Value {
			if len(vs) > 0 {
				form[k] = vs[0]
			}
		}
	}
	f.calls = append(f.calls, call{method: method, form: form})

	result := f.responses[method]
	if result == "" {
		result = "true"
	}
	payload := fmt.Sprintf(`{"ok":true,"result":%s}`, result)
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(bytes.NewBufferString(payload)),
		Header:     make(http.Header),
	}, nil
}

func (f *fakeTelegramAPI) callsFor(method string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.method == method {
			out = append(out, c)
		}
	}
	return out
}

func newTestNotifier(t *testing.T, allowedChatID int64) (*TelegramNotifier, *fakeTelegramAPI) {
	t.Helper()
	api := newFakeTelegramAPI()
	// sendMessage's real result is a Message object, not a bare bool —
	// the bot library decodes `result` into models.Message and errors on
	// a type mismatch.
	api.responses["sendMessage"] = `{"message_id":1,"date":0,"chat":{"id":1,"type":"private"}}`
	n, err := NewTelegramNotifier("123456:test-token", allowedChatID, WithHTTPClient(time.Second, api))
	if err != nil {
		t.Fatalf("NewTelegramNotifier: %v", err)
	}
	return n, api
}

func TestNewTelegramNotifierRejectsMissingCredentials(t *testing.T) {
	if _, err := NewTelegramNotifier("", 42); err == nil {
		t.Fatal("expected error for empty token")
	}
	if _, err := NewTelegramNotifier("tok", 0); err == nil {
		t.Fatal("expected error for zero allowed_chat_id")
	}
}

func TestProposeTradeSendsMessageWithKeyboard(t *testing.T) {
	n, api := newTestNotifier(t, 42)

	p := Proposal{ID: "prop-1", Symbol: "SOL/USDT", Strategy: "trendfollow", RefPrice: 142.3, StopPrice: 128.07,
		QtyDisplay: "12.6 SOL", NotionalQuote: 1793, QuoteAsset: "USDT", RiskAmount: 180, RiskPct: 0.01,
		Reason: "kırılım", PortfolioAfter: PortfolioAfter{OpenPositions: 3, MaxPositions: 5, ExposurePct: 0.62, CashAfter: 6810},
		ExpiresAt: time.Now().Add(4 * time.Hour)}

	if err := n.ProposeTrade(context.Background(), p); err != nil {
		t.Fatalf("ProposeTrade: %v", err)
	}

	calls := api.callsFor("sendMessage")
	if len(calls) != 1 {
		t.Fatalf("sendMessage calls = %d, want 1", len(calls))
	}
	text := calls[0].form["text"]
	if !strings.Contains(text, "SOL/USDT") {
		t.Errorf("sendMessage text missing symbol: %s", text)
	}
	if calls[0].form["chat_id"] != "42" {
		t.Errorf("chat_id = %q, want \"42\"", calls[0].form["chat_id"])
	}
	if calls[0].form["reply_markup"] == "" {
		t.Error("sendMessage missing reply_markup (approve/reject/detay keyboard)")
	}
}

func TestNotifyPrefixesLevelEmoji(t *testing.T) {
	n, api := newTestNotifier(t, 42)
	if err := n.Notify(context.Background(), LevelCritical, "Devre kesici açıldı", "detay"); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	calls := api.callsFor("sendMessage")
	if len(calls) != 1 {
		t.Fatalf("sendMessage calls = %d, want 1", len(calls))
	}
	text := calls[0].form["text"]
	if !strings.Contains(text, "KRİTİK") || !strings.Contains(text, "Devre kesici açıldı") {
		t.Errorf("Notify text = %q, want KRİTİK prefix + title", text)
	}
}

func TestHandleDecisionPublishesApprovalForAllowedChat(t *testing.T) {
	n, _ := newTestNotifier(t, 42)

	update := &models.Update{CallbackQuery: &models.CallbackQuery{
		ID:   "cbq-1",
		Data: EncodeCallback(CallbackApprove, "prop-1"),
		From: models.User{ID: 42},
		Message: models.MaybeInaccessibleMessage{
			Type:    models.MaybeInaccessibleMessageTypeMessage,
			Message: &models.Message{Chat: models.Chat{ID: 42}},
		},
	}}

	n.handleDecision(true)(context.Background(), n.bot, update)

	select {
	case d := <-n.Approvals():
		if d.ProposalID != "prop-1" || !d.Approved {
			t.Errorf("Decision = %+v, want {prop-1 true ...}", d)
		}
	default:
		t.Fatal("expected a Decision on the Approvals channel")
	}
}

func TestHandleDecisionIgnoresDisallowedChat(t *testing.T) {
	n, api := newTestNotifier(t, 42)

	var logged []string
	n.logf = func(format string, args ...any) { logged = append(logged, format) }

	update := &models.Update{CallbackQuery: &models.CallbackQuery{
		ID:   "cbq-2",
		Data: EncodeCallback(CallbackApprove, "prop-1"),
		From: models.User{ID: 999}, // not the allowed chat
		Message: models.MaybeInaccessibleMessage{
			Type:    models.MaybeInaccessibleMessageTypeMessage,
			Message: &models.Message{Chat: models.Chat{ID: 999}},
		},
	}}

	n.handleDecision(true)(context.Background(), n.bot, update)

	select {
	case d := <-n.Approvals():
		t.Fatalf("expected no Decision from a disallowed chat, got %+v", d)
	default:
	}
	if len(logged) == 0 {
		t.Error("expected the disallowed attempt to be logged (SPEC.md Bölüm 6.8: sessizce yok sayılır ve loglanır)")
	}
	// Still acks the callback query so Telegram stops the button spinner,
	// even though the decision itself was dropped.
	if len(api.callsFor("answerCallbackQuery")) != 1 {
		t.Error("expected answerCallbackQuery to be called even for a disallowed chat")
	}
}
