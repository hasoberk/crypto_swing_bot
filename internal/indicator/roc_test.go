package indicator

import "testing"

func TestROC_KnownValues(t *testing.T) {
	// v = [100, 110, 105, 120, 90], n = 2
	//   ROC[2] = 105/100 - 1 = 0.05
	//   ROC[3] = 120/110 - 1 = 0.090909...
	//   ROC[4] = 90/105  - 1 = -0.142857...
	v := []float64{100, 110, 105, 120, 90}
	want := []float64{nan, nan, 0.05, 120.0/110.0 - 1, 90.0/105.0 - 1}
	requireEqual(t, ROC(v, 2), want)
}

func TestROC_NEqualsOne(t *testing.T) {
	// v = [10, 20, 15], n = 1
	//   ROC[1] = 20/10 - 1 = 1.0
	//   ROC[2] = 15/20 - 1 = -0.25
	v := []float64{10, 20, 15}
	want := []float64{nan, 1.0, -0.25}
	requireEqual(t, ROC(v, 1), want)
}

func TestROC_NGreaterThanLength(t *testing.T) {
	v := []float64{1, 2, 3}
	requireAllNaN(t, ROC(v, 5))
}

func TestROC_NZeroOrNegative(t *testing.T) {
	v := []float64{1, 2, 3}
	requireAllNaN(t, ROC(v, 0))
	requireAllNaN(t, ROC(v, -2))
}

func TestROC_EmptyInput(t *testing.T) {
	got := ROC(nil, 2)
	if len(got) != 0 {
		t.Fatalf("expected empty output, got %v", got)
	}
}
