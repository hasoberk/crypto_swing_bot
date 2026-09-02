package broker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"swingbot/internal/domain"
)

var testCosts = Costs{FeeRate: 0.001, SlippageBps: 15}

func day(n int) time.Time {
	return time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, n)
}

func mustBroker(t *testing.T, candles map[string][]domain.Candle, cash float64, costs Costs) *PaperBroker {
	t.Helper()
	b, err := NewPaperBroker("backtest", candles, cash, costs, NewBacktestClock(day(0)), "test")
	if err != nil {
		t.Fatalf("NewPaperBroker: %v", err)
	}
	return b
}

func candle(n int, open, high, low, close float64) domain.Candle {
	return domain.Candle{OpenTime: day(n), Open: open, High: high, Low: low, Close: close, Volume: 1, QuoteVolume: open}
}

// TestZeroCostsRejected is the İ4 guard: a PaperBroker refuses to
// simulate cost-free, even if its caller forgot to validate config first.
func TestZeroCostsRejected(t *testing.T) {
	candles := map[string][]domain.Candle{"BTC/USDT": {candle(0, 100, 100, 100, 100)}}
	_, err := NewPaperBroker("backtest", candles, 10000, Costs{}, NewBacktestClock(day(0)), "test")
	if !errors.Is(err, ErrCostsNotConfigured) {
		t.Fatalf("want ErrCostsNotConfigured, got %v", err)
	}
}

// TestMarketEntryFillsNextBarOpen is İ2 at the broker level: a market buy
// submitted while the broker's clock sits on day t must fill using day
// t+1's open, with slippage moving the price against the buyer.
func TestMarketEntryFillsNextBarOpen(t *testing.T) {
	candles := map[string][]domain.Candle{
		"BTC/USDT": {
			candle(0, 100, 105, 95, 102),
			candle(1, 110, 112, 108, 111), // t+1 open = 110
		},
	}
	b := mustBroker(t, candles, 10000, testCosts)
	ctx := context.Background()

	if err := b.Advance(ctx, day(0)); err != nil {
		t.Fatalf("Advance(0): %v", err)
	}

	_, err := b.Submit(ctx, domain.OrderRequest{
		ClientOrderID: "entry-1", Symbol: "BTC/USDT", Side: domain.SideBuy, Type: "market",
		Qty: decimal.NewFromInt(10),
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// Not yet filled: still day 0, no bar has passed.
	pf, _ := b.Portfolio(ctx)
	if _, open := pf.Positions["BTC/USDT"]; open {
		t.Fatalf("position should not exist before the next Advance")
	}

	if err := b.Advance(ctx, day(1)); err != nil {
		t.Fatalf("Advance(1): %v", err)
	}

	pf, _ = b.Portfolio(ctx)
	pos, open := pf.Positions["BTC/USDT"]
	if !open {
		t.Fatalf("expected an open position after day 1's Advance")
	}
	wantFill := 110 * (1 + 15.0/10000)
	if diff := pos.EntryPrice - wantFill; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("EntryPrice = %v, want %v", pos.EntryPrice, wantFill)
	}

	wantCost := wantFill*10 + wantFill*10*0.001
	wantCash := 10000 - wantCost
	if diff := pf.Cash - wantCash; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("Cash = %v, want %v", pf.Cash, wantCash)
	}
}

// TestStopGapRule is the single most important simulation detail in the
// project (SPEC.md Bölüm 6.6.1): a stop must fill at the WORSE of the
// stop level and the next bar's open, never assume it always fills at the
// exact stop price.
func TestStopGapRule(t *testing.T) {
	tests := []struct {
		name        string
		nextOpen    float64
		nextLow     float64
		stop        float64
		wantGapFill bool // true: open gapped below stop, fill uses open
	}{
		{"normal stop touch", 98, 94, 95, false}, // open still above stop
		{"gap below stop", 90, 85, 95, true},     // open already through stop
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			candles := map[string][]domain.Candle{
				"BTC/USDT": {
					candle(0, 100, 105, 99, 100),
					candle(1, 100, 100, 100, 100), // entry fill bar (t+1 open = 100)
					candle(2, tc.nextOpen, tc.nextOpen+2, tc.nextLow, tc.nextOpen+1),
				},
			}
			b := mustBroker(t, candles, 10000, testCosts)
			ctx := context.Background()

			must(t, b.Advance(ctx, day(0)))
			_, err := b.Submit(ctx, domain.OrderRequest{
				ClientOrderID: "e1", Symbol: "BTC/USDT", Side: domain.SideBuy, Type: "market",
				Qty: decimal.NewFromInt(10),
			})
			if err != nil {
				t.Fatal(err)
			}
			must(t, b.Advance(ctx, day(1))) // entry fills here at open=100

			_, err = b.Submit(ctx, domain.OrderRequest{
				ClientOrderID: "s1", Symbol: "BTC/USDT", Side: domain.SideSell, Type: "stop_market",
				Price: decimal.NewFromFloat(tc.stop),
			})
			if err != nil {
				t.Fatal(err)
			}

			must(t, b.Advance(ctx, day(2))) // stop check happens here

			pf, _ := b.Portfolio(ctx)
			if _, stillOpen := pf.Positions["BTC/USDT"]; stillOpen {
				t.Fatalf("position should have been stopped out")
			}

			trades := b.ClosedTrades()
			if len(trades) != 1 {
				t.Fatalf("want 1 closed trade, got %d", len(trades))
			}
			tr := trades[0]
			if tr.ExitReason != "stop" {
				t.Errorf("ExitReason = %q, want \"stop\"", tr.ExitReason)
			}

			gapBase := tc.stop
			if tc.wantGapFill {
				gapBase = tc.nextOpen
			}
			wantExit := gapBase * (1 - 15.0/10000)
			if diff := tr.ExitPrice - wantExit; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("ExitPrice = %v, want %v (gap rule: min(stop, open))", tr.ExitPrice, wantExit)
			}
		})
	}
}

