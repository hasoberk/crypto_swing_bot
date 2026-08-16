package backtest

import (
	"math"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"swingbot/internal/broker"
)

func TestCompute_DrawdownAndWinRate(t *testing.T) {
	equity := []EquityPoint{
		{Date: day(0), Equity: 1000, Exposure: 0},
		{Date: day(1), Equity: 1100, Exposure: 1}, // new peak
		{Date: day(2), Equity: 880, Exposure: 1},  // -20% from peak
		{Date: day(3), Equity: 990, Exposure: 1},  // partial recovery, still below peak
		{Date: day(4), Equity: 1100, Exposure: 0}, // recovers to peak
	}
	trades := []broker.ClosedTrade{
		{Symbol: "A", EntryTime: day(0), ExitTime: day(1), PnLQuote: 100, Fees: 1, Qty: decimal.NewFromInt(1)},
		{Symbol: "B", EntryTime: day(1), ExitTime: day(2), PnLQuote: -50, Fees: 1, Qty: decimal.NewFromInt(1)},
		{Symbol: "C", EntryTime: day(2), ExitTime: day(3), PnLQuote: 30, Fees: 1, Qty: decimal.NewFromInt(1)},
	}

	m := Compute(equity, trades)

	if diff := math.Abs(m.MaxDrawdown - (-0.2)); diff > 1e-9 {
		t.Errorf("MaxDrawdown = %v, want -0.2", m.MaxDrawdown)
	}
	if m.MaxDDDuration != 2 {
		t.Errorf("MaxDDDuration = %d, want 2 (longest gap since a new peak: day1's peak to day3, 2 days; day4 ties it, resetting)", m.MaxDDDuration)
	}
	if m.TradeCount != 3 {
		t.Errorf("TradeCount = %d, want 3", m.TradeCount)
	}
	wantWinRate := 2.0 / 3.0
	if diff := math.Abs(m.WinRate - wantWinRate); diff > 1e-9 {
		t.Errorf("WinRate = %v, want %v", m.WinRate, wantWinRate)
	}
	wantPF := 130.0 / 50.0
	if diff := math.Abs(m.ProfitFactor - wantPF); diff > 1e-9 {
		t.Errorf("ProfitFactor = %v, want %v", m.ProfitFactor, wantPF)
	}
	if diff := math.Abs(m.TotalFees - 3); diff > 1e-9 {
		t.Errorf("TotalFees = %v, want 3", m.TotalFees)
	}
}

func TestBuyAndHoldCurve_AppliesCostsOnce(t *testing.T) {
	candles := btcSeries(10)
	costs := broker.Costs{FeeRate: 0.001, SlippageBps: 15}

	calendar := make([]time.Time, len(candles))
	for i, c := range candles {
		calendar[i] = c.OpenTime
	}

	curve := BuyAndHoldCurve(candles, calendar, 10000, costs)
	if len(curve) != len(candles) {
		t.Fatalf("curve length = %d, want %d", len(curve), len(candles))
	}
	if curve[0].Exposure < 0.99 {
		t.Errorf("day0 exposure = %v, want ~1 (fully invested immediately)", curve[0].Exposure)
	}
	wantFinal := curve[0].Equity / candles[0].Close * candles[len(candles)-1].Close
	if diff := math.Abs(curve[len(curve)-1].Equity - wantFinal); diff > 1e-6 {
		t.Errorf("final equity = %v, want %v (equity should scale with close price after the one-time entry cost)", curve[len(curve)-1].Equity, wantFinal)
	}
}
