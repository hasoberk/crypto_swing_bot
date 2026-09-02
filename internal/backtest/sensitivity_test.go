package backtest

import (
	"context"
	"testing"

	"swingbot/internal/broker"
	"swingbot/internal/domain"
	"swingbot/internal/strategy"
)

func metricOfSharpe(m Metrics) float64 { return m.Sharpe }

func TestDetectPlateau(t *testing.T) {
	pt := func(v, sharpe float64) SensitivityPoint {
		return SensitivityPoint{Param: "atr_stop_mult", Value: v, Metrics: Metrics{Sharpe: sharpe}}
	}

	tests := []struct {
		name          string
		points        []SensitivityPoint
		wantIsPlateau bool
	}{
		{
			// SPEC.md Bölüm 11.3's own "coincidence" example: 1.8 at the
			// center, 0.4 on both immediate neighbors.
			name:          "sharp peak (SPEC.md example)",
			points:        []SensitivityPoint{pt(2.0, 0.4), pt(2.4, 0.4), pt(2.5, 1.8), pt(2.6, 0.4), pt(3.0, 0.4)},
			wantIsPlateau: false,
		},
		{
			// SPEC.md Bölüm 11.3's own "may be real" example: a wide band
			// of 0.9-1.1.
			name:          "plateau (SPEC.md example)",
			points:        []SensitivityPoint{pt(2.0, 0.95), pt(2.4, 1.0), pt(2.5, 1.1), pt(2.6, 1.05), pt(3.0, 0.9)},
			wantIsPlateau: true,
		},
		{
			name:          "too few points to judge",
			points:        []SensitivityPoint{pt(2.4, 1.0), pt(2.5, 1.1)},
			wantIsPlateau: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := DetectPlateau("atr_stop_mult", tt.points, metricOfSharpe, 0)
			if v.IsPlateau != tt.wantIsPlateau {
				t.Errorf("DetectPlateau().IsPlateau = %v, want %v (detail: %s)", v.IsPlateau, tt.wantIsPlateau, v.Detail)
			}
			if v.Detail == "" {
				t.Error("PlateauVerdict.Detail should never be empty")
			}
		})
	}
}

func TestGroupByParam(t *testing.T) {
	points := []SensitivityPoint{
		{Param: "a", Value: 1}, {Param: "b", Value: 1}, {Param: "a", Value: 2},
	}
	order, byParam := GroupByParam(points)
	if len(order) != 2 || order[0] != "a" || order[1] != "b" {
		t.Fatalf("GroupByParam order = %v, want [a b]", order)
	}
	if len(byParam["a"]) != 2 {
		t.Errorf("byParam[a] has %d points, want 2", len(byParam["a"]))
	}
	if len(byParam["b"]) != 1 {
		t.Errorf("byParam[b] has %d points, want 1", len(byParam["b"]))
	}
}

// TestParamSensitivitySweep_Integration runs a real Trendfollow-shaped
// strategy (via a tiny local factory, not internal/strategy directly, to
// keep this package's test independent of internal/strategy's own default
// values changing) across a small atr_stop_mult axis and checks that every
// requested value produced a usable SensitivityPoint.
func TestParamSensitivitySweep_Integration(t *testing.T) {
	candles := map[string][]domain.Candle{"BTC/USDT": btcSeries(400)}
	costs := broker.Costs{FeeRate: 0.001, SlippageBps: 15}

	factory := func(ps ParamSet) (strategy.Strategy, error) {
		offset := 30
		if v, ok := ps["offset"]; ok {
			offset = int(v)
		}
		return &offsetEntryStrategy{symbol: "BTC/USDT", offset: offset}, nil
	}

	points, err := ParamSensitivitySweep(context.Background(), SensitivityConfig{
		NewStrategy: factory,
		BaseParams:  ParamSet{"offset": 20},
		Axes:        []ParamAxis{{Name: "offset", Values: []float64{5, 10, 20, 30, 40}}},
		Candles:     candles, InitialCash: 10000, Costs: costs,
		RiskGate: allInRiskGate{},
	})
	if err != nil {
		t.Fatalf("ParamSensitivitySweep: %v", err)
	}
	if len(points) != 5 {
		t.Fatalf("len(points) = %d, want 5 (one per axis value)", len(points))
	}

	// Earlier entry (smaller offset) should produce a strictly higher
	// TotalReturn in this monotonically rising synthetic series.
	byOffset := map[float64]float64{}
	for _, p := range points {
		byOffset[p.Value] = p.Metrics.TotalReturn
	}
	if !(byOffset[5] > byOffset[20] && byOffset[20] > byOffset[40]) {
		t.Errorf("expected TotalReturn to decrease as offset grows: offset5=%.4f offset20=%.4f offset40=%.4f",
			byOffset[5], byOffset[20], byOffset[40])
	}
}
