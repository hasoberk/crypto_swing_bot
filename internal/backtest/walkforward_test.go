package backtest

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"swingbot/internal/broker"
	"swingbot/internal/domain"
	"swingbot/internal/strategy"
)

func TestBuildWindows(t *testing.T) {
	tests := []struct {
		name                          string
		dataStart, dataEnd            time.Time
		trainDays, testDays, stepDays int
		wantWindows                   int
	}{
		{
			name: "exactly two windows", dataStart: day(0), dataEnd: day(0).AddDate(0, 0, 365+90+90),
			trainDays: 365, testDays: 90, stepDays: 90, wantWindows: 2,
		},
		{
			name: "too short for even one window", dataStart: day(0), dataEnd: day(0).AddDate(0, 0, 100),
			trainDays: 365, testDays: 90, stepDays: 90, wantWindows: 0,
		},
		{
			name: "exactly one window", dataStart: day(0), dataEnd: day(0).AddDate(0, 0, 365+90),
			trainDays: 365, testDays: 90, stepDays: 90, wantWindows: 1,
		},
		{
			name: "invalid params", dataStart: day(0), dataEnd: day(0).AddDate(0, 0, 1000),
			trainDays: 0, testDays: 90, stepDays: 90, wantWindows: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildWindows(tt.dataStart, tt.dataEnd, tt.trainDays, tt.testDays, tt.stepDays)
			if len(got) != tt.wantWindows {
				t.Fatalf("BuildWindows() = %d windows, want %d (%+v)", len(got), tt.wantWindows, got)
			}
			for i, w := range got {
				if !w.TestStart.Equal(w.TrainEnd) {
					t.Errorf("window %d: TestStart != TrainEnd (%v vs %v) — train/test must not overlap", i, w.TestStart, w.TrainEnd)
				}
				if w.TrainEnd.Sub(w.TrainStart) != time.Duration(tt.trainDays)*24*time.Hour {
					t.Errorf("window %d: train span = %v, want %d days", i, w.TrainEnd.Sub(w.TrainStart), tt.trainDays)
				}
				if w.TestEnd.After(tt.dataEnd) {
					t.Errorf("window %d: TestEnd %v exceeds dataEnd %v", i, w.TestEnd, tt.dataEnd)
				}
				if i > 0 {
					if got[i].TrainStart.Sub(got[i-1].TrainStart) != time.Duration(tt.stepDays)*24*time.Hour {
						t.Errorf("window %d: did not slide forward by stepDays", i)
					}
				}
			}
		})
	}
}

func TestClassifyRegime(t *testing.T) {
	tests := []struct {
		name      string
		btcReturn float64
		want      Regime
	}{
		{"strong bull", 0.35, RegimeBull},
		{"exactly at bull threshold", 0.20, RegimeBull},
		{"strong bear", -0.35, RegimeBear},
		{"exactly at bear threshold", -0.20, RegimeBear},
		{"sideways", 0.05, RegimeSideways},
		{"mild negative sideways", -0.10, RegimeSideways},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyRegime(tt.btcReturn, 0.20, 0.20); got != tt.want {
				t.Errorf("ClassifyRegime(%.2f) = %s, want %s", tt.btcReturn, got, tt.want)
			}
		})
	}
}

func TestCartesianParamGrid(t *testing.T) {
	axes := []ParamAxis{
		{Name: "a", Values: []float64{1, 2}},
		{Name: "b", Values: []float64{10, 20, 30}},
	}
	grid, err := CartesianParamGrid(axes, 500)
	if err != nil {
		t.Fatalf("CartesianParamGrid: %v", err)
	}
	if len(grid) != 6 {
		t.Fatalf("len(grid) = %d, want 6 (2*3)", len(grid))
	}
	seen := map[string]bool{}
	for _, ps := range grid {
		key := fmt.Sprintf("a=%v,b=%v", ps["a"], ps["b"])
		seen[key] = true
	}
	if len(seen) != 6 {
		t.Errorf("grid combinations are not all distinct: %d unique of 6", len(seen))
	}

	if _, err := CartesianParamGrid(axes, 3); err == nil {
		t.Error("CartesianParamGrid with maxCombos=3 should have refused a 6-combination grid")
	}

	if grid, err := CartesianParamGrid(nil, 0); err != nil || grid != nil {
		t.Errorf("CartesianParamGrid(nil) = (%v, %v), want (nil, nil)", grid, err)
	}
}

