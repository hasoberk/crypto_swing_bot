package indicator

import (
	"math"
	"testing"
)

const eps = 1e-9

// requireEqual compares got against want element by element. NaN == NaN is
// treated as a match (that's the whole point of the warmup contract), and
// non-NaN values are compared within a small floating point tolerance.
func requireEqual(t *testing.T, got, want []float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("length mismatch: got %d, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		g, w := got[i], want[i]
		switch {
		case math.IsNaN(w):
			if !math.IsNaN(g) {
				t.Errorf("index %d: got %v, want NaN", i, g)
			}
		case math.IsNaN(g):
			t.Errorf("index %d: got NaN, want %v", i, w)
		case math.Abs(g-w) > eps:
			t.Errorf("index %d: got %v, want %v (diff %v)", i, g, w, math.Abs(g-w))
		}
	}
}

func requireAllNaN(t *testing.T, got []float64) {
	t.Helper()
	for i, g := range got {
		if !math.IsNaN(g) {
			t.Errorf("index %d: got %v, want NaN", i, g)
		}
	}
}

var nan = math.NaN()
