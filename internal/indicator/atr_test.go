package indicator

import (
	"testing"

	"swingbot/internal/domain"
)

func candles(highs, lows, closes []float64) []domain.Candle {
	c := make([]domain.Candle, len(highs))
	for i := range highs {
		c[i] = domain.Candle{High: highs[i], Low: lows[i], Close: closes[i]}
	}
	return c
}

func TestATR_KnownValues(t *testing.T) {
	// High:  [10, 12, 11, 13, 14]
	// Low:   [ 8,  9,  9, 10, 11]
	// Close: [ 9, 11, 10, 12, 13]
	//
	// True range (Wilder):
	//   TR[0] = high[0]-low[0]                                   = 10-8  = 2
	//   TR[1] = max(12-9, |12-9|, |9-9|)   = max(3,3,0)           = 3
	//   TR[2] = max(11-9, |11-11|, |9-11|) = max(2,0,2)           = 2
	//   TR[3] = max(13-10, |13-10|, |10-10|) = max(3,3,0)         = 3
	//   TR[4] = max(14-11, |14-12|, |11-12|) = max(3,2,1)         = 3
	//
	// ATR(n=3), Wilder smoothing:
	//   ATR[2] = mean(TR[0..2]) = (2+3+2)/3           = 2.333333...
	//   ATR[3] = (ATR[2]*2 + TR[3]) / 3 = (4.666667+3)/3 = 2.555556...
	//   ATR[4] = (ATR[3]*2 + TR[4]) / 3 = (5.111111+3)/3 = 2.703704...
	highs := []float64{10, 12, 11, 13, 14}
	lows := []float64{8, 9, 9, 10, 11}
	closes := []float64{9, 11, 10, 12, 13}
	c := candles(highs, lows, closes)

	want := []float64{nan, nan, 7.0 / 3.0, 23.0 / 9.0, 73.0 / 27.0}
	requireEqual(t, ATR(c, 3), want)
}

func TestATR_UsesWilderNotSimpleAverage(t *testing.T) {
	// Regression guard: a naive simple moving average of TR would give
	// ATR[3] = mean(TR[1..3]) = (3+2+3)/3 = 2.666667, which differs from
	// the Wilder-smoothed value of 2.555556. If someone "simplifies" the
	// implementation to a plain SMA of TR, this test must fail.
	highs := []float64{10, 12, 11, 13, 14}
	lows := []float64{8, 9, 9, 10, 11}
	closes := []float64{9, 11, 10, 12, 13}
	c := candles(highs, lows, closes)

	got := ATR(c, 3)
	naiveSMA := 8.0 / 3.0 // (3+2+3)/3
	if diff := got[3] - naiveSMA; diff > -eps && diff < eps {
		t.Fatalf("ATR[3] matches naive SMA-of-TR (%v); Wilder smoothing expected instead", naiveSMA)
	}
}

func TestATR_NGreaterThanLength(t *testing.T) {
	c := candles([]float64{10, 11}, []float64{9, 10}, []float64{9.5, 10.5})
	requireAllNaN(t, ATR(c, 5))
}

func TestATR_NZeroOrNegative(t *testing.T) {
	c := candles([]float64{10, 11}, []float64{9, 10}, []float64{9.5, 10.5})
	requireAllNaN(t, ATR(c, 0))
	requireAllNaN(t, ATR(c, -1))
}

func TestATR_EmptyInput(t *testing.T) {
	got := ATR(nil, 3)
	if len(got) != 0 {
		t.Fatalf("expected empty output, got %v", got)
	}
}