// offsetEntryStrategy enters symbol on the offset-th day it is evaluated
// and never exits — used to prove RunWalkForward's training-slice
// parameter selection actually picks the objectively better ParamSet
// (smaller offset = enters earlier = higher return in a monotonically
// rising synthetic series).
type offsetEntryStrategy struct {
	symbol string
	offset int
	day    int
}

func (s *offsetEntryStrategy) Name() string    { return "offset_entry" }
func (s *offsetEntryStrategy) WarmupBars() int { return 0 }
func (s *offsetEntryStrategy) Params() map[string]any {
	return map[string]any{"offset": s.offset}
}
func (s *offsetEntryStrategy) Evaluate(in strategy.Input) ([]domain.Signal, error) {
	s.day++
	if _, open := in.Portfolio.Positions[s.symbol]; open {
		return nil, nil
	}
	if s.day < s.offset {
		return nil, nil
	}
	series := in.Series[s.symbol]
	last := series[len(series)-1]
	return []domain.Signal{{
		AsOf: in.AsOf, Symbol: s.symbol, Kind: domain.SignalEnter,
		RefPrice: last.Close, StopPrice: 0.01, Reason: "offset entry test",
	}}, nil
}

func TestRunWalkForward_SelectsBetterTrainingParams(t *testing.T) {
	candles := map[string][]domain.Candle{"BTC/USDT": btcSeries(500)}
	costs := broker.Costs{FeeRate: 0.001, SlippageBps: 15}

	factory := func(ps ParamSet) (strategy.Strategy, error) {
		offset := 30
		if v, ok := ps["offset"]; ok {
			offset = int(v)
		}
		return &offsetEntryStrategy{symbol: "BTC/USDT", offset: offset}, nil
	}

	// All three offsets are deliberately > 7 (sliceCandlesForWindow's
	// warmup-lookback buffer for a WarmupBars()==0 strategy): an entry
	// inside that buffer would land BEFORE the training window's nominal
	// start and get trimmed differently by runSubBacktest's filterEquity,
	// which would make the comparison unfair rather than testing what this
	// test is actually about (does selectParams pick the objectively
	// better in-window choice).
	cfg := WalkForwardConfig{
		NewStrategy: factory,
		ParamGrid:   []ParamSet{{"offset": 40}, {"offset": 20}, {"offset": 8}},
		Candles:     candles,
		InitialCash: 10000,
		Costs:       costs,
		NewRiskGate: func() RiskGate { return allInRiskGate{} },
		TrainDays:   150, TestDays: 60, StepDays: 60,
		BenchmarkSymbol: "BTC/USDT",
	}

	res, err := RunWalkForward(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunWalkForward: %v", err)
	}
	if len(res.Windows) == 0 {
		t.Fatal("expected at least one window")
	}
	for i, w := range res.Windows {
		if got := w.ChosenParams["offset"]; got != 8 {
			t.Errorf("window %d: ChosenParams[offset] = %v, want 8 (earliest entry always wins in a monotonic uptrend)", i, got)
		}
	}
}

