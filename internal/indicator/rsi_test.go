package indicator

import (
	"math"
	"testing"
)

func TestRSI_KnownValues(t *testing.T) {
	// Classic Wilder RSI textbook example, n=14.
	// deltas[1..14] seed avgGain/avgLoss with a simple mean:
	//   avgGain = 0.23857142857142838
	//   avgLoss = 0.09999999999999991
	//   RS = avgGain/avgLoss, RSI[14] = 100 - 100/(1+RS) = 70.46413502109705
	prices := []float64{
		44.34, 44.09, 44.15, 43.61, 44.33, 44.83, 45.10, 45.42,
		45.84, 46.08, 45.89, 46.03, 45.61, 46.28, 46.28,
	}
	got := RSI(prices, 14)
	for i := 0; i < 14; i++ {
		if !math.IsNaN(got[i]) {
			t.Errorf("index %d: got %v, want NaN (warmup)", i, got[i])
		}
	}
	want14 := 70.46413502109705
	if diff := got[14] - want14; diff > eps || diff < -eps {
		t.Errorf("RSI[14]: got %v, want %v", got[14], want14)
	}
}

func TestRSI_WilderContinuation(t *testing.T) {
	// Same series as TestRSI_KnownValues, with one more (rising) close
	// appended, to exercise the Wilder recursion step past the seed:
	//   RSI[14] = 70.46413502109705 (seed, same as above)
	//   RSI[15] = 74.61645746164574 (one Wilder-smoothed step forward)
	prices := []float64{
		44.34, 44.09, 44.15, 43.61, 44.33, 44.83, 45.10, 45.42,
		45.84, 46.08, 45.89, 46.03, 45.61, 46.28, 46.28, 47.00,
	}
	got := RSI(prices, 14)
	want := []float64{
		nan, nan, nan, nan, nan, nan, nan, nan, nan, nan, nan, nan, nan, nan,
		70.46413502109705, 74.61645746164574,
	}
	requireEqual(t, got, want)
}

func TestRSI_AllGainsIsHundred(t *testing.T) {
	// Strictly increasing series: every delta is a gain, avgLoss stays 0,
	// so RSI must be exactly 100 (never divide by zero).
	v := make([]float64, 16)
	for i := range v {
		v[i] = float64(i + 1)
	}
	got := RSI(v, 14)
	if got[14] != 100 {
		t.Errorf("RSI[14]: got %v, want 100", got[14])
	}
	if got[15] != 100 {
		t.Errorf("RSI[15]: got %v, want 100", got[15])
	}
}

func TestRSI_FlatSeriesIsFifty(t *testing.T) {
	// No movement at all: avgGain == avgLoss == 0. Defined as neutral (50),
	// not NaN and not a divide-by-zero panic.
	v := make([]float64, 16)
	for i := range v {
		v[i] = 100
	}
	got := RSI(v, 14)
	if got[14] != 50 {
		t.Errorf("RSI[14]: got %v, want 50", got[14])
	}
}

func TestRSI_NotEnoughData(t *testing.T) {
	v := []float64{1, 2, 3}
	requireAllNaN(t, RSI(v, 14))
}

func TestRSI_NZeroOrNegative(t *testing.T) {
	v := []float64{1, 2, 3, 4, 5}
	requireAllNaN(t, RSI(v, 0))
	requireAllNaN(t, RSI(v, -1))
}

func TestRSI_EmptyInput(t *testing.T) {
	got := RSI(nil, 14)
	if len(got) != 0 {
		t.Fatalf("expected empty output, got %v", got)
	}
}
