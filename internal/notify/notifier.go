// Package notify implements SPEC.md Bölüm 5.6 (the Notifier interface) and
// Bölüm 6.8 (Telegram + onay durum makinesi). internal/engine is the only
// caller: it proposes trades, waits for a Decision on Approvals(), and
// reports every other operationally important event (stop tetiklenmesi,
// devre kesici, veri kalitesi hatası, emir hatası, günlük özet) through
// Notify.
//
// Binding rule (SPEC.md Bölüm 1.2 İ6): every Proposal this package renders
// carries the full human-readable reason the caller (risk/strategy) already
// computed — this package only formats it, it never invents or summarizes
// away a decision's rationale.
package notify

import (
	"context"
	"time"
)

// Level is the severity of a Notify call. SPEC.md Bölüm 6.8's "bildirilecek
// diğer olaylar" (stop tetiklenmesi, devre kesici, veri kalitesi hatası,
// emir hatası, günlük özet) span the full range: LevelCritical is reserved
// for events that stop the bot from taking new risk (breaker trip, data
// verify failure) — İ7 requires the breaker's trip notification specifically
// to never be silently downgraded or dropped.
type Level string

const (
	LevelInfo     Level = "info"
	LevelWarning  Level = "warning"
	LevelCritical Level = "critical"
)

// Notifier is SPEC.md Bölüm 5.6, unchanged. TelegramNotifier (telegram.go)
// is the only implementation; internal/engine depends on this interface,
// never on the concrete type, so a future non-Telegram transport is a
// drop-in replacement.
type Notifier interface {
	ProposeTrade(ctx context.Context, p Proposal) error
	Notify(ctx context.Context, level Level, title, body string) error
	// Approvals publishes approve/reject decisions as they arrive. The
	// channel is never closed while the Notifier is running — a caller
	// selects on it alongside a deadline timer (SPEC.md Bölüm 6.7 adım 12).
	Approvals() <-chan Decision
}

// Decision is SPEC.md Bölüm 5.6, unchanged.
type Decision struct {
	ProposalID string
	Approved   bool
	At         time.Time
}

// PortfolioAfter is the "portföy sonrası" block of SPEC.md Bölüm 6.8's
// proposal template: what the book would look like if this proposal is
// approved and fills. It is computed by the caller (internal/engine, which
// alone knows risk.Cfg's max_positions) from the portfolio snapshot the
// proposal was sized against — Proposal carries the rendered numbers, not a
// domain.Portfolio, so this package never needs to import internal/risk or
// internal/broker.
type PortfolioAfter struct {
	OpenPositions int // count AFTER this entry, i.e. currently open + 1
	MaxPositions  int
	ExposurePct   float64 // 0..1, AFTER this entry's notional is added
	CashAfter     float64
}

// Proposal is the notify-layer view of a store.Proposal: everything SPEC.md
// Bölüm 6.8's message template needs, already computed by the caller
// (internal/engine) so this package stays free of domain/risk/store
// imports and is trivial to unit test without a database.
type Proposal struct {
	ID       string
	AsOf     time.Time
	Symbol   string
	Strategy string
	// Side is "long" (entry) or "exit" — mirrors store.Proposal.Side.
	// ProposeTrade is only ever called for "long" (entries are the only
	// signals that wait on an approval, per risk.Gate's doc comment —
	// exits/stops are submitted immediately, SPEC.md Bölüm 6.7 adım 8, and
	// reported via Notify instead); Side is still carried here for the
	// message renderer and for a future exit-confirmation flow.
	Side string

	RefPrice      float64
	RefPriceAsOf  time.Time // the candle the ref price is the close of
	StopPrice     float64
	QtyDisplay    string // human-readable quantity, e.g. "12.6 SOL"
	NotionalQuote float64
	QuoteAsset    string // e.g. "USDT"

	RiskAmount float64
	RiskPct    float64 // riskAmount / equity, 0..1

	Reason string // İ6: human-readable rationale

	PortfolioAfter PortfolioAfter

	ExpiresAt time.Time
}