// TestSubmitIdempotent is SPEC.md Bölüm 10's zorunlu idempotency testi
// (İ5): resubmitting the same ClientOrderID must not create a second
// order or a second fill.
func TestSubmitIdempotent(t *testing.T) {
	candles := map[string][]domain.Candle{
		"BTC/USDT": {candle(0, 100, 105, 95, 102), candle(1, 110, 112, 108, 111)},
	}
	b := mustBroker(t, candles, 10000, testCosts)
	ctx := context.Background()
	must(t, b.Advance(ctx, day(0)))

	req := domain.OrderRequest{ClientOrderID: "dup-1", Symbol: "BTC/USDT", Side: domain.SideBuy, Type: "market", Qty: decimal.NewFromInt(1)}
	o1, err := b.Submit(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	o2, err := b.Submit(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if o1.ID != o2.ID {
		t.Errorf("resubmitting the same ClientOrderID produced a different order (%s vs %s)", o1.ID, o2.ID)
	}
	if len(b.pending) != 1 {
		t.Fatalf("want exactly 1 queued fill, got %d", len(b.pending))
	}
}

// TestLongOnlyGuardsRejectSecondEntry protects the "one position per
// symbol" invariant the golden buy-and-hold test and the SPEC.md Bölüm
// 6.4 strategies all rely on.
func TestLongOnlyGuardsRejectSecondEntry(t *testing.T) {
	candles := map[string][]domain.Candle{
		"BTC/USDT": {candle(0, 100, 105, 95, 102), candle(1, 110, 112, 108, 111)},
	}
	b := mustBroker(t, candles, 10000, testCosts)
	ctx := context.Background()
	must(t, b.Advance(ctx, day(0)))

	_, err := b.Submit(ctx, domain.OrderRequest{ClientOrderID: "e1", Symbol: "BTC/USDT", Side: domain.SideBuy, Type: "market", Qty: decimal.NewFromInt(1)})
	if err != nil {
		t.Fatal(err)
	}
	must(t, b.Advance(ctx, day(1))) // fills, position now open

	_, err = b.Submit(ctx, domain.OrderRequest{ClientOrderID: "e2", Symbol: "BTC/USDT", Side: domain.SideBuy, Type: "market", Qty: decimal.NewFromInt(1)})
	if err == nil {
		t.Fatalf("expected an error submitting a second entry for an already-open symbol")
	}
}

// TestSubmitRejectsDuplicateExit protects against a duplicate/retried
// exit submission (e.g. a buggy Strategy emitting two SignalExit entries
// for the same symbol in one Evaluate call) queuing two pending sells for
// the same position. Before this guard existed, the second sell's
// settleExit would silently no-op after the first closed the position,
// while markFilled still recorded it as a fabricated FILLED trade.
func TestSubmitRejectsDuplicateExit(t *testing.T) {
	candles := map[string][]domain.Candle{
		"BTC/USDT": {candle(0, 100, 105, 95, 102), candle(1, 110, 112, 108, 111), candle(2, 111, 115, 109, 112)},
	}
	b := mustBroker(t, candles, 10000, testCosts)
	ctx := context.Background()
	must(t, b.Advance(ctx, day(0)))

	_, err := b.Submit(ctx, domain.OrderRequest{ClientOrderID: "e1", Symbol: "BTC/USDT", Side: domain.SideBuy, Type: "market", Qty: decimal.NewFromInt(1)})
	if err != nil {
		t.Fatal(err)
	}
	must(t, b.Advance(ctx, day(1))) // entry fills

	_, err = b.Submit(ctx, domain.OrderRequest{ClientOrderID: "x1", Symbol: "BTC/USDT", Side: domain.SideSell, Type: "market", Qty: decimal.NewFromInt(1)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.Submit(ctx, domain.OrderRequest{ClientOrderID: "x2", Symbol: "BTC/USDT", Side: domain.SideSell, Type: "market", Qty: decimal.NewFromInt(1)})
	if err == nil {
		t.Fatalf("expected an error submitting a second exit while one is already pending")
	}

	must(t, b.Advance(ctx, day(2))) // x1 fills; must be the only closed trade
	trades := b.ClosedTrades()
	if len(trades) != 1 {
		t.Fatalf("want exactly 1 closed trade, got %d: %+v", len(trades), trades)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
