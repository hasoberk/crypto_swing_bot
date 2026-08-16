package indicator

import "testing"

func TestSMA_KnownValues(t *testing.T) {
	// v = [1..10], n = 3
	// hand-computed: SMA[i] = mean(v[i-2:i+1])
	//   i=2: (1+2+3)/3 = 2
	//   i=3: (2+3+4)/3 = 3
	//   i=4: (3+4+5)/3 = 4
	//   ...
	//   i=9: (8+9+10)/3 = 9
	v := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	want := []float64{nan, nan, 2, 3, 4, 5, 6, 7, 8, 9}
	requireEqual(t, SMA(v, 3), want)
}

func TestSMA_SmallHandComputed(t *testing.T) {
	// v = [3, 6, 9], n = 2
	//   i=0: nan (warmup)
	//   i=1: (3+6)/2 = 4.5
	//   i=2: (6+9)/2 = 7.5
	v := []float64{3, 6, 9}
	want := []float64{nan, 4.5, 7.5}
	requireEqual(t, SMA(v, 2), want)
}

func TestSMA_NEqualsOne(t *testing.T) {
	// n=1: SMA equals the input itself, no warmup.
	v := []float64{5, 2, 8, 1}
	want := []float64{5, 2, 8, 1}
	requireEqual(t, SMA(v, 1), want)
}

func TestSMA_NGreaterThanLength(t *testing.T) {
	v := []float64{1, 2, 3}
	requireAllNaN(t, SMA(v, 5))
}

func TestSMA_NZeroOrNegative(t *testing.T) {
	v := []float64{1, 2, 3}
	requireAllNaN(t, SMA(v, 0))
	requireAllNaN(t, SMA(v, -1))
}

func TestSMA_EmptyInput(t *testing.T) {
	got := SMA(nil, 3)
	if len(got) != 0 {
		t.Fatalf("expected empty output, got %v", got)
	}
}

func TestSMA_PreservesLength(t *testing.T) {
	v := make([]float64, 17)
	for i := range v {
		v[i] = float64(i)
	}
	got := SMA(v, 4)
	if len(got) != len(v) {
		t.Fatalf("length mismatch: got %d, want %d", len(got), len(v))
	}
}
