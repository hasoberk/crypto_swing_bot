package datafeed

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"swingbot/internal/domain"
	"swingbot/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func btcMarket() domain.Market {
	return domain.Market{
		Symbol: "BTC/USDT", Base: "BTC", Quote: "USDT", Active: true,
		TickSize: decimal.NewFromFloat(0.01), StepSize: decimal.NewFromFloat(0.0001), MinNotional: decimal.NewFromInt(10),
	}
}

func ethMarket() domain.Market {
	return domain.Market{
		Symbol: "ETH/USDT", Base: "ETH", Quote: "USDT", Active: true,
		TickSize: decimal.NewFromFloat(0.01), StepSize: decimal.NewFromFloat(0.001), MinNotional: decimal.NewFromInt(10),
	}
}

func TestBackfillFetchesAndPersists(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	ex := newFakeExchange()
	ex.markets = []domain.Market{btcMarket()}

	start := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start.AddDate(0, 0, 40)
	ex.candles["BTC/USDT"] = dailyCandles(start, 40)

	f := NewFeed(ex, s, "1d", WithClock(func() time.Time { return now }), WithBackfillYears(3))

	report, err := f.Backfill(ctx, BackfillOptions{Symbols: []string{"BTC/USDT"}, Years: 1})
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if len(report.Symbols) != 1 || report.Symbols[0].CandlesWritten == 0 {
		t.Fatalf("expected candles written, got report=%+v", report)
	}

	n, err := s.CountCandles(ctx, "BTC/USDT", "1d")
	if err != nil {
		t.Fatalf("CountCandles: %v", err)
	}
	// All 40 candles close before `now`, so all should be stored.
	if n != 40 {
		t.Fatalf("expected 40 stored candles, got %d", n)
	}

	m, ok, err := s.GetMarket(ctx, "BTC/USDT")
	if err != nil || !ok {
		t.Fatalf("expected market row to exist after backfill sync: ok=%v err=%v", ok, err)
	}
	if !m.Active {
		t.Errorf("expected market to be active")
	}
}

func TestBackfillNeverPersistsUnclosedCandle(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	ex := newFakeExchange()
	ex.markets = []domain.Market{btcMarket()}

	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	// now is only 5h into the last candle's day: that candle is not closed.
	now := start.AddDate(0, 0, 9).Add(5 * time.Hour)
	ex.candles["BTC/USDT"] = dailyCandles(start, 10) // days 0..9, last open_time = start+9d

	f := NewFeed(ex, s, "1d", WithClock(func() time.Time { return now }))
	_, err := f.Backfill(ctx, BackfillOptions{Symbols: []string{"BTC/USDT"}, Years: 1})
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}

	last, ok, err := s.MaxOpenTime(ctx, "BTC/USDT", "1d")
	if err != nil || !ok {
		t.Fatalf("expected some candles stored: ok=%v err=%v", ok, err)
	}
	if !last.Before(start.AddDate(0, 0, 9)) {
		t.Fatalf("expected the still-open final candle (open_time=%s) to be excluded, but MaxOpenTime=%s", start.AddDate(0, 0, 9), last)
	}
}

func TestBackfillResumesFromCheckpoint(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	ex := newFakeExchange()
	ex.markets = []domain.Market{btcMarket()}

	start := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start.AddDate(0, 0, 40)
	ex.candles["BTC/USDT"] = dailyCandles(start, 40)

	f := NewFeed(ex, s, "1d", WithClock(func() time.Time { return now }))

	// First run: only fetch the first 10 days (simulates an interrupted run
	// by artificially truncating the exchange's dataset first).
	full := ex.candles["BTC/USDT"]
	ex.candles["BTC/USDT"] = full[:10]
	if _, err := f.Backfill(ctx, BackfillOptions{Symbols: []string{"BTC/USDT"}, Years: 1}); err != nil {
		t.Fatalf("first Backfill: %v", err)
	}
	n, _ := s.CountCandles(ctx, "BTC/USDT", "1d")
	if n != 10 {
		t.Fatalf("expected 10 candles after first (truncated) backfill, got %d", n)
	}

	// "Restore" full history and re-run: it must resume after day 10, not
	// refetch from the beginning.
	ex.candles["BTC/USDT"] = full
	ex.calls = nil
	if _, err := f.Backfill(ctx, BackfillOptions{Symbols: []string{"BTC/USDT"}, Years: 1}); err != nil {
		t.Fatalf("second Backfill: %v", err)
	}

	if len(ex.calls) == 0 {
		t.Fatal("expected the resumed backfill to make at least one call")
	}
	firstCallSince := ex.calls[0].since
	if !firstCallSince.Equal(start.AddDate(0, 0, 10)) {
		t.Fatalf("expected resumed backfill to start at day 10 (%s), got since=%s", start.AddDate(0, 0, 10), firstCallSince)
	}

	n, _ = s.CountCandles(ctx, "BTC/USDT", "1d")
	if n != 40 {
		t.Fatalf("expected all 40 candles after resumed backfill, got %d", n)
	}
}

func TestUpdateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	ex := newFakeExchange()
	ex.markets = []domain.Market{btcMarket()}

	start := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start.AddDate(0, 0, 40).Add(1 * time.Hour) // fixed clock, does not advance between calls
	ex.candles["BTC/USDT"] = dailyCandles(start, 40)

	f := NewFeed(ex, s, "1d", WithClock(func() time.Time { return now }))

	if _, err := f.Backfill(ctx, BackfillOptions{Symbols: []string{"BTC/USDT"}, Years: 1}); err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	n1, _ := s.CountCandles(ctx, "BTC/USDT", "1d")

	if _, err := f.Update(ctx); err != nil {
		t.Fatalf("first Update: %v", err)
	}
	n2, _ := s.CountCandles(ctx, "BTC/USDT", "1d")

	if _, err := f.Update(ctx); err != nil {
		t.Fatalf("second Update: %v", err)
	}
	n3, _ := s.CountCandles(ctx, "BTC/USDT", "1d")

	if n1 != n2 || n2 != n3 {
		t.Fatalf("expected row count to stay constant across Update calls with a fixed clock: n1=%d n2=%d n3=%d", n1, n2, n3)
	}
}

func TestUpdateRewritesLastThreeCandles(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	ex := newFakeExchange()
	ex.markets = []domain.Market{btcMarket()}

	start := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start.AddDate(0, 0, 10).Add(1 * time.Hour)
	ex.candles["BTC/USDT"] = dailyCandles(start, 10) // last closed candle: day 9

	f := NewFeed(ex, s, "1d", WithClock(func() time.Time { return now }))
	if _, err := f.Backfill(ctx, BackfillOptions{Symbols: []string{"BTC/USDT"}, Years: 1}); err != nil {
		t.Fatalf("Backfill: %v", err)
	}

	// Exchange "corrects" the last stored candle's close price.
	corrected := make([]domain.Candle, len(ex.candles["BTC/USDT"]))
	copy(corrected, ex.candles["BTC/USDT"])
	corrected[len(corrected)-1].Close = 999999
	corrected[len(corrected)-1].High = 999999
	ex.candles["BTC/USDT"] = corrected
	ex.calls = nil

	if _, err := f.Update(ctx); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if len(ex.calls) == 0 {
		t.Fatal("expected Update to call FetchOHLCV")
	}
	wantSince := start.AddDate(0, 0, 7) // last(day9) - 2 bars = day7: re-fetch days 7,8,9
	if !ex.calls[0].since.Equal(wantSince) {
		t.Fatalf("expected Update to rewind since to %s, got %s", wantSince, ex.calls[0].since)
	}

	got, err := s.GetCandles(ctx, "BTC/USDT", "1d", start.AddDate(0, 0, 9), time.Time{})
	if err != nil {
		t.Fatalf("GetCandles: %v", err)
	}
	if len(got) != 1 || got[0].Close != 999999 {
		t.Fatalf("expected the last candle's close to be overwritten to 999999, got %+v", got)
	}
}

