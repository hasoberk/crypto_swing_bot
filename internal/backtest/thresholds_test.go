package backtest

import (
	"testing"

	"swingbot/internal/broker"
)

func TestDefaultThresholds(t *testing.T) {
	th := DefaultThresholds()
	if th.MinTradeCount != 50 {
		t.Errorf("MinTradeCount = %d, want 50 (SPEC.md Bölüm 11.4)", th.MinTradeCount)
	}
	if !th.RequireBeatBenchmarkInAnyRegime || !th.RequireLowerMaxDrawdownThanBenchmark ||
		!th.RequirePositiveAt2xCosts || !th.RequireParamPlateau {
		t.Errorf("DefaultThresholds() should require all four SPEC.md Bölüm 11.4 pass/fail criteria: %+v", th)
	}
}

// curve builds a two-point EquityPoint slice from startEquity to
// endEquity over n days starting at day(0) — enough for
// Compute/MaxDrawdown/TotalReturn to have a well-defined answer in tests
// below.
func curve(startEquity, endEquity float64, n int) []EquityPoint {
	out := make([]EquityPoint, n)
	for i := 0; i < n; i++ {
		frac := float64(i) / float64(n-1)
		out[i] = EquityPoint{Date: day(i), Equity: startEquity + (endEquity-startEquity)*frac}
	}
	return out
}

func tradesN(n int) []broker.ClosedTrade {
	out := make([]broker.ClosedTrade, n)
	for i := range out {
		out[i] = broker.ClosedTrade{Symbol: "BTC/USDT", EntryTime: day(i), ExitTime: day(i + 1), PnLQuote: 1}
	}
	return out
}

// newPassingWF builds a WalkForwardResult meant to satisfy every SPEC.md
// Bölüm 11.4 criterion at once (a baseline every subtest below mutates one
// piece of). Metrics is always Compute(CombinedEquity, CombinedTrades) —
// EvaluateThresholds reads wf.Metrics directly, exactly like the real
// caller (RunWalkForward) sets it, rather than recomputing it itself.
func newPassingWF() *WalkForwardResult {
	// A small dip right after "entry" (fees) then a strong climb — deeper
	// than BTC's own dip, so criterion 3 (max drawdown) has a real,
	// non-degenerate answer on both sides.
	stratEquity := []EquityPoint{
		{Date: day(0), Equity: 10000},
		{Date: day(1), Equity: 9900}, // -1%
		{Date: day(2), Equity: 15000},
	}
	btcEquity := []EquityPoint{
		{Date: day(0), Equity: 10000},
		{Date: day(1), Equity: 8000}, // -20%
		{Date: day(2), Equity: 11000},
	}
	trades := tradesN(60)
	return &WalkForwardResult{
		Windows: []WindowResult{{
			Window:       Window{TestStart: day(0), TestEnd: day(90)},
			TestEquity:   curve(10000, 15000, 90), // strategy: +50%
			TestBenchBTC: curve(10000, 11000, 90), // BTC: +10% — strategy wins this window
			Regime:       RegimeBull,
		}},
		CombinedEquity:   stratEquity,
		CombinedBenchBTC: btcEquity,
		CombinedTrades:   trades,
		Metrics:          Compute(stratEquity, trades),
	}
}

