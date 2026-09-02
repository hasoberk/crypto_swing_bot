package store

import (
	"context"
	"testing"
	"time"

	"swingbot/internal/domain"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustFloat(f float64) *float64 { return &f }

func TestOpenAppliesSchema(t *testing.T) {
	s := openTestStore(t)
	tables := []string{"markets", "candles", "proposals", "orders", "trades", "equity_snapshots", "runs", "system_state"}
	for _, tbl := range tables {
		var name string
		err := s.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name = ?`, tbl).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing: %v", tbl, err)
		}
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	// Re-running migrations against an already-migrated DB (simulated here
	// by opening twice against the same file) must not error.
	dir := t.TempDir()
	path := dir + "/swingbot.db"

	s1, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	s1.Close()

	s2, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	s2.Close()
}

func TestSystemStateRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	_, ok, err := s.GetState(ctx, "breaker")
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if ok {
		t.Fatalf("expected missing key to report ok=false")
	}

	if err := s.SetState(ctx, "breaker", "open"); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	val, ok, err := s.GetState(ctx, "breaker")
	if err != nil || !ok || val != "open" {
		t.Fatalf("GetState after set: val=%q ok=%v err=%v", val, ok, err)
	}

	if err := s.SetState(ctx, "breaker", "closed"); err != nil {
		t.Fatalf("SetState overwrite: %v", err)
	}
	val, _, _ = s.GetState(ctx, "breaker")
	if val != "closed" {
		t.Fatalf("expected overwrite to stick, got %q", val)
	}
}

func TestMarketsUpsertAndDelist(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	m := Market{
		Symbol: "BTC/USDT", Base: "BTC", Quote: "USDT", Active: true,
		TickSize: "0.01", StepSize: "0.0001", MinNotional: "10",
		UpdatedAt: time.Now().UTC(),
	}
	if err := s.UpsertMarket(ctx, m); err != nil {
		t.Fatalf("UpsertMarket: %v", err)
	}

	got, ok, err := s.GetMarket(ctx, "BTC/USDT")
	if err != nil || !ok {
		t.Fatalf("GetMarket: ok=%v err=%v", ok, err)
	}
	if !got.Active || got.TickSize != "0.01" {
		t.Fatalf("unexpected market row: %+v", got)
	}

	delistTime := time.Now().UTC()
	if err := s.MarkDelisted(ctx, "BTC/USDT", delistTime); err != nil {
		t.Fatalf("MarkDelisted: %v", err)
	}
	got, _, _ = s.GetMarket(ctx, "BTC/USDT")
	if got.Active {
		t.Fatalf("expected market to be inactive after delist")
	}
	if got.DelistedAt.IsZero() {
		t.Fatalf("expected delisted_at to be set")
	}

	// Delisting must never remove the row (survivorship bias, SPEC.md 6.1).
	all, err := s.ListMarkets(ctx, false)
	if err != nil || len(all) != 1 {
		t.Fatalf("expected delisted market to remain in table, got %d rows, err=%v", len(all), err)
	}
	active, err := s.ListMarkets(ctx, true)
	if err != nil || len(active) != 0 {
		t.Fatalf("expected 0 active markets after delist, got %d, err=%v", len(active), err)
	}
}

func TestCandlesUpsertIsIdempotent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	candles := []domain.Candle{
		{OpenTime: base, Open: 1, High: 2, Low: 0.5, Close: 1.5, Volume: 100, QuoteVolume: 150},
		{OpenTime: base.AddDate(0, 0, 1), Open: 1.5, High: 2.5, Low: 1, Close: 2, Volume: 110, QuoteVolume: 220},
	}

	if err := s.UpsertCandles(ctx, "BTC/USDT", "1d", candles); err != nil {
		t.Fatalf("UpsertCandles: %v", err)
	}
	n, err := s.CountCandles(ctx, "BTC/USDT", "1d")
	if err != nil || n != 2 {
		t.Fatalf("CountCandles after first insert: n=%d err=%v", n, err)
	}

	// Re-run the same backfill page: row count must not grow (İ5-style
	// idempotency, storage-engineer kabul kriteri), but a changed value
	// must be reflected (e.g. an exchange revising a not-yet-final bar).
	candles[0].Close = 1.9
	if err := s.UpsertCandles(ctx, "BTC/USDT", "1d", candles); err != nil {
		t.Fatalf("UpsertCandles (rerun): %v", err)
	}
	n, err = s.CountCandles(ctx, "BTC/USDT", "1d")
	if err != nil || n != 2 {
		t.Fatalf("CountCandles after rerun: n=%d err=%v (expected no growth)", n, err)
	}

	max, ok, err := s.MaxOpenTime(ctx, "BTC/USDT", "1d")
	if err != nil || !ok || !max.Equal(base.AddDate(0, 0, 1)) {
		t.Fatalf("MaxOpenTime: max=%v ok=%v err=%v", max, ok, err)
	}

	got, err := s.GetCandles(ctx, "BTC/USDT", "1d", base, time.Time{})
	if err != nil || len(got) != 2 {
		t.Fatalf("GetCandles: len=%d err=%v", len(got), err)
	}
	if got[0].Close != 1.9 {
		t.Fatalf("expected upsert to overwrite close price, got %v", got[0].Close)
	}
}

// TestGetCandlesForSymbols checks the batched multi-symbol reader against
// GetCandles called once per symbol: same data, one round trip instead of
// N, and a symbol with no rows is simply absent (not an error, not an
// empty-slice entry).
func TestGetCandlesForSymbols(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	btc := []domain.Candle{
		{OpenTime: base, Open: 1, High: 2, Low: 0.5, Close: 1.5, Volume: 100, QuoteVolume: 150},
		{OpenTime: base.AddDate(0, 0, 1), Open: 1.5, High: 2.5, Low: 1, Close: 2, Volume: 110, QuoteVolume: 220},
	}
	eth := []domain.Candle{
		{OpenTime: base, Open: 10, High: 12, Low: 9, Close: 11, Volume: 50, QuoteVolume: 550},
	}
	if err := s.UpsertCandles(ctx, "BTC/USDT", "1d", btc); err != nil {
		t.Fatalf("UpsertCandles BTC: %v", err)
	}
	if err := s.UpsertCandles(ctx, "ETH/USDT", "1d", eth); err != nil {
		t.Fatalf("UpsertCandles ETH: %v", err)
	}

	got, err := s.GetCandlesForSymbols(ctx, []string{"BTC/USDT", "ETH/USDT", "DOGE/USDT"}, "1d", base, time.Time{})
	if err != nil {
		t.Fatalf("GetCandlesForSymbols: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 symbols in result (DOGE/USDT has no rows), got %d: %v", len(got), got)
	}
	if len(got["BTC/USDT"]) != 2 || got["BTC/USDT"][0].Close != 1.5 || got["BTC/USDT"][1].Close != 2 {
		t.Errorf("BTC/USDT candles = %+v, want the 2 upserted above in order", got["BTC/USDT"])
	}
	if len(got["ETH/USDT"]) != 1 || got["ETH/USDT"][0].Close != 11 {
		t.Errorf("ETH/USDT candles = %+v, want the 1 upserted above", got["ETH/USDT"])
	}
	if _, present := got["DOGE/USDT"]; present {
		t.Errorf("DOGE/USDT has no rows and should be absent from the map, not present with an empty slice")
	}
}

func TestProposalLifecycle(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	p := Proposal{
		ID: "prop-1", CreatedAt: now, AsOf: now, Symbol: "BTC/USDT", Side: "long", Strategy: "momentum",
		Score: mustFloat(0.8), RefPrice: 50000, StopPrice: mustFloat(48000), Qty: "0.01",
		RiskAmount: 20, Reason: "test", MetricsJSON: "{}", Status: ProposalPending,
		ExpiresAt: now.Add(24 * time.Hour),
	}
	if err := s.InsertProposal(ctx, p); err != nil {
		t.Fatalf("InsertProposal: %v", err)
	}

	pending, err := s.ListProposalsByStatus(ctx, ProposalPending)
	if err != nil || len(pending) != 1 {
		t.Fatalf("ListProposalsByStatus(PENDING): len=%d err=%v", len(pending), err)
	}

	decided := now.Add(time.Minute)
	if err := s.UpdateProposalStatus(ctx, "prop-1", ProposalApproved, decided, ""); err != nil {
		t.Fatalf("UpdateProposalStatus: %v", err)
	}
	got, ok, err := s.GetProposal(ctx, "prop-1")
	if err != nil || !ok {
		t.Fatalf("GetProposal: ok=%v err=%v", ok, err)
	}
	if got.Status != ProposalApproved || got.DecidedAt.IsZero() {
		t.Fatalf("unexpected proposal after status update: %+v", got)
	}

	if err := s.UpdateProposalStatus(ctx, "prop-1", ProposalSubmitted, time.Time{}, "order-1"); err != nil {
		t.Fatalf("UpdateProposalStatus (order): %v", err)
	}
	got, _, _ = s.GetProposal(ctx, "prop-1")
	if got.Status != ProposalSubmitted || got.OrderID != "order-1" {
		t.Fatalf("unexpected proposal after order attach: %+v", got)
	}
	// decided_at must have been preserved (COALESCE), not wiped by the zero
	// value passed in this second update.
	if got.DecidedAt.IsZero() {
		t.Fatalf("expected decided_at to be preserved across status update")
	}
}

func TestOrdersIdempotencyByClientID(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	o := Order{
		ID: "ex-order-1", ClientOrderID: "coid-1", Symbol: "BTC/USDT", Side: "buy", Type: "market",
		Qty: "0.01", Status: "open", FilledQty: "0", CreatedAt: now, UpdatedAt: now,
	}
	if err := s.InsertOrder(ctx, o); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}

	// A retried CreateOrder call reuses the same ClientOrderID; a caller
	// checks GetOrderByClientID first and must find the original order.
	got, ok, err := s.GetOrderByClientID(ctx, "coid-1")
	if err != nil || !ok || got.ID != "ex-order-1" {
		t.Fatalf("GetOrderByClientID: got=%+v ok=%v err=%v", got, ok, err)
	}

	// The UNIQUE constraint on client_order_id must reject a genuine
	// duplicate insert (a caller that ignored GetOrderByClientID).
	dup := o
	dup.ID = "ex-order-2"
	if err := s.InsertOrder(ctx, dup); err == nil {
		t.Fatalf("expected duplicate client_order_id to be rejected")
	}

	if err := s.UpdateOrderFill(ctx, "ex-order-1", "filled", "0.01", "50000", "0.5", `{"raw":true}`); err != nil {
		t.Fatalf("UpdateOrderFill: %v", err)
	}
	got, _, _ = s.GetOrderByClientID(ctx, "coid-1")
	if got.Status != "filled" || got.FilledQty != "0.01" || got.AvgPrice != "50000" {
		t.Fatalf("unexpected order after fill update: %+v", got)
	}
}

func TestTradeRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	entry := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tr := Trade{
		ID: "trade-1", Symbol: "BTC/USDT", Strategy: "momentum",
		EntryTime: entry, EntryPrice: 50000, Qty: "0.01", Fees: 1, Mode: "backtest",
	}
	if err := s.InsertTrade(ctx, tr); err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}

	open, err := s.ListTrades(ctx, "backtest", "")
	if err != nil || len(open) != 1 || !open[0].ExitTime.IsZero() {
		t.Fatalf("ListTrades before close: %+v err=%v", open, err)
	}

	exit := entry.AddDate(0, 0, 5)
	if err := s.CloseTrade(ctx, "trade-1", exit, 55000, 50, 1.0, 2, "signal"); err != nil {
		t.Fatalf("CloseTrade: %v", err)
	}

	closed, err := s.ListTrades(ctx, "backtest", "BTC/USDT")
	if err != nil || len(closed) != 1 {
		t.Fatalf("ListTrades after close: %+v err=%v", closed, err)
	}
	if closed[0].ExitTime.IsZero() || closed[0].ExitReason != "signal" {
		t.Fatalf("unexpected closed trade: %+v", closed[0])
	}
}

func TestEquitySnapshotUpsert(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e := EquitySnapshot{Mode: "paper", TS: ts, Equity: 10000, Cash: 5000, Exposure: 0.5, BenchBTC: mustFloat(10100)}
	if err := s.InsertEquitySnapshot(ctx, e); err != nil {
		t.Fatalf("InsertEquitySnapshot: %v", err)
	}

	e.Equity = 10500
	if err := s.InsertEquitySnapshot(ctx, e); err != nil {
		t.Fatalf("InsertEquitySnapshot (upsert): %v", err)
	}

	snaps, err := s.ListEquitySnapshots(ctx, "paper")
	if err != nil || len(snaps) != 1 {
		t.Fatalf("ListEquitySnapshots: len=%d err=%v (expected upsert, not duplicate row)", len(snaps), err)
	}
	if snaps[0].Equity != 10500 {
		t.Fatalf("expected upsert to overwrite equity, got %v", snaps[0].Equity)
	}
}

func TestRunRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	r := Run{
		ID: "run-1", CreatedAt: now, Strategy: "momentum", ParamsJSON: "{}",
		StartTS: now.AddDate(-1, 0, 0), EndTS: now, CostsJSON: "{}", MetricsJSON: `{"sharpe":1.2}`,
		GitSHA: "abc123",
	}
	if err := s.InsertRun(ctx, r); err != nil {
		t.Fatalf("InsertRun: %v", err)
	}

	got, ok, err := s.GetRun(ctx, "run-1")
	if err != nil || !ok || got.GitSHA != "abc123" {
		t.Fatalf("GetRun: got=%+v ok=%v err=%v", got, ok, err)
	}

	all, err := s.ListRuns(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("ListRuns: len=%d err=%v", len(all), err)
	}
}
