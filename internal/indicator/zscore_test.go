package indicator

import "testing"

func TestZScore_KnownValues(t *testing.T) {
	// xs = [10, 20, 30, 40], cross-sectional (one point in time, many symbols).
	//   mean = 25
	//   popStdDev = sqrt(((15)^2+(5)^2+(5)^2+(15)^2)/4) = sqrt(500/4) = sqrt(125) = 11.180339887498949
	//   z = (x-25)/11.180339887498949
	xs := []float64{10, 20, 30, 40}
	want := []float64{
		-1.3416407864998738,
		-0.4472135954999579,
		0.4472135954999579,
		1.3416407864998738,
	}
	requireEqual(t, ZScore(xs), want)
}

func TestZScore_SumIsZeroMeanCentered(t *testing.T) {
	// Sanity property independent of the hand-computed values above: a
	// z-scored cross-section is always mean-centered, so the values sum to
	// ~0 regardless of the underlying distribution.
	xs := []float64{3, 17, 9, 42, 1}
	got := ZScore(xs)
	var sum float64
	for _, z := range got {
		sum += z
	}
	if sum > eps || sum < -eps {
		t.Errorf("z-scores should sum to ~0, got %v (%v)", sum, got)
	}
}

func TestZScore_ConstantInputIsNaN(t *testing.T) {
	// All values identical -> zero variance -> z-score undefined. Must be
	// NaN, not a divide-by-zero Inf/NaN silently propagating as 0.
	xs := []float64{7, 7, 7, 7}
	requireAllNaN(t, ZScore(xs))
}

func TestZScore_SingleElementIsNaN(t *testing.T) {
	// A single symbol has no cross-section to compare against: std dev is
	// 0 by definition, so z-score is undefined.
	xs := []float64{42}
	requireAllNaN(t, ZScore(xs))
}

func TestZScore_EmptyInput(t *testing.T) {
	got := ZScore(nil)
	if len(got) != 0 {
		t.Fatalf("expected empty output, got %v", got)
	}
}
