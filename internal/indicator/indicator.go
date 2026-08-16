// Package indicator implements the technical indicators used by the
// strategy layer (SPEC.md Bölüm 6.3).
//
// Binding rules for every function in this package:
//   - Pure functions only. Input []float64 or []domain.Candle, output
//     []float64. No side effects, no global state, no file/network access.
//   - During warmup (not enough data yet to compute a value) the function
//     returns math.NaN(), never zero. Returning zero silently corrupts
//     downstream logic (e.g. a strategy reading ATR=0 and computing a
//     zero-width stop).
//   - The output slice always has the same length as the (longest) input
//     slice, so callers can index output[i] against input[i] directly.
package indicator

import "math"

// nanSlice returns a new []float64 of length n filled with math.NaN().
func nanSlice(n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = math.NaN()
	}
	return out
}
