package risk

import (
	"testing"
	"time"

	"swingbot/internal/config"
)

func baseBreakerCfg() config.BreakerConfig {
	return config.BreakerConfig{
		MaxDrawdown:          0.15,
		MaxDailyLoss:         0.05,
		MaxConsecutiveLosses: 6,
		MaxOrderErrors24h:    3,
	}
}

func TestBreaker_TripsOnDrawdown(t *testing.T) {
	b := NewBreaker(baseBreakerCfg())
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	if st := b.Check(10000, t0); st.Open {
		t.Fatalf("did not expect trip at peak equity: %+v", st)
	}

	// drawdown = (10000-8400)/10000 = 16% >= 15%
	st := b.Check(8400, t0.Add(24*time.Hour))
	if !st.Open {
		t.Fatal("expected breaker to trip on drawdown")
	}
	if st.Reason != BreakerReasonMaxDrawdown {
		t.Errorf("Reason = %q, want %q", st.Reason, BreakerReasonMaxDrawdown)
	}
	if st.At.IsZero() {
		t.Error("expected a non-zero trip timestamp")
	}
	if !b.Open() {
		t.Error("Open() should report true once tripped")
	}
}

// Drawdown exactly at the threshold must trip (>=, not >).
func TestBreaker_DrawdownExactlyAtThresholdTrips(t *testing.T) {
	b := NewBreaker(baseBreakerCfg())
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b.Check(10000, t0)

	// exactly 15% drawdown
	st := b.Check(8500, t0.Add(time.Hour))
	if !st.Open {
		t.Fatal("expected trip at exactly the drawdown threshold")
	}
}

// A drawdown just below the threshold must not trip.
func TestBreaker_DrawdownJustBelowThresholdDoesNotTrip(t *testing.T) {
	b := NewBreaker(baseBreakerCfg())
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b.Check(10000, t0)

	st := b.Check(8501, t0.Add(time.Hour)) // 14.99% drawdown
	if st.Open {
		t.Fatal("did not expect a trip below the drawdown threshold")
	}
}

func TestBreaker_TripsOnConsecutiveLosses(t *testing.T) {
	cfg := baseBreakerCfg()
	cfg.MaxConsecutiveLosses = 3
	b := NewBreaker(cfg)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for i := 0; i < 3; i++ {
		b.RecordTrade(TradeResult{ClosedAt: now.Add(time.Duration(i) * time.Hour), PnL: -10})
	}

	st := b.Check(10000, now.Add(4*time.Hour))
	if !st.Open {
		t.Fatal("expected trip on consecutive losses")
	}
	if st.Reason != BreakerReasonConsecutiveLosses {
		t.Errorf("Reason = %q, want %q", st.Reason, BreakerReasonConsecutiveLosses)
	}
}

// A winning trade resets the consecutive-loss streak, even if there were
// losses both before and after it.
func TestBreaker_WinningTradeBreaksConsecutiveLossStreak(t *testing.T) {
	cfg := baseBreakerCfg()
	cfg.MaxConsecutiveLosses = 3
	b := NewBreaker(cfg)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	trades := []float64{-10, -10, -10, 5, -10, -10} // last streak is only 2 losses
	for i, pnl := range trades {
		b.RecordTrade(TradeResult{ClosedAt: now.Add(time.Duration(i) * time.Hour), PnL: pnl})
	}

	st := b.Check(10000, now.Add(10*time.Hour))
	if st.Open {
		t.Fatalf("did not expect trip: only 2 consecutive losses since the last win, got %+v", st)
	}
}

func TestBreaker_TripsOnDailyLoss(t *testing.T) {
	cfg := baseBreakerCfg()
	b := NewBreaker(cfg)
	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Establish peak equity first so this test isolates the daily-loss
	// condition from the drawdown one.
	b.Check(10000, day)

	b.RecordTrade(TradeResult{ClosedAt: day.Add(3 * time.Hour), PnL: -600})

	// equity after the loss: 9400. daily loss = 600/9400 = 6.38% >= 5%.
	// drawdown from peak 10000 -> 9400 is only 6%, below the 15% drawdown
	// threshold, so drawdown does not confound this check.
	st := b.Check(9400, day.Add(4*time.Hour))
	if !st.Open {
		t.Fatal("expected trip on daily loss")
	}
	if st.Reason != BreakerReasonDailyLoss {
		t.Errorf("Reason = %q, want %q", st.Reason, BreakerReasonDailyLoss)
	}
}

// A loss booked on a PREVIOUS UTC calendar day must not count toward
// today's daily-loss check.
func TestBreaker_DailyLossOnlyCountsCurrentUTCDay(t *testing.T) {
	b := NewBreaker(baseBreakerCfg())
	day1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	b.Check(10000, day1)
	b.RecordTrade(TradeResult{ClosedAt: day1.Add(23 * time.Hour), PnL: -600}) // yesterday's loss

	// today (day2) has no realized loss yet.
	st := b.Check(9400, day2.Add(time.Hour))
	if st.Open {
		t.Fatalf("did not expect trip: the -600 loss happened on the prior UTC day, got %+v", st)
	}
}

func TestBreaker_TripsOnOrderErrors24h(t *testing.T) {
	cfg := baseBreakerCfg()
	cfg.MaxOrderErrors24h = 3
	b := NewBreaker(cfg)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	b.RecordOrderError(OrderError{At: now.Add(-3 * time.Hour)})
	b.RecordOrderError(OrderError{At: now.Add(-2 * time.Hour)})
	b.RecordOrderError(OrderError{At: now.Add(-1 * time.Hour)})

	st := b.Check(10000, now)
	if !st.Open {
		t.Fatal("expected trip on order-error rate")
	}
	if st.Reason != BreakerReasonOrderErrors {
		t.Errorf("Reason = %q, want %q", st.Reason, BreakerReasonOrderErrors)
	}
}

