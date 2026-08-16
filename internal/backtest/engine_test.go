package backtest

import (
	"context"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"swingbot/internal/broker"
	"swingbot/internal/domain"
	"swingbot/internal/strategy"
)

func day(n int) time.Time {
	return time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, n)
}

// btcSeries returns n deterministic daily candles with a gentle upward
// drift and no gaps, sized so High/Low never come close to the trivial
// stop levels these tests use.
func btcSeries(n int) []domain.Candle {
	out := make([]domain.Candle, n)
	price := 20000.0
	for i := 0; i < n; i++ {
		price *= 1.004 // ~ +0.4%/day drift
		out[i] = domain.Candle{
			OpenTime: day(i), Open: price * 0.999, High: price * 1.01, Low: price * 0.99,
			Close: price, Volume: 100, QuoteVolume: price * 100,
		}
	}
	return out
}

// buyAndHoldStrategy is a test-only Strategy that enters 100% of capital
// into a single symbol the first day it can and never exits — the trivial
// strategy SPEC.md Bölüm 12 (Faz 1 kabul kriteri) requires the engine to
// reproduce real BTC buy-and-hold returns (minus costs) against.
type buyAndHoldStrategy struct {
	symbol string
}

func (s buyAndHoldStrategy) Name() string           { return "buy_and_hold" }
func (s buyAndHoldStrategy) WarmupBars() int        { return 0 }
func (s buyAndHoldStrategy) Params() map[string]any { return map[string]any{"symbol": s.symbol} }
func (s buyAndHoldStrategy) Evaluate(in strategy.Input) ([]domain.Signal, error) {
	if _, open := in.Portfolio.Positions[s.symbol]; open {
		return nil, nil
	}
	last := in.Series[s.symbol][len(in.Series[s.symbol])-1]
	return []domain.Signal{{
		AsOf: in.AsOf, Symbol: s.symbol, Kind: domain.SignalEnter,
		RefPrice: last.Close, StopPrice: 0.01, Reason: "buy and hold",
	}}, nil
}

// allInRiskGate sizes every entry to "spend all available cash", so the
// golden test's return is directly comparable to a raw price ratio
// instead of the (unrelated) risk-per-trade formula SimpleRiskGate uses.
type allInRiskGate struct{}

func (allInRiskGate) Size(s domain.Signal, p domain.Portfolio) (decimal.Decimal, string) {
	if p.Cash <= 0 {
		return decimal.Zero, "insufficient_cash"
	}
	qty := decimal.NewFromFloat(p.Cash / s.RefPrice).Truncate(8)
	if !qty.IsPositive() {
		return decimal.Zero, "below_min_qty"
	}
	return qty, ""
}

