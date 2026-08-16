package risk

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"swingbot/internal/config"
	"swingbot/internal/domain"
)

func baseGateInput(now time.Time) GateInput {
	return GateInput{
		Portfolio:   basePortfolio(10000, 10000),
		Market:      baseMarket(),
		Now:         now,
		LastExitAt:  time.Time{},
		BreakerOpen: false,
	}
}

func newGate(cfg config.RiskConfig) *Gate {
	return NewGate(cfg, NewSizer(cfg))
}

func TestGate_ApprovesValidSignal(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	gate := newGate(baseRiskCfg())
	sig := enterSignal("BTC/USDT", 100, 90)

	dec := gate.Evaluate(sig, baseGateInput(now))
	if !dec.Approved {
		t.Fatalf("expected approved, got rejected: reason=%q", dec.Reason)
	}
	if !dec.Size.Accepted {
		t.Errorf("expected Size.Accepted, got %+v", dec.Size)
	}
}

// Exit and stop_update signals must ALWAYS pass through approved,
// regardless of any other rule — including an open breaker and a full
// position book. This is İ7's binding guarantee: the breaker (and the
// rest of the gate) block new entries only, never exits/stops.
func TestGate_ExitAndStopAlwaysApprovedEvenWhenEverythingElseWouldReject(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	gate := newGate(baseRiskCfg())

	in := baseGateInput(now)
	in.BreakerOpen = true
	in.Portfolio = basePortfolio(0, 10000) // no cash at all
	in.Portfolio.Positions = map[string]domain.Position{
		"A/USDT": {}, "B/USDT": {}, "C/USDT": {}, "D/USDT": {}, "E/USDT": {},
	}
	// Already "open" in BTC/USDT too, and within cooldown.
	in.LastExitAt = now.Add(-1 * time.Minute)

	for _, kind := range []domain.SignalKind{domain.SignalExit, domain.SignalStop} {
		sig := domain.Signal{Symbol: "BTC/USDT", Kind: kind, RefPrice: 100}
		dec := gate.Evaluate(sig, in)
		if !dec.Approved {
			t.Errorf("kind=%s: expected approved, got rejected: reason=%q", kind, dec.Reason)
		}
	}
}

func TestGate_RejectsWhenBreakerOpen(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	gate := newGate(baseRiskCfg())
	in := baseGateInput(now)
	in.BreakerOpen = true

	dec := gate.Evaluate(enterSignal("BTC/USDT", 100, 90), in)
	if dec.Approved {
		t.Fatal("expected rejection")
	}
	if dec.Reason != ReasonBreakerOpen {
		t.Errorf("Reason = %q, want %q", dec.Reason, ReasonBreakerOpen)
	}
}

func TestGate_RejectsMaxPositions(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	cfg := baseRiskCfg()
	cfg.MaxPositions = 2
	gate := newGate(cfg)

	in := baseGateInput(now)
	in.Portfolio.Positions = map[string]domain.Position{
		"ETH/USDT": {}, "SOL/USDT": {},
	}

	dec := gate.Evaluate(enterSignal("BTC/USDT", 100, 90), in)
	if dec.Approved {
		t.Fatal("expected rejection")
	}
	if dec.Reason != ReasonMaxPositions {
		t.Errorf("Reason = %q, want %q", dec.Reason, ReasonMaxPositions)
	}
}

func TestGate_AllowsOneBelowMaxPositions(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	cfg := baseRiskCfg()
	cfg.MaxPositions = 2
	gate := newGate(cfg)

	in := baseGateInput(now)
	in.Portfolio.Positions = map[string]domain.Position{"ETH/USDT": {}}

	dec := gate.Evaluate(enterSignal("BTC/USDT", 100, 90), in)
	if !dec.Approved {
		t.Fatalf("expected approved (1 of 2 slots used), got rejected: %q", dec.Reason)
	}
}

