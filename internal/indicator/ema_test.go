package indicator

import "testing"

func TestEMA_KnownValues(t *testing.T) {
	// v = [22,23,24,25,23,22,21,22,25,27], n = 4, k = 2/(4+1) = 0.4
	// Seed: EMA[3] = mean(v[0..3]) = (22+23+24+25)/4 = 23.5
	// EMA[4] = 23*0.4 + 23.5*0.6 = 9.2 + 14.1   = 23.3
	// EMA[5] = 22*0.4 + 23.3*0.6 = 8.8 + 13.98  = 22.78
	// EMA[6] = 21*0.4 + 22.78*0.6 = 8.4 + 13.668 = 22.068
	// EMA[7] = 22*0.4 + 22.068*0.6 = 8.8 + 13.2408 = 22.0408
	// EMA[8] = 25*0.4 + 22.0408*0.6 = 10 + 13.22448 = 23.22448
	// EMA[9] = 27*0.4 + 23.22448*0.6 = 10.8 + 13.934688 = 24.734688
	v := []float64{22, 23, 24, 25, 23, 22, 21, 22, 25, 27}
	want := []float64{nan, nan, nan, 23.5, 23.3, 22.78, 22.068, 22.0408, 23.22448, 24.734688}
	requireEqual(t, EMA(v, 4), want)
}

func TestEMA_NEqualsOne(t *testing.T) {
	// n=1: k = 2/2 = 1, EMA[i] = v[i]*1 + EMA[i-1]*0, seed EMA[0]=v[0].
	// So EMA degenerates to the input itself.
	v := []float64{5, 2, 8, 1}
	want := []float64{5, 2, 8, 1}
	requireEqual(t, EMA(v, 1), want)
}

func TestEMA_NGreaterThanLength(t *testing.T) {
	v := []float64{1, 2, 3}
	requireAllNaN(t, EMA(v, 5))
}

func TestEMA_NZeroOrNegative(t *testing.T) {
	v := []float64{1, 2, 3}
	requireAllNaN(t, EMA(v, 0))
	requireAllNaN(t, EMA(v, -3))
}

func TestEMA_EmptyInput(t *testing.T) {
	got := EMA(nil, 3)
	if len(got) != 0 {
		t.Fatalf("expected empty output, got %v", got)
	}
}
