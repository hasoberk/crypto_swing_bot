package risk

import (
	"fmt"
	"time"

	"swingbot/internal/config"
)

// Breaker trip reason codes, one per SPEC.md Bölüm 6.5.3 condition.
const (
	BreakerReasonMaxDrawdown       = "max_drawdown"
	BreakerReasonConsecutiveLosses = "max_consecutive_losses"
	BreakerReasonDailyLoss         = "max_daily_loss"
	BreakerReasonOrderErrors       = "max_order_errors_24h"
)

// defaults mirror SPEC.md Bölüm 8's config.yaml `breaker:` defaults.
const (
	defaultMaxDrawdown          = 0.15
	defaultMaxConsecutiveLosses = 6
	defaultMaxDailyLoss         = 0.05
	defaultMaxOrderErrors24h    = 3
)

// TradeResult is the minimal shape of a closed round-trip trade the
// breaker needs to evaluate the consecutive-loss and daily-loss
// conditions. It deliberately does not reuse a store/domain trade type —
// the caller (engine) adapts whatever it has (store.Trade, a backtest
// fill, ...) into this.
type TradeResult struct {
	ClosedAt time.Time
	// PnL is realized profit/loss in quote currency; negative is a loss.
	PnL float64
}

// OrderError records a single failed order submission/fill, for the 24h
// order-error-rate trip condition.
type OrderError struct {
	At time.Time
}

// State is the breaker's current status. Open, Reason and At are exactly
// what SPEC.md Bölüm 6.5.3 requires be written to
// system_state["breaker"] when the breaker trips ("gerekçe + zaman
// damgası") — Breaker itself performs no I/O, so persisting State is the
// caller's job (e.g. store.SetState("breaker", ...)).
type State struct {
	Open bool
	// Reason is one of the BreakerReason* consts, empty when Open is
	// false.
	Reason string
	// Detail is a human-readable elaboration of Reason (İ6), e.g.
	// "drawdown 16.3% >= limit 15.0%".
	Detail string
	// At is when the breaker tripped (or the zero time if never tripped
	// since the last Reset).
	At time.Time
}

// Breaker implements SPEC.md Bölüm 6.5.3. It watches four independent trip
// conditions — drawdown from peak equity, consecutive losing trades, daily
// loss, and order-error rate — against a rolling history the caller feeds
// it via RecordTrade/RecordOrderError, and evaluates them on every Check
// call.
//
// Once tripped, Breaker stays open until a caller explicitly calls Reset:
// there is no automatic recovery ("kapanış YALNIZCA manuel: `swingbot
// breaker reset --confirm`"). Check is a no-op while already open — it
// never silently re-derives a different reason or, more importantly,
// never closes itself just because the condition that tripped it has since
// resolved.
//
// Breaker has no notion of blocking exits. Only risk.Gate consults
// Breaker.Open() (via GateInput.BreakerOpen), and Gate only ever applies
// its checks to SignalEnter — exit and stop_update signals bypass Gate
// entirely (see gate.go's doc comment). This is the mechanism behind İ7's
// "yeni giriş YOK, çıkışlar ve stop'lar ÇALIŞMAYA DEVAM EDER": the breaker
// protects you from taking on NEW risk, it does not freeze you inside the
// risk you already hold. Do not wire Breaker into any exit/stop code path
// — that would silently invert the guarantee.
type Breaker struct {
	cfg config.BreakerConfig

	state State

	trades     []TradeResult
	errors     []OrderError
	peakEquity float64
}

// NewBreaker constructs a Breaker bound to cfg (the `breaker:` section of
// config.yaml).
func NewBreaker(cfg config.BreakerConfig) *Breaker {
	return &Breaker{cfg: cfg}
}

// State returns the breaker's current status.
func (b *Breaker) State() State {
	return b.state
}

// Open reports whether the breaker currently forbids new entries.
func (b *Breaker) Open() bool {
	return b.state.Open
}

// RecordTrade appends a closed round-trip trade to the breaker's rolling
// history. Call this once per closed trade, in chronological order, before
// the next Check. It does not itself trigger evaluation — call Check
// afterwards.
func (b *Breaker) RecordTrade(tr TradeResult) {
	b.trades = append(b.trades, tr)
}

// RecordOrderError appends a failed order event to the breaker's rolling
// history. Call this once per order failure, before the next Check.
func (b *Breaker) RecordOrderError(oe OrderError) {
	b.errors = append(b.errors, oe)
}