func TestGate_RejectsMaxExposure(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	cfg := baseRiskCfg()
	cfg.MaxExposure = 0.80
	gate := newGate(cfg)

	in := baseGateInput(now)
	// Exposure = (Equity - Cash) / Equity = (10000-1000)/10000 = 0.90 >= 0.80
	in.Portfolio = domain.Portfolio{Cash: 1000, Equity: 10000, Positions: map[string]domain.Position{}}

	dec := gate.Evaluate(enterSignal("BTC/USDT", 100, 90), in)
	if dec.Approved {
		t.Fatal("expected rejection")
	}
	if dec.Reason != ReasonMaxExposure {
		t.Errorf("Reason = %q, want %q", dec.Reason, ReasonMaxExposure)
	}
}

func TestGate_RejectsAlreadyOpen(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	gate := newGate(baseRiskCfg())

	in := baseGateInput(now)
	in.Portfolio.Positions = map[string]domain.Position{"BTC/USDT": {Symbol: "BTC/USDT"}}

	dec := gate.Evaluate(enterSignal("BTC/USDT", 100, 90), in)
	if dec.Approved {
		t.Fatal("expected rejection")
	}
	if dec.Reason != ReasonAlreadyOpen {
		t.Errorf("Reason = %q, want %q", dec.Reason, ReasonAlreadyOpen)
	}
}

func TestGate_RejectsCooldown(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	cfg := baseRiskCfg()
	cfg.CooldownHours = 24
	gate := newGate(cfg)

	in := baseGateInput(now)
	in.LastExitAt = now.Add(-1 * time.Hour) // exited 1h ago, well within 24h cooldown

	dec := gate.Evaluate(enterSignal("BTC/USDT", 100, 90), in)
	if dec.Approved {
		t.Fatal("expected rejection")
	}
	if dec.Reason != ReasonCooldown {
		t.Errorf("Reason = %q, want %q", dec.Reason, ReasonCooldown)
	}
}

func TestGate_AllowsAfterCooldownExpires(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	cfg := baseRiskCfg()
	cfg.CooldownHours = 24
	gate := newGate(cfg)

	in := baseGateInput(now)
	in.LastExitAt = now.Add(-25 * time.Hour) // exited 25h ago, past cooldown

	dec := gate.Evaluate(enterSignal("BTC/USDT", 100, 90), in)
	if !dec.Approved {
		t.Fatalf("expected approved (cooldown expired), got rejected: %q", dec.Reason)
	}
}

// At the exact cooldown boundary (Now == LastExitAt + cooldown) the signal
// is allowed; a single millisecond before that boundary it is still
// blocked. This pins down the boundary's inclusivity as a deliberate
// behavior rather than an accident of comparison operators.
func TestGate_CooldownBoundary(t *testing.T) {
	cfg := baseRiskCfg()
	cfg.CooldownHours = 24
	gate := newGate(cfg)

	lastExit := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := lastExit.Add(24 * time.Hour) // exactly at the boundary

	in := baseGateInput(now)
	in.LastExitAt = lastExit

	dec := gate.Evaluate(enterSignal("BTC/USDT", 100, 90), in)
	if !dec.Approved {
		t.Fatalf("expected approved at exact cooldown boundary (Now == LastExitAt+cooldown), got rejected: %q", dec.Reason)
	}

	// One millisecond earlier must still be blocked.
	in.Now = now.Add(-time.Millisecond)
	dec = gate.Evaluate(enterSignal("BTC/USDT", 100, 90), in)
	if dec.Approved {
		t.Fatal("expected rejection one millisecond before the cooldown boundary")
	}
	if dec.Reason != ReasonCooldown {
		t.Errorf("Reason = %q, want %q", dec.Reason, ReasonCooldown)
	}
}

func TestGate_RejectsInsufficientCash(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	gate := newGate(baseRiskCfg())

	in := baseGateInput(now)
	// Cash below MinNotional (10): the smallest possible order does not
	// fit, so this must be rejected before sizing is even attempted.
	in.Portfolio = domain.Portfolio{Cash: 5, Equity: 5, Positions: map[string]domain.Position{}}

	dec := gate.Evaluate(enterSignal("BTC/USDT", 100, 90), in)
	if dec.Approved {
		t.Fatal("expected rejection")
	}
	if dec.Reason != ReasonInsufficientCash {
		t.Errorf("Reason = %q, want %q", dec.Reason, ReasonInsufficientCash)
	}
}