func TestEvaluateThresholds(t *testing.T) {
	passingPlateaus := []PlateauVerdict{{Param: "atr_stop_mult", IsPlateau: true}}
	passingCosts2x := Metrics{TotalReturn: 0.05}

	t.Run("all criteria pass", func(t *testing.T) {
		v := EvaluateThresholds(DefaultThresholds(), newPassingWF(), passingPlateaus, passingCosts2x)
		if !v.Passed {
			for _, c := range v.Criteria {
				t.Logf("%s: passed=%v detail=%s", c.Name, c.Passed, c.Detail)
			}
			t.Error("expected Verdict.Passed = true")
		}
	})

	t.Run("too few trades fails only that criterion", func(t *testing.T) {
		wf := newPassingWF()
		wf.CombinedTrades = tradesN(10)
		wf.Metrics = Compute(wf.CombinedEquity, wf.CombinedTrades)
		v := EvaluateThresholds(DefaultThresholds(), wf, passingPlateaus, passingCosts2x)
		if v.Passed {
			t.Error("expected Verdict.Passed = false with only 10 trades (< 50)")
		}
		if v.Criteria[0].Passed {
			t.Error("İşlem sayısı criterion should have failed")
		}
	})

	t.Run("negative return at 2x costs fails", func(t *testing.T) {
		v := EvaluateThresholds(DefaultThresholds(), newPassingWF(), passingPlateaus, Metrics{TotalReturn: -0.02})
		if v.Passed {
			t.Error("expected Verdict.Passed = false when 2x-cost TotalReturn is negative")
		}
	})

	t.Run("sharp peak among swept params fails plateau criterion", func(t *testing.T) {
		plateaus := []PlateauVerdict{
			{Param: "atr_stop_mult", IsPlateau: true},
			{Param: "sma_long", IsPlateau: false},
		}
		v := EvaluateThresholds(DefaultThresholds(), newPassingWF(), plateaus, passingCosts2x)
		if v.Passed {
			t.Error("expected Verdict.Passed = false when any swept parameter shows a sharp peak")
		}
	})

	t.Run("never beating BTC in any regime fails", func(t *testing.T) {
		wf := newPassingWF()
		wf.Windows = []WindowResult{{
			Window:       Window{TestStart: day(0), TestEnd: day(90)},
			TestEquity:   curve(10000, 10500, 90), // strategy: +5%
			TestBenchBTC: curve(10000, 12000, 90), // BTC: +20% — strategy loses
			Regime:       RegimeBull,
		}}
		v := EvaluateThresholds(DefaultThresholds(), wf, passingPlateaus, passingCosts2x)
		if v.Passed {
			t.Error("expected Verdict.Passed = false when the strategy never beats BTC in any regime")
		}
	})

	t.Run("higher max drawdown than benchmark fails", func(t *testing.T) {
		wf := newPassingWF()
		// Strategy draws down hard mid-curve, BTC does not.
		steepDD := []EquityPoint{
			{Date: day(0), Equity: 10000},
			{Date: day(1), Equity: 5000}, // -50%
			{Date: day(2), Equity: 9000},
		}
		flatBTC := []EquityPoint{
			{Date: day(0), Equity: 10000},
			{Date: day(1), Equity: 9800}, // -2%
			{Date: day(2), Equity: 10500},
		}
		wf.CombinedEquity = steepDD
		wf.CombinedBenchBTC = flatBTC
		wf.Metrics = Compute(steepDD, wf.CombinedTrades)
		v := EvaluateThresholds(DefaultThresholds(), wf, passingPlateaus, passingCosts2x)
		if v.Passed {
			t.Error("expected Verdict.Passed = false when strategy drawdown exceeds BTC's")
		}
	})

	t.Run("disabled criteria are always passed regardless of data", func(t *testing.T) {
		th := Thresholds{MinTradeCount: 50} // every Require* left false
		wf := newPassingWF()                // already has >= 50 trades
		v := EvaluateThresholds(th, wf, nil, Metrics{TotalReturn: -1})
		if !v.Passed {
			for _, c := range v.Criteria {
				t.Logf("%s: passed=%v detail=%s", c.Name, c.Passed, c.Detail)
			}
			t.Error("expected Verdict.Passed = true when every Require* is false and only trade count is checked")
		}
	})
}

func TestEvaluateThresholds_ComputeIsUsed(t *testing.T) {
	// Sanity: a window with zero trades and a flat curve still yields a
	// well-formed Metrics so EvaluateThresholds never panics on it.
	equity := []EquityPoint{{Date: day(0), Equity: 10000}, {Date: day(1), Equity: 10000}}
	wf := &WalkForwardResult{
		Windows: []WindowResult{{
			Window: Window{TestStart: day(0), TestEnd: day(1)},
		}},
		CombinedEquity:   equity,
		CombinedBenchBTC: equity,
		Metrics:          Compute(equity, nil),
	}
	v := EvaluateThresholds(DefaultThresholds(), wf, nil, Metrics{})
	if v.Passed {
		t.Error("expected Verdict.Passed = false with zero trades and no plateau data")
	}
}
