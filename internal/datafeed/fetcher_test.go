package datafeed

import (
	"context"
	"errors"
	"testing"
	"time"

	"swingbot/internal/domain"
)

func TestParseTimeframe(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"1d", 24 * time.Hour, false},
		{"4h", 4 * time.Hour, false},
		{"15m", 15 * time.Minute, false},
		{"1w", 7 * 24 * time.Hour, false},
		{"", 0, true},
		{"d", 0, true},
		{"0d", 0, true},
		{"1x", 0, true},
		{"abc", 0, true},
	}
	for _, c := range cases {
		got, err := ParseTimeframe(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseTimeframe(%q): expected error, got nil", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseTimeframe(%q): unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseTimeframe(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestIsClosed(t *testing.T) {
	tf := 24 * time.Hour
	open := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)

	// Exactly closed: open+24h == now.
	if !IsClosed(open, tf, open.Add(24*time.Hour)) {
		t.Error("expected open_time+tf == now to count as closed")
	}
	// Closed with margin.
	if !IsClosed(open, tf, open.Add(25*time.Hour)) {
		t.Error("expected open_time+tf < now to count as closed")
	}
	// Not yet closed.
	if IsClosed(open, tf, open.Add(23*time.Hour)) {
		t.Error("expected open_time+tf > now to count as NOT closed")
	}
}

func TestFetchPagesAdvancesAndStopsOnShortPage(t *testing.T) {
	ex := newFakeExchange()
	start := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start.AddDate(0, 0, 25) // 25 days later
	ex.candles["BTC/USDT"] = dailyCandles(start, 20)

	var pages [][]domain.Candle
	err := FetchPages(context.Background(), ex, "BTC/USDT", "1d", start, 8, func() time.Time { return now }, func(p []domain.Candle) error {
		pages = append(pages, p)
		return nil
	})
	if err != nil {
		t.Fatalf("FetchPages: %v", err)
	}
	if len(pages) != 3 { // 8 + 8 + 4
		t.Fatalf("expected 3 pages, got %d", len(pages))
	}
	if len(pages[0]) != 8 || len(pages[1]) != 8 || len(pages[2]) != 4 {
		t.Fatalf("unexpected page sizes: %v", []int{len(pages[0]), len(pages[1]), len(pages[2])})
	}

	// since parameter of successive calls must advance monotonically.
	if len(ex.calls) != 3 {
		t.Fatalf("expected 3 exchange calls, got %d", len(ex.calls))
	}
	for i := 1; i < len(ex.calls); i++ {
		if !ex.calls[i].since.After(ex.calls[i-1].since) {
			t.Fatalf("call %d since=%s did not advance past call %d since=%s", i, ex.calls[i].since, i-1, ex.calls[i-1].since)
		}
	}
}

func TestFetchPagesStopsWhenCursorReachesNow(t *testing.T) {
	ex := newFakeExchange()
	start := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start // cursor starts already at "now": nothing to fetch.

	called := false
	err := FetchPages(context.Background(), ex, "BTC/USDT", "1d", start, 500, func() time.Time { return now }, func(p []domain.Candle) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("FetchPages: %v", err)
	}
	if called {
		t.Error("onPage should not be called when cursor is already at/after now")
	}
	if len(ex.calls) != 0 {
		t.Errorf("expected zero exchange calls, got %d", len(ex.calls))
	}
}

func TestFetchPagesStopsOnEmptyResponse(t *testing.T) {
	ex := newFakeExchange() // no candles seeded for the symbol
	start := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start.AddDate(1, 0, 0)

	pages := 0
	err := FetchPages(context.Background(), ex, "GHOST/USDT", "1d", start, 500, func() time.Time { return now }, func(p []domain.Candle) error {
		pages++
		return nil
	})
	if err != nil {
		t.Fatalf("FetchPages: %v", err)
	}
	if pages != 0 {
		t.Errorf("expected zero pages for a symbol with no data, got %d", pages)
	}
}

func TestFetchPagesPropagatesExchangeError(t *testing.T) {
	ex := newFakeExchange()
	ex.err = errors.New("boom")
	start := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start.AddDate(1, 0, 0)

	err := FetchPages(context.Background(), ex, "BTC/USDT", "1d", start, 500, func() time.Time { return now }, func(p []domain.Candle) error { return nil })
	if err == nil {
		t.Fatal("expected error to propagate from FetchOHLCV")
	}
}

func TestFetchPagesPropagatesOnPageError(t *testing.T) {
	ex := newFakeExchange()
	start := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start.AddDate(0, 0, 10)
	ex.candles["BTC/USDT"] = dailyCandles(start, 5)

	sentinel := errors.New("store failed")
	err := FetchPages(context.Background(), ex, "BTC/USDT", "1d", start, 500, func() time.Time { return now }, func(p []domain.Candle) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error to propagate, got %v", err)
	}
}
