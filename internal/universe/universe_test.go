package universe

import (
	"context"
	"testing"
	"time"

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

// seedMarket + seedCandles are thin wrappers around store.Store's own typed
// writers, matching how internal/datafeed populates the DB (SPEC.md Bölüm
// 6.1) so this test exercises Build against the same shapes production code
// writes.
func seedMarket(t *testing.T, s *store.Store, symbol, base, quote string, listedAt time.Time) {
	t.Helper()
	if err := s.UpsertMarket(context.Background(), store.Market{
		Symbol: symbol, Base: base, Quote: quote, Active: true,
		TickSize: "0.01", StepSize: "0.001", MinNotional: "10",
		ListedAt: listedAt, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("UpsertMarket %s: %v", symbol, err)
	}
}

func seedCandles(t *testing.T, s *store.Store, symbol string, asOf time.Time, n int, quoteVolume float64) {
	t.Helper()
	candles := make([]domain.Candle, n)
	for i := 0; i < n; i++ {
		candles[i] = domain.Candle{
			OpenTime: asOf.AddDate(0, 0, -(n - 1 - i)),
			Open:     100, High: 100, Low: 100, Close: 100,
			Volume: 1, QuoteVolume: quoteVolume,
		}
	}
	if err := s.UpsertCandles(context.Background(), symbol, "1d", candles); err != nil {
		t.Fatalf("UpsertCandles %s: %v", symbol, err)
	}
}

// TestBuild_EndToEnd wires Build against a real (in-memory) store.Store,
// checking that: a healthy USDT market is included and ranked, a leveraged
// token and a too-young market are excluded with the right reasons, and a
// non-USDT market is skipped outright (SPEC.md Bölüm 6.2).
func TestBuild_EndToEnd(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	asOf := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)

	seedMarket(t, s, "BTC/USDT", "BTC", "USDT", asOf.AddDate(0, 0, -400))
	seedCandles(t, s, "BTC/USDT", asOf, 400, 20_000_000)

	seedMarket(t, s, "ETH/USDT", "ETH", "USDT", asOf.AddDate(0, 0, -400))
	seedCandles(t, s, "ETH/USDT", asOf, 400, 15_000_000)

	seedMarket(t, s, "BTCUP/USDT", "BTCUP", "USDT", asOf.AddDate(0, 0, -400))
	seedCandles(t, s, "BTCUP/USDT", asOf, 400, 20_000_000)

	seedMarket(t, s, "NEW/USDT", "NEW", "USDT", asOf.AddDate(0, 0, -30))
	seedCandles(t, s, "NEW/USDT", asOf, 200, 20_000_000)

	seedMarket(t, s, "ETH/BTC", "ETH", "BTC", asOf.AddDate(0, 0, -400))
	seedCandles(t, s, "ETH/BTC", asOf, 400, 20_000_000)

	result, err := Build(ctx, s, "1d", asOf, FilterParams{ExcludeStablecoins: true}, DefaultWeights)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	symbols := result.Symbols()
	if len(symbols) != 2 {
		t.Fatalf("expected 2 symbols in the universe, got %v (excluded=%+v)", symbols, result.Excluded)
	}
	found := map[string]bool{}
	for _, s := range symbols {
		found[s] = true
	}
	if !found["BTC/USDT"] || !found["ETH/USDT"] {
		t.Fatalf("expected BTC/USDT and ETH/USDT in the universe, got %v", symbols)
	}

	reasons := map[string]string{}
	for _, e := range result.Excluded {
		reasons[e.Symbol] = e.Reason
	}
	if reasons["BTCUP/USDT"] != ReasonLeveragedToken {
		t.Errorf("BTCUP/USDT reason = %q, want %q", reasons["BTCUP/USDT"], ReasonLeveragedToken)
	}
	if reasons["NEW/USDT"] != ReasonTooYoung {
		t.Errorf("NEW/USDT reason = %q, want %q", reasons["NEW/USDT"], ReasonTooYoung)
	}
	if _, ok := reasons["ETH/BTC"]; ok {
		t.Errorf("ETH/BTC (wrong quote) should be skipped outright, not reported as an exclusion: %+v", result.Excluded)
	}
}

func TestBuild_RespectsMaxSymbolsCap(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	asOf := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)

	symbols := []string{"AAA/USDT", "BBB/USDT", "CCC/USDT"}
	for i, sym := range symbols {
		base := sym[:3]
		seedMarket(t, s, sym, base, "USDT", asOf.AddDate(0, 0, -400))
		seedCandles(t, s, sym, asOf, 400, 10_000_000+float64(i)*1_000_000)
	}

	result, err := Build(ctx, s, "1d", asOf, FilterParams{MaxSymbols: 2}, DefaultWeights)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(result.Universe) != 2 {
		t.Fatalf("expected universe capped to 2, got %d: %+v", len(result.Universe), result.Universe)
	}
	var capped bool
	for _, e := range result.Excluded {
		if e.Reason == ReasonCappedByMaxSymbols {
			capped = true
		}
	}
	if !capped {
		t.Fatalf("expected one symbol excluded with ReasonCappedByMaxSymbols, got %+v", result.Excluded)
	}
}