// Check re-evaluates all four SPEC.md Bölüm 6.5.3 trip conditions as of
// now, using equity for the drawdown check and the trade/error history
// recorded so far for the others, and returns the resulting State.
//
// If the breaker is already open, Check returns the existing State
// unchanged — see the Breaker doc comment on why there is no automatic
// re-evaluation or recovery.
//
// Check also always updates the tracked peak equity (even while already
// open, and even when no condition trips), since peak-equity tracking must
// continue uninterrupted for drawdown to mean anything after a manual
// Reset.
func (b *Breaker) Check(equity float64, now time.Time) State {
	if equity > b.peakEquity {
		b.peakEquity = equity
	}
	if b.state.Open {
		return b.state
	}

	maxDrawdown := b.cfg.MaxDrawdown
	if maxDrawdown <= 0 {
		maxDrawdown = defaultMaxDrawdown
	}
	maxConsecutiveLosses := b.cfg.MaxConsecutiveLosses
	if maxConsecutiveLosses <= 0 {
		maxConsecutiveLosses = defaultMaxConsecutiveLosses
	}
	maxDailyLoss := b.cfg.MaxDailyLoss
	if maxDailyLoss <= 0 {
		maxDailyLoss = defaultMaxDailyLoss
	}
	maxOrderErrors := b.cfg.MaxOrderErrors24h
	if maxOrderErrors <= 0 {
		maxOrderErrors = defaultMaxOrderErrors24h
	}

	// 1. Drawdown from peak equity.
	if b.peakEquity > 0 {
		drawdown := (b.peakEquity - equity) / b.peakEquity
		if drawdown >= maxDrawdown {
			b.trip(BreakerReasonMaxDrawdown,
				fmt.Sprintf("equity drawdown from peak is %.2f%%, >= limit %.2f%%", drawdown*100, maxDrawdown*100), now)
			return b.state
		}
	}

	// 2. Consecutive losing trades, most recent first.
	consecutive := 0
	for i := len(b.trades) - 1; i >= 0; i-- {
		if b.trades[i].PnL < 0 {
			consecutive++
		} else {
			break
		}
	}
	if consecutive >= maxConsecutiveLosses {
		b.trip(BreakerReasonConsecutiveLosses,
			fmt.Sprintf("%d consecutive losing trades, >= limit %d", consecutive, maxConsecutiveLosses), now)
		return b.state
	}

	// 3. Daily loss: trades closed on now's UTC calendar day, as a
	// fraction of current equity.
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	dayEnd := dayStart.Add(24 * time.Hour)
	var dailyPnL float64
	for _, tr := range b.trades {
		closed := tr.ClosedAt.UTC()
		if !closed.Before(dayStart) && closed.Before(dayEnd) {
			dailyPnL += tr.PnL
		}
	}
	if dailyPnL < 0 && equity > 0 {
		dailyLossPct := -dailyPnL / equity
		if dailyLossPct >= maxDailyLoss {
			b.trip(BreakerReasonDailyLoss,
				fmt.Sprintf("daily loss is %.2f%% of equity, >= limit %.2f%%", dailyLossPct*100, maxDailyLoss*100), now)
			return b.state
		}
	}

	// 4. Order errors in the trailing 24h window.
	cutoff := now.Add(-24 * time.Hour)
	errCount := 0
	for _, oe := range b.errors {
		if !oe.At.Before(cutoff) && !oe.At.After(now) {
			errCount++
		}
	}
	if errCount >= maxOrderErrors {
		b.trip(BreakerReasonOrderErrors,
			fmt.Sprintf("%d order errors in the trailing 24h, >= limit %d", errCount, maxOrderErrors), now)
		return b.state
	}

	return b.state
}

func (b *Breaker) trip(reason, detail string, now time.Time) {
	b.state = State{Open: true, Reason: reason, Detail: detail, At: now}
}

// Reset manually closes the breaker. Per SPEC.md Bölüm 6.5.3 this must be
// the ONLY way the breaker closes ("kapanış YALNIZCA manuel") — callers
// should invoke it exclusively in direct response to a confirmed operator
// action (`swingbot breaker reset --confirm`), never automatically.
//
// Trade/error history and peak equity are deliberately NOT cleared by
// Reset: if the underlying condition (e.g. equity is still 15% below peak,
// or the last 6 trades are still losers) has not actually resolved, the
// very next Check call will trip the breaker again. Clearing history on
// Reset would let an operator escape drawdown/loss-streak protection by
// resetting repeatedly without the account ever having recovered, which
// defeats the point of having a breaker at all.
func (b *Breaker) Reset() {
	b.state = State{}
}