// Order errors older than 24h must not count toward the trailing-window
// trip condition.
func TestBreaker_OrderErrorsOutsideWindowDontCount(t *testing.T) {
	cfg := baseBreakerCfg()
	cfg.MaxOrderErrors24h = 3
	b := NewBreaker(cfg)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	b.RecordOrderError(OrderError{At: now.Add(-25 * time.Hour)})
	b.RecordOrderError(OrderError{At: now.Add(-26 * time.Hour)})
	b.RecordOrderError(OrderError{At: now.Add(-27 * time.Hour)})

	st := b.Check(10000, now)
	if st.Open {
		t.Fatalf("did not expect trip: all 3 order errors are outside the trailing 24h window, got %+v", st)
	}
}

// Once tripped, Check must be a no-op: it must not silently re-evaluate to
// a different reason, and it must NOT close itself even if the condition
// that tripped it (e.g. drawdown) has since fully recovered. Only Reset
// closes the breaker.
func TestBreaker_StaysOpenUntilManualReset(t *testing.T) {
	b := NewBreaker(baseBreakerCfg())
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b.Check(10000, t0)

	st := b.Check(8000, t0.Add(time.Hour)) // trips: 20% drawdown
	if !st.Open {
		t.Fatal("expected trip")
	}
	trippedAt := st.At

	// Equity fully recovers above the old peak — still must stay open.
	st = b.Check(20000, t0.Add(2*time.Hour))
	if !st.Open {
		t.Fatal("breaker must stay open until Reset, even after equity recovers")
	}
	if !st.At.Equal(trippedAt) {
		t.Errorf("trip timestamp changed on a no-op Check: got %v, want %v", st.At, trippedAt)
	}

	b.Reset()
	if b.Open() {
		t.Fatal("expected breaker closed after Reset")
	}
}

// Reset does not erase trade/error history or peak equity: if the
// underlying condition has not actually resolved, the very next Check
// re-trips. This is deliberate (see Breaker.Reset's doc comment) — an
// operator cannot escape drawdown/loss-streak protection by resetting
// without the account actually recovering.
func TestBreaker_ResetDoesNotClearHistorySoConditionCanReTrip(t *testing.T) {
	b := NewBreaker(baseBreakerCfg())
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b.Check(10000, t0)
	b.Check(8000, t0.Add(time.Hour)) // trips on drawdown, peak stays 10000

	b.Reset()
	if b.Open() {
		t.Fatal("expected closed right after Reset")
	}

	// Equity still 8000 (20% below the untouched peak of 10000) -> re-trips.
	st := b.Check(8000, t0.Add(2*time.Hour))
	if !st.Open {
		t.Fatal("expected the breaker to re-trip: the drawdown condition never actually resolved")
	}
	if st.Reason != BreakerReasonMaxDrawdown {
		t.Errorf("Reason = %q, want %q", st.Reason, BreakerReasonMaxDrawdown)
	}
}

// Peak equity keeps being tracked while the breaker is open (per Check's
// doc comment), so that once the operator resets, drawdown is measured
// against the true historical peak rather than a stale one.
func TestBreaker_PeakEquityUpdatesEvenWhileOpen(t *testing.T) {
	b := NewBreaker(baseBreakerCfg())
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	b.Check(10000, t0)
	b.Check(8000, t0.Add(time.Hour)) // trips, peak = 10000
	if !b.Open() {
		t.Fatal("expected tripped")
	}

	// A new high while still open (e.g. broad market recovers before
	// operator notices/resets) must still raise the tracked peak.
	b.Check(12000, t0.Add(2*time.Hour))

	b.Reset()

	// 12.5% drawdown from the new peak of 12000 -> below the 15% limit.
	st := b.Check(10500, t0.Add(3*time.Hour))
	if st.Open {
		t.Fatalf("expected no trip: drawdown from the updated peak (12000) is only 12.5%%, got %+v", st)
	}
}

// breaker config left entirely zero-valued must fall back to SPEC.md
// Bölüm 8's stated defaults (0.15 / 6 / 0.05 / 3), not to "trips on
// anything" (0 thresholds) or "never trips" (unbounded thresholds).
func TestBreaker_DefaultsAppliedWhenConfigZero(t *testing.T) {
	b := NewBreaker(config.BreakerConfig{})
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b.Check(10000, t0)

	// 10% drawdown: below the default 15% threshold -> must not trip.
	st := b.Check(9000, t0.Add(time.Hour))
	if st.Open {
		t.Fatalf("did not expect trip below the default 15%% drawdown threshold, got %+v", st)
	}

	// 16% drawdown: above the default 15% threshold -> must trip.
	st = b.Check(8400, t0.Add(2*time.Hour))
	if !st.Open {
		t.Fatal("expected trip above the default 15% drawdown threshold")
	}
}

// State() must reflect exactly what Check most recently returned.
func TestBreaker_StateMatchesLastCheck(t *testing.T) {
	b := NewBreaker(baseBreakerCfg())
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	if b.State().Open {
		t.Fatal("fresh breaker must start closed")
	}
	b.Check(10000, t0)
	b.Check(8000, t0.Add(time.Hour))
	if !b.State().Open {
		t.Fatal("State() should reflect the trip")
	}
}