// TestGolden_BuyAndHoldMatchesRawBTCReturn is SPEC.md Bölüm 12's single
// non-negotiable Faz 1 gate: "Her gün BTC al-tut trivial stratejisi,
// backtest'te gerçek BTC getirisiyle komisyon farkı kadar örtüşüyor." The
// entry fills on the bar AFTER the signal (İ2); this test recomputes that
// exact fill analytically (SPEC.md Bölüm 6.6.1's formula) and asserts the
// engine's final equity matches it, not just "close in spirit".
func TestGolden_BuyAndHoldMatchesRawBTCReturn(t *testing.T) {
	candles := btcSeries(60)
	costs := broker.Costs{FeeRate: 0.001, SlippageBps: 15}
	const initialCash = 10000.0

	res, err := Run(context.Background(), Config{
		Strategy:    buyAndHoldStrategy{symbol: "BTC/USDT"},
		Candles:     map[string][]domain.Candle{"BTC/USDT": candles},
		InitialCash: initialCash,
		Costs:       costs,
		RiskGate:    allInRiskGate{},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// WarmupBars=0, so the first evaluated day is calendar[1] (day1; see
	// engine.go: Start = calendar[warmup+1]). buy_and_hold's signal there
	// uses day1's CLOSE as RefPrice. The resulting market order fills on
	// the NEXT Advance call, i.e. at day2's OPEN (İ2).
	refCandle := candles[1]
	fillCandle := candles[2]
	fillPrice := fillCandle.Open * (1 + costs.SlippageBps/10000)
	qty := math.Trunc(initialCash/refCandle.Close*1e8) / 1e8 // mirrors allInRiskGate's Truncate(8)
	cost := qty * fillPrice
	fee := cost * costs.FeeRate
	cashRemaining := initialCash - cost - fee

	lastClose := candles[len(candles)-1].Close
	wantEquity := cashRemaining + qty*lastClose

	gotEquity := res.Equity[len(res.Equity)-1].Equity
	if diff := math.Abs(gotEquity - wantEquity); diff > 1e-6 {
		t.Errorf("final equity = %.10f, want %.10f (diff %.2e)", gotEquity, wantEquity, diff)
	}

	rawBTCReturn := lastClose/fillCandle.Open - 1
	engineReturn := gotEquity/initialCash - 1
	roundTripCost := costs.FeeRate + costs.SlippageBps/10000 // one entry only, never exits
	if diff := math.Abs((rawBTCReturn - engineReturn) - roundTripCost); diff > 0.001 {
		t.Errorf("engine return (%.6f) should trail raw BTC return (%.6f) by ~the entry cost (%.6f); diff from expected gap = %.6f",
			engineReturn, rawBTCReturn, roundTripCost, diff)
	}

	if len(res.Trades) != 0 {
		t.Errorf("buy-and-hold should never close a trade, got %d", len(res.Trades))
	}
}

// peekingStrategy deliberately reads one index past its Series slice.
// SPEC.md Bölüm 10's look-ahead test requires this to panic — if it does
// NOT panic, İ2's engine-level guarantee is broken (see
// engine.go/truncateSeries).
type peekingStrategy struct {
	panicked bool
}

func (s *peekingStrategy) Name() string           { return "peeking" }
func (s *peekingStrategy) WarmupBars() int        { return 0 }
func (s *peekingStrategy) Params() map[string]any { return nil }
func (s *peekingStrategy) Evaluate(in strategy.Input) (out []domain.Signal, err error) {
	defer func() {
		if r := recover(); r != nil {
			s.panicked = true
		}
	}()
	series := in.Series["BTC/USDT"]
	_ = series[len(series)] // out of bounds by construction: must panic
	return nil, nil
}

func TestLookAhead_ReadingPastAsOfPanics(t *testing.T) {
	strat := &peekingStrategy{}
	_, err := Run(context.Background(), Config{
		Strategy:    strat,
		Candles:     map[string][]domain.Candle{"BTC/USDT": btcSeries(10)},
		InitialCash: 10000,
		Costs:       broker.Costs{FeeRate: 0.001, SlippageBps: 15},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strat.panicked {
		t.Fatal("reading Series[len(Series)] did not panic — İ2 look-ahead protection is broken")
	}
}

// TestDeterminism is SPEC.md Bölüm 10's zorunlu determinizm testi: the
// same backtest run twice must produce bit-identical metrics.
func TestDeterminism(t *testing.T) {
	run := func() *Result {
		res, err := Run(context.Background(), Config{
			Strategy:    buyAndHoldStrategy{symbol: "BTC/USDT"},
			Candles:     map[string][]domain.Candle{"BTC/USDT": btcSeries(60)},
			InitialCash: 10000,
			Costs:       broker.Costs{FeeRate: 0.001, SlippageBps: 15},
			RiskGate:    allInRiskGate{},
		})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		return res
	}

	a, b := run(), run()
	if !reflect.DeepEqual(a.Metrics, b.Metrics) {
		t.Errorf("metrics differ between two runs of the same backtest:\n%+v\n%+v", a.Metrics, b.Metrics)
	}
	if !reflect.DeepEqual(a.Equity, b.Equity) {
		t.Errorf("equity curves differ between two runs of the same backtest")
	}
}

// TestCostSensitivity is SPEC.md Bölüm 10's zorunlu maliyet duyarlılığı
// testi: doubling commission must meaningfully reduce the result, or the
// cost model isn't actually wired into the fill path.
func TestCostSensitivity(t *testing.T) {
	runWith := func(costs broker.Costs) float64 {
		res, err := Run(context.Background(), Config{
			Strategy:    buyAndHoldStrategy{symbol: "BTC/USDT"},
			Candles:     map[string][]domain.Candle{"BTC/USDT": btcSeries(60)},
			InitialCash: 10000,
			Costs:       costs,
			RiskGate:    allInRiskGate{},
		})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		return res.Metrics.TotalReturn
	}

	base := runWith(broker.Costs{FeeRate: 0.001, SlippageBps: 15})
	doubled := runWith(broker.Costs{FeeRate: 0.002, SlippageBps: 30})

	if doubled >= base {
		t.Fatalf("doubling costs should reduce return: base=%.6f doubled=%.6f", base, doubled)
	}
	if base-doubled < 0.0005 {
		t.Fatalf("doubling costs barely changed the result (base=%.6f doubled=%.6f) — cost model may not be wired into fills", base, doubled)
	}
}
