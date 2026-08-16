package web

import (
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"swingbot/internal/backtest"
	"swingbot/internal/broker"
)

func TestGenerateReport_ContainsAllNineSections(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	equity := make([]backtest.EquityPoint, 0, 400)
	e := 10000.0
	for i := 0; i < 400; i++ {
		e *= 1.001
		equity = append(equity, backtest.EquityPoint{
			Date: start.AddDate(0, 0, i), Equity: e, Cash: e * 0.1, Exposure: 0.9,
		})
	}
	bench := make([]backtest.EquityPoint, len(equity))
	b := 10000.0
	for i := range bench {
		b *= 1.0009
		bench[i] = backtest.EquityPoint{Date: equity[i].Date, Equity: b, Exposure: 1}
	}

	metrics := backtest.Compute(equity, nil)
	benchMetrics := backtest.Compute(bench, nil)
	metrics.BenchBTC = &benchMetrics
	metrics.BenchTop10 = &benchMetrics

	trades := []broker.ClosedTrade{
		{Symbol: "BTC/USDT", EntryTime: start, EntryPrice: 100, ExitTime: start.AddDate(0, 0, 5), ExitPrice: 110,
			Qty: decimal.NewFromInt(1), Fees: 1, PnLQuote: 9, PnLPct: 0.09, ExitReason: "signal"},
		{Symbol: "BTC/USDT", EntryTime: start.AddDate(0, 0, 10), EntryPrice: 110, ExitTime: start.AddDate(0, 0, 12), ExitPrice: 100,
			Qty: decimal.NewFromInt(1), Fees: 1, PnLQuote: -11, PnLPct: -0.09, ExitReason: "stop"},
	}

	out, err := GenerateReport(ReportData{
		Strategy: "trendfollow",
		Params:   map[string]any{"atr_stop_mult": 2.5},
		Start:    equity[0].Date, End: equity[len(equity)-1].Date,
		Costs:       broker.Costs{FeeRate: 0.001, SlippageBps: 15},
		GitSHA:      "abc1234",
		GeneratedAt: time.Now().UTC(),
		Warnings:    []string{"kısa örneklem: 400 gün"},
		Metrics:     metrics,
		Equity:      equity,
		BenchBTC:    bench,
		BenchTop10:  bench,
		Trades:      trades,
		Rejections:  []backtest.Rejection{{AsOf: start, Symbol: "DOGE/USDT", Reason: "below_min_notional"}},
	})
	if err != nil {
		t.Fatalf("GenerateReport: %v", err)
	}

	if !strings.HasPrefix(out, "<!doctype html>") {
		t.Errorf("report does not start with a doctype")
	}
	for _, want := range []string{
		"trendfollow", "kısa örneklem", "Equity Eğrisi", "Düşüş (Drawdown)",
		"Metrikler", "Yıllık Kırılım", "İşlem Dağılımı", "İşlem Listesi",
		"Parametre Duyarlılığı", "below_min_notional", "abc1234", "<svg",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing expected content %q", want)
		}
	}
	if strings.Contains(out, ">NaN<") || strings.Contains(out, "+Inf") || strings.Contains(out, "-Inf") {
		t.Errorf("report leaked a raw NaN/Inf value into the HTML")
	}
}

func TestGenerateReport_EmptyDataDoesNotPanic(t *testing.T) {
	_, err := GenerateReport(ReportData{Strategy: "empty", GeneratedAt: time.Now().UTC()})
	if err != nil {
		t.Fatalf("GenerateReport on empty data: %v", err)
	}
}
