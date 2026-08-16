package indicator

import (
	"math"
	"testing"
)

func TestStdDev_KnownValues(t *testing.T) {
	// v = [2, 4, 4, 4, 5, 5, 7, 9], n = 3 (population std dev)
	//   window [2,4,4]: mean=10/3, sqSum/3 = 8/9  -> sqrt(8/9)  = 0.942809...
	//   window [4,4,4]: mean=4,    sqSum/3 = 0    -> 0
	//   window [4,4,5]: mean=13/3, sqSum/3 = 2/9  -> sqrt(2/9)  = 0.471405...
	//   window [4,5,5]: mean=14/3, sqSum/3 = 2/9  -> sqrt(2/9)  = 0.471405...
	//   window [5,5,7]: mean=17/3, sqSum/3 = 8/9  -> sqrt(8/9)  = 0.942809...
	//   window [5,7,9]: mean=7,    sqSum/3 = 8/3  -> sqrt(8/3)  = 1.632993...
	v := []float64{2, 4, 4, 4, 5, 5, 7, 9}
	want := []float64{
		nan, nan,
		math.Sqrt(8.0 / 9.0),
		0,
		math.Sqrt(2.0 / 9.0),
		math.Sqrt(2.0 / 9.0),
		math.Sqrt(8.0 / 9.0),
		math.Sqrt(8.0 / 3.0),
	}
	requireEqual(t, StdDev(v, 3), want)
}

func TestStdDev_ConstantWindowIsZero(t *testing.T) {
	v := []float64{5, 5, 5, 5}
	want := []float64{nan, nan, nan, 0}
	requireEqual(t, StdDev(v, 4), want)
}

func TestStdDev_NGreaterThanLength(t *testing.T) {
	v := []float64{1, 2, 3}
	requireAllNaN(t, StdDev(v, 5))
}

func TestStdDev_NZeroOrNegative(t *testing.T) {
	v := []float64{1, 2, 3}
	requireAllNaN(t, StdDev(v, 0))
	requireAllNaN(t, StdDev(v, -2))
}

func TestStdDev_EmptyInput(t *testing.T) {
	got := StdDev(nil, 3)
	if len(got) != 0 {
		t.Fatalf("expected empty output, got %v", got)
	}
}
