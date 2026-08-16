package risk

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"swingbot/internal/config"
	"swingbot/internal/domain"
)

func baseRiskCfg() config.RiskConfig {
	return config.RiskConfig{
		RiskPerTrade:   0.01,
		MaxPositions:   5,
		MaxExposure:    0.80,
		MaxPositionPct: 0.25,
		CooldownHours:  24,
	}
}

func baseMarket() domain.Market {
	return domain.Market{
		Symbol:      "BTC/USDT",
		Base:        "BTC",
		Quote:       "USDT",
		Active:      true,
		TickSize:    decimal.NewFromFloat(0.01),
		StepSize:    decimal.NewFromFloat(0.001),
		MinNotional: decimal.NewFromFloat(10),
	}
}

func basePortfolio(cash, equity float64) domain.Portfolio {
	return domain.Portfolio{
		Cash:      cash,
		Equity:    equity,
		Positions: map[string]domain.Position{},
	}
}

func enterSignal(symbol string, refPrice, stopPrice float64) domain.Signal {
	return domain.Signal{
		AsOf:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Symbol:    symbol,
		Kind:      domain.SignalEnter,
		RefPrice:  refPrice,
		StopPrice: stopPrice,
		Reason:    "test signal",
	}
}

// Basic computation matches SPEC.md Bölüm 6.5.1 exactly:
// risk_tutari = equity * risk_per_trade; stop_mesafesi = entry - stop;
// ham_qty = risk_tutari / stop_mesafesi.
func TestSize_BasicComputation(t *testing.T) {
	sizer := NewSizer(baseRiskCfg())
	sig := enterSignal("BTC/USDT", 100, 90) // stop distance = 10
	pf := basePortfolio(5000, 10000)        // riskAmount = 10000*0.01 = 100

	got := sizer.Size(sig, pf, baseMarket())

	if !got.Accepted {
		t.Fatalf("expected accepted, got rejected: %+v", got)
	}
	wantQty := decimal.NewFromFloat(10) // 100/10 = 10, exact multiple of step 0.001
	if !got.Qty.Equal(wantQty) {
		t.Errorf("Qty = %s, want %s", got.Qty, wantQty)
	}
	if got.RiskAmount != 100 {
		t.Errorf("RiskAmount = %v, want 100", got.RiskAmount)
	}
	if got.Notional != 1000 {
		t.Errorf("Notional = %v, want 1000", got.Notional)
	}
	if got.CappedByCash || got.CappedByMaxPositionPct {
		t.Errorf("expected no caps applied, got %+v", got)
	}
}

// qty must be rounded DOWN to StepSize, never up or to nearest.
func TestSize_RoundsDownToStepSize(t *testing.T) {
	sizer := NewSizer(baseRiskCfg())
	sig := enterSignal("BTC/USDT", 100, 90) // stop distance 10, rawQty = 10
	pf := basePortfolio(100000, 10000)

	market := baseMarket()
	market.StepSize = decimal.NewFromFloat(0.3)
	market.MinNotional = decimal.NewFromFloat(1)

	got := sizer.Size(sig, pf, market)
	if !got.Accepted {
		t.Fatalf("expected accepted, got: %+v", got)
	}
	// floor(10 / 0.3) * 0.3 = 33 * 0.3 = 9.9
	want := decimal.NewFromFloat(9.9)
	if !got.Qty.Equal(want) {
		t.Errorf("Qty = %s, want %s", got.Qty, want)
	}
}

// A zero StepSize (unset market data) must not divide by zero; it should
// simply skip rounding.
func TestSize_ZeroStepSizeSkipsRounding(t *testing.T) {
	sizer := NewSizer(baseRiskCfg())
	// entry=30, stop=27 (distance 3): the risk-sized position is 10% of
	// equity (well under the default 25% max_position_pct cap), so
	// neither the position-pct cap nor the cash cap binds here — this
	// test isolates StepSize rounding behavior alone.
	sig := enterSignal("BTC/USDT", 30, 27)
	pf := basePortfolio(1000000, 10000) // riskAmount = 100

	market := baseMarket()
	market.StepSize = decimal.Decimal{} // zero value
	market.MinNotional = decimal.Decimal{}

	got := sizer.Size(sig, pf, market)
	if !got.Accepted {
		t.Fatalf("expected accepted, got: %+v", got)
	}
	if got.CappedByCash || got.CappedByMaxPositionPct {
		t.Fatalf("test setup issue: expected no caps to bind, got %+v", got)
	}
	want := decimal.NewFromFloat(100.0 / 3.0)
	if !got.Qty.Equal(want) {
		t.Errorf("Qty = %s, want %s (unrounded)", got.Qty, want)
	}
}

// entry <= stop (non-positive stop distance) must be rejected with
// invalid_stop_distance, never silently produce a negative/inf quantity.
func TestSize_InvalidStopDistance(t *testing.T) {
	sizer := NewSizer(baseRiskCfg())
	pf := basePortfolio(10000, 10000)

	cases := []struct {
		name  string
		entry float64
		stop  float64
	}{
		{"stop equals entry", 100, 100},
		{"stop above entry", 100, 110},
		{"zero entry", 0, -5},
		{"negative entry", -10, -20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sig := enterSignal("BTC/USDT", tc.entry, tc.stop)
			got := sizer.Size(sig, pf, baseMarket())
			if got.Accepted {
				t.Fatalf("expected rejection, got accepted: %+v", got)
			}
			if got.Reason != ReasonInvalidStopDistance {
				t.Errorf("Reason = %q, want %q", got.Reason, ReasonInvalidStopDistance)
			}
		})
	}
}