// insufficient_cash must not fire when cash is merely less than the
// risk-sized quantity's notional (that's a trim, not a rejection) — only
// when cash can't even cover MinNotional.
func TestGate_CashBelowRiskSizeButAboveMinNotionalIsNotRejected(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	cfg := baseRiskCfg()
	cfg.RiskPerTrade = 0.05   // risk-sized notional would be 50% of equity...
	cfg.MaxPositionPct = 0.60 // ...comfortably under the position cap...
	gate := newGate(cfg)

	in := baseGateInput(now)
	// ...but cash (3000, exposure 30% -> passes max_exposure at 80%) can
	// only cover 30 of the risk-sized 50 units, so the sizer must trim,
	// not reject.
	in.Portfolio = domain.Portfolio{Cash: 3000, Equity: 10000, Positions: map[string]domain.Position{}}

	dec := gate.Evaluate(enterSignal("BTC/USDT", 100, 90), in)
	if !dec.Approved {
		t.Fatalf("expected approved (trimmed by cash, not rejected), got: reason=%q", dec.Reason)
	}
	if !dec.Size.CappedByCash {
		t.Errorf("expected the sizer to report CappedByCash, got %+v", dec.Size)
	}
}

func TestGate_RejectsBelowMinNotionalViaSizer(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	cfg := baseRiskCfg()
	cfg.RiskPerTrade = 0.001 // tiny risk budget -> tiny quantity
	gate := newGate(cfg)

	in := baseGateInput(now)
	in.Portfolio = domain.Portfolio{Cash: 100, Equity: 100, Positions: map[string]domain.Position{}}
	in.Market.MinNotional = decimal.NewFromFloat(50) // Cash(100) >= MinNotional(50): passes the cash gate check

	dec := gate.Evaluate(enterSignal("BTC/USDT", 100, 99), in) // stop distance 1
	if dec.Approved {
		t.Fatal("expected rejection")
	}
	if dec.Reason != ReasonBelowMinNotional {
		t.Errorf("Reason = %q, want %q", dec.Reason, ReasonBelowMinNotional)
	}
}

func TestGate_RejectsMarketMismatch(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	gate := newGate(baseRiskCfg())

	in := baseGateInput(now)
	in.Market.Symbol = "ETH/USDT" // signal is for BTC/USDT

	dec := gate.Evaluate(enterSignal("BTC/USDT", 100, 90), in)
	if dec.Approved {
		t.Fatal("expected rejection")
	}
	if dec.Reason != ReasonMarketMismatch {
		t.Errorf("Reason = %q, want %q", dec.Reason, ReasonMarketMismatch)
	}
}

// max_positions=0 / max_exposure=0 / cooldown_hours=0 in config must fall
// back to SPEC.md Bölüm 6.5.2/6.5.1's stated defaults, matching the
// zero-means-unconfigured convention used everywhere else in this package.
func TestGate_DefaultsAppliedWhenConfigZero(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	gate := newGate(config.RiskConfig{}) // everything zero

	in := baseGateInput(now)
	in.Portfolio.Positions = map[string]domain.Position{
		"A/USDT": {}, "B/USDT": {}, "C/USDT": {}, "D/USDT": {}, "E/USDT": {},
	} // 5 open positions == default max_positions

	dec := gate.Evaluate(enterSignal("BTC/USDT", 100, 90), in)
	if dec.Approved {
		t.Fatal("expected rejection at default max_positions=5")
	}
	if dec.Reason != ReasonMaxPositions {
		t.Errorf("Reason = %q, want %q", dec.Reason, ReasonMaxPositions)
	}
}

// Every rejection decision must carry the signal it was evaluated for, so
// callers can log "why did we not trade this" per SPEC.md Bölüm 6.5.2's
// binding rule that rejected signals are recorded, not merely dropped.
func TestGate_DecisionAlwaysCarriesOriginalSignal(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	gate := newGate(baseRiskCfg())
	in := baseGateInput(now)
	in.BreakerOpen = true

	sig := enterSignal("BTC/USDT", 100, 90)
	dec := gate.Evaluate(sig, in)
	if dec.Signal.Symbol != sig.Symbol || dec.Signal.RefPrice != sig.RefPrice {
		t.Errorf("Decision.Signal = %+v, want it to equal the input signal %+v", dec.Signal, sig)
	}
}
