package strategy

import (
	"math"
	"time"

	"swingbot/internal/domain"
)

// monday2024 is a fixed, known Monday (2024-01-01) used as AsOf across
// tests so momentum's weekly-rebalance gate is easy to hit or miss on
// purpose.
var monday2024 = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

// wiggle returns a small deterministic value in roughly [-1, 1], used to
// give synthetic price paths a bit of realistic noise without pulling in
// math/rand (whose determinism guarantees are weaker/version-dependent).
func wiggle(i int) float64 {
	return math.Sin(float64(i) * 1.7)
}

// trendingCloses returns n closes starting at `start` and compounding by
// dailyReturn each bar, with a small deterministic multiplicative wiggle
// of amplitude noiseAmp layered on top. With dailyReturn large enough
// relative to noiseAmp the sequence is "monotonic enough" that the last
// close is the maximum close of any trailing window (needed by
// trendfollow's breakout tests); with dailyReturn == 0 this produces
// sideways, non-trending noise (no long-term drift).
func trendingCloses(n int, start, dailyReturn, noiseAmp float64) []float64 {
	closes := make([]float64, n)
	price := start
	for i := 0; i < n; i++ {
		price *= 1 + dailyReturn
		closes[i] = price * (1 + noiseAmp*wiggle(i))
	}
	return closes
}

// makeCandles turns a slice of closes into a chronological candle series
// ending at `end` (end is the OpenTime of the LAST candle, i.e. the AsOf
// candle in most tests). Open is the previous close (or the first close,
// for bar 0); High/Low bracket [min(open,close), max(open,close)] widened
// by hlSpread on each side, so a bigger hlSpread produces a bigger true
// range (and thus a bigger ATR) without changing the close series at all.
func makeCandles(closes []float64, end time.Time, hlSpread, volume float64) []domain.Candle {
	n := len(closes)
	start := end.AddDate(0, 0, -(n - 1))
	out := make([]domain.Candle, n)
	prevClose := closes[0]
	for i, c := range closes {
		open := prevClose
		hi := math.Max(open, c) * (1 + hlSpread)
		lo := math.Min(open, c) * (1 - hlSpread)
		out[i] = domain.Candle{
			OpenTime:    start.AddDate(0, 0, i),
			Open:        open,
			High:        hi,
			Low:         lo,
			Close:       c,
			Volume:      volume,
			QuoteVolume: c * volume,
		}
		prevClose = c
	}
	return out
}

// findSignal returns the first signal in sigs matching symbol+kind, and
// whether it was found.
func findSignal(sigs []domain.Signal, symbol string, kind domain.SignalKind) (domain.Signal, bool) {
	for _, s := range sigs {
		if s.Symbol == symbol && s.Kind == kind {
			return s, true
		}
	}
	return domain.Signal{}, false
}

// countKind returns how many signals in sigs have the given kind.
func countKind(sigs []domain.Signal, kind domain.SignalKind) int {
	n := 0
	for _, s := range sigs {
		if s.Kind == kind {
			n++
		}
	}
	return n
}