func TestSyncMarketsMarksDelistedWithoutDeleting(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	ex := newFakeExchange()
	ex.markets = []domain.Market{btcMarket(), ethMarket()}
	now := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)

	f := NewFeed(ex, s, "1d", WithClock(func() time.Time { return now }))

	if _, err := f.SyncMarkets(ctx); err != nil {
		t.Fatalf("first SyncMarkets: %v", err)
	}

	// ETH disappears from the exchange's symbol list (delisted).
	ex.markets = []domain.Market{btcMarket()}
	delisted, err := f.SyncMarkets(ctx)
	if err != nil {
		t.Fatalf("second SyncMarkets: %v", err)
	}
	if len(delisted) != 1 || delisted[0] != "ETH/USDT" {
		t.Fatalf("expected ETH/USDT reported delisted, got %v", delisted)
	}

	m, ok, err := s.GetMarket(ctx, "ETH/USDT")
	if err != nil || !ok {
		t.Fatalf("expected ETH/USDT row to still exist (never deleted): ok=%v err=%v", ok, err)
	}
	if m.Active {
		t.Error("expected ETH/USDT to be marked inactive")
	}
	if m.DelistedAt.IsZero() {
		t.Error("expected ETH/USDT delisted_at to be set")
	}

	active, err := s.ListMarkets(ctx, true)
	if err != nil {
		t.Fatalf("ListMarkets: %v", err)
	}
	for _, am := range active {
		if am.Symbol == "ETH/USDT" {
			t.Error("delisted symbol must not appear in the active-only listing")
		}
	}
}

func TestVerifyReportsCriticalIssuesAndSurvivorshipWarning(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	ex := newFakeExchange()
	ex.markets = []domain.Market{btcMarket()}

	start := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start.AddDate(0, 0, 20)

	f := NewFeed(ex, s, "1d", WithClock(func() time.Time { return now }))

	// Clean data first: Verify should be OK.
	if err := s.UpsertCandles(ctx, "BTC/USDT", "1d", dailyCandles(start, 5)); err != nil {
		t.Fatalf("seed candles: %v", err)
	}
	if err := s.UpsertMarket(ctx, store.Market{Symbol: "BTC/USDT", Base: "BTC", Quote: "USDT", Active: true, TickSize: "0.01", StepSize: "0.0001", MinNotional: "10", UpdatedAt: now}); err != nil {
		t.Fatalf("seed market: %v", err)
	}

	report, err := f.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !report.OK || report.CriticalIssues != 0 {
		t.Fatalf("expected clean data to verify OK, got %+v", report)
	}
	foundWarning := false
	for _, w := range report.Warnings {
		if w == survivorshipBiasWarning {
			foundWarning = true
		}
	}
	if !foundWarning {
		t.Fatalf("expected survivorship bias warning in report, got %v", report.Warnings)
	}

	// Now smuggle in an invalid-OHLC row directly (bypassing the normal
	// write path, simulating e.g. a bug or manual DB edit) and confirm
	// Verify catches it as critical.
	bad := domain.Candle{OpenTime: start.AddDate(0, 0, 100), Open: 10, High: 5, Low: 8, Close: 6, Volume: 1}
	if err := s.UpsertCandles(ctx, "BTC/USDT", "1d", []domain.Candle{bad}); err != nil {
		t.Fatalf("seed bad candle: %v", err)
	}

	report2, err := f.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("Verify (with bad candle): %v", err)
	}
	if report2.OK || report2.CriticalIssues == 0 {
		t.Fatalf("expected invalid OHLC candle to be reported as a critical issue, got %+v", report2)
	}
}

func TestUpdateFailsFastOnHardError(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	ex := newFakeExchange()
	ex.markets = []domain.Market{btcMarket()}
	now := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)

	f := NewFeed(ex, s, "1d", WithClock(func() time.Time { return now }))
	if _, err := f.SyncMarkets(ctx); err != nil {
		t.Fatalf("SyncMarkets: %v", err)
	}

	ex.err = context.DeadlineExceeded // simulate a hard exchange failure
	_, err := f.Update(ctx)
	if err == nil {
		t.Fatal("expected Update to return an error rather than silently succeed on a broken fetch (sessiz hata yasak)")
	}
}