func TestRunWalkForward_WindowBoundsAndChaining(t *testing.T) {
	candles := map[string][]domain.Candle{"BTC/USDT": btcSeries(500)}
	costs := broker.Costs{FeeRate: 0.001, SlippageBps: 15}

	cfg := WalkForwardConfig{
		NewStrategy: func(ParamSet) (strategy.Strategy, error) {
			return buyAndHoldStrategy{symbol: "BTC/USDT"}, nil
		},
		Candles:     candles,
		InitialCash: 10000,
		Costs:       costs,
		NewRiskGate: func() RiskGate { return allInRiskGate{} },
		TrainDays:   150, TestDays: 60, StepDays: 60,
		BenchmarkSymbol: "BTC/USDT",
	}

	res, err := RunWalkForward(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunWalkForward: %v", err)
	}
	if len(res.Windows) < 2 {
		t.Fatalf("expected >= 2 windows for this range, got %d", len(res.Windows))
	}

	for i, w := range res.Windows {
		for _, p := range w.TestEquity {
			if p.Date.Before(w.Window.TestStart) || !p.Date.Before(w.Window.TestEnd) {
				t.Errorf("window %d: equity point %v outside [%v,%v)", i, p.Date, w.Window.TestStart, w.Window.TestEnd)
			}
		}
		for _, tr := range w.TestTrades {
			if tr.EntryTime.Before(w.Window.TestStart) || !tr.EntryTime.Before(w.Window.TestEnd) {
				t.Errorf("window %d: trade entry %v outside [%v,%v) — a training window's data leaked into the test slice, or vice versa", i, tr.EntryTime, w.Window.TestStart, w.Window.TestEnd)
			}
		}
	}

	// CombinedEquity must be one continuous curve: total length equals the
	// sum of each window's own TestEquity length, in window order.
	var wantLen int
	for _, w := range res.Windows {
		wantLen += len(w.TestEquity)
	}
	if len(res.CombinedEquity) != wantLen {
		t.Errorf("len(CombinedEquity) = %d, want %d (sum of window test-equity lengths)", len(res.CombinedEquity), wantLen)
	}

	// Chaining: window i+1's first equity point's Equity should equal
	// window i's LAST equity point's Equity (same-day carry, not reset to
	// InitialCash).
	for i := 1; i < len(res.Windows); i++ {
		prevEnd := res.Windows[i-1].TestEquity[len(res.Windows[i-1].TestEquity)-1]
		curStart := res.Windows[i].TestEquity[0]
		if curStart.Date.Equal(prevEnd.Date) && curStart.Equity != prevEnd.Equity {
			t.Errorf("window %d does not chain from window %d's ending equity: got %.4f, want %.4f", i, i-1, curStart.Equity, prevEnd.Equity)
		}
	}
}

func TestRunWalkForward_Deterministic(t *testing.T) {
	candles := map[string][]domain.Candle{"BTC/USDT": btcSeries(500)}
	costs := broker.Costs{FeeRate: 0.001, SlippageBps: 15}

	newCfg := func() WalkForwardConfig {
		return WalkForwardConfig{
			NewStrategy: func(ParamSet) (strategy.Strategy, error) {
				return buyAndHoldStrategy{symbol: "BTC/USDT"}, nil
			},
			Candles: candles, InitialCash: 10000, Costs: costs,
			NewRiskGate: func() RiskGate { return allInRiskGate{} },
			TrainDays:   150, TestDays: 60, StepDays: 60,
			BenchmarkSymbol: "BTC/USDT",
		}
	}

	a, err := RunWalkForward(context.Background(), newCfg())
	if err != nil {
		t.Fatalf("RunWalkForward (a): %v", err)
	}
	b, err := RunWalkForward(context.Background(), newCfg())
	if err != nil {
		t.Fatalf("RunWalkForward (b): %v", err)
	}
	if !reflect.DeepEqual(a.Metrics, b.Metrics) {
		t.Errorf("walk-forward Metrics differ between two identical runs:\n%+v\n%+v", a.Metrics, b.Metrics)
	}
}

func TestSplitDevLocked(t *testing.T) {
	candles := map[string][]domain.Candle{"BTC/USDT": btcSeries(20)} // day(0)..day(19)
	cutoff := day(10)

	dev, locked := SplitDevLocked(candles, cutoff)
	for _, c := range dev["BTC/USDT"] {
		if !c.OpenTime.Before(cutoff) {
			t.Errorf("dev segment contains a candle at/after cutoff: %v", c.OpenTime)
		}
	}
	for _, c := range locked["BTC/USDT"] {
		if c.OpenTime.Before(cutoff) {
			t.Errorf("locked segment contains a candle before cutoff: %v", c.OpenTime)
		}
	}
	if len(dev["BTC/USDT"])+len(locked["BTC/USDT"]) != 20 {
		t.Errorf("dev+locked = %d candles, want 20 (every candle must land in exactly one segment)", len(dev["BTC/USDT"])+len(locked["BTC/USDT"]))
	}
}