// qty*entry > cash must trim (cap) the quantity to what cash affords,
// rather than rejecting the signal outright.
func TestSize_CappedByCash(t *testing.T) {
	sizer := NewSizer(baseRiskCfg())
	sig := enterSignal("BTC/USDT", 100, 90) // rawQty would be 10 (equity 10000, risk 1%)
	pf := basePortfolio(500, 10000)         // only 500 cash -> max 5 units at price 100

	got := sizer.Size(sig, pf, baseMarket())
	if !got.Accepted {
		t.Fatalf("expected accepted (trimmed), got rejected: %+v", got)
	}
	if !got.CappedByCash {
		t.Errorf("expected CappedByCash=true, got %+v", got)
	}
	want := decimal.NewFromFloat(5)
	if !got.Qty.Equal(want) {
		t.Errorf("Qty = %s, want %s", got.Qty, want)
	}
	if got.Notional > 500 {
		t.Errorf("Notional %v exceeds available cash 500", got.Notional)
	}
}

// A single position must never exceed max_position_pct of equity, even
// when the risk-based raw quantity would be larger and cash is plentiful.
func TestSize_CappedByMaxPositionPct(t *testing.T) {
	cfg := baseRiskCfg()
	cfg.MaxPositionPct = 0.01 // 1% of equity per position
	sizer := NewSizer(cfg)

	sig := enterSignal("BTC/USDT", 100, 90) // rawQty = 10 (equity 10000, risk 1%)
	pf := basePortfolio(1000000, 10000)     // cash is not the binding constraint

	got := sizer.Size(sig, pf, baseMarket())
	if !got.Accepted {
		t.Fatalf("expected accepted (trimmed), got rejected: %+v", got)
	}
	if !got.CappedByMaxPositionPct {
		t.Errorf("expected CappedByMaxPositionPct=true, got %+v", got)
	}
	if got.CappedByCash {
		t.Errorf("did not expect CappedByCash, got %+v", got)
	}
	// 1% of 10000 equity = 100 notional cap -> 1 unit at price 100.
	want := decimal.NewFromFloat(1)
	if !got.Qty.Equal(want) {
		t.Errorf("Qty = %s, want %s", got.Qty, want)
	}
}

// A quantity that survives rounding/caps but whose notional still falls
// below the exchange's MinNotional must be dropped with below_min_notional.
func TestSize_BelowMinNotional(t *testing.T) {
	sizer := NewSizer(baseRiskCfg())
	sig := enterSignal("BTC/USDT", 100, 90) // rawQty = 1 with equity 1000
	pf := basePortfolio(100000, 1000)

	market := baseMarket()
	market.MinNotional = decimal.NewFromFloat(5000) // far above the ~100 notional this trade sizes to

	got := sizer.Size(sig, pf, market)
	if got.Accepted {
		t.Fatalf("expected rejection, got accepted: %+v", got)
	}
	if got.Reason != ReasonBelowMinNotional {
		t.Errorf("Reason = %q, want %q", got.Reason, ReasonBelowMinNotional)
	}
}

// If StepSize rounding (or a cap) drives the quantity to exactly zero, the
// signal must be dropped with zero_qty rather than silently placing a
// zero-size order.
func TestSize_ZeroQtyAfterRounding(t *testing.T) {
	sizer := NewSizer(baseRiskCfg())
	sig := enterSignal("BTC/USDT", 100, 98) // distance 2, riskAmount = 1 (equity 100) -> rawQty 0.5
	pf := basePortfolio(100000, 100)

	market := baseMarket()
	market.StepSize = decimal.NewFromFloat(1) // floor(0.5/1)*1 = 0
	market.MinNotional = decimal.NewFromFloat(0)

	got := sizer.Size(sig, pf, market)
	if got.Accepted {
		t.Fatalf("expected rejection, got accepted: %+v", got)
	}
	if got.Reason != ReasonZeroQty {
		t.Errorf("Reason = %q, want %q", got.Reason, ReasonZeroQty)
	}
	if got.CappedByCash || got.CappedByMaxPositionPct {
		t.Errorf("zero qty here is from step rounding, not a cap: %+v", got)
	}
}

// risk_per_trade=0 and max_position_pct=0 in config must fall back to
// SPEC.md Bölüm 6.5.1's stated defaults (0.01 and 0.25), not to a
// zero-sized (always-rejected) sizer.
func TestSize_DefaultsAppliedWhenConfigZero(t *testing.T) {
	sizer := NewSizer(config.RiskConfig{}) // everything zero-valued
	sig := enterSignal("BTC/USDT", 100, 90)
	pf := basePortfolio(100000, 10000)

	got := sizer.Size(sig, pf, baseMarket())
	if !got.Accepted {
		t.Fatalf("expected accepted using defaults, got: %+v", got)
	}
	// default risk_per_trade 0.01 * equity 10000 = 100; / stop distance 10 = 10.
	want := decimal.NewFromFloat(10)
	if !got.Qty.Equal(want) {
		t.Errorf("Qty = %s, want %s (default risk_per_trade not applied)", got.Qty, want)
	}
}
