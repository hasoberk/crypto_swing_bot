package indicator

import "math"

// StdDev computes the rolling population standard deviation of v over a
// window of n:
//
//	StdDev[i] = sqrt( mean( (v[j]-mean)^2 ) )   for j in [i-n+1, i]
//
// The population (n-divisor, not n-1) convention is used, matching the
// common technical-indicator definition (e.g. Bollinger Bands).
//
// For i < n-1, or when n <= 0, the result is math.NaN().
func StdDev(v []float64, n int) []float64 {
	out := nanSlice(len(v))
	if n <= 0 || len(v) < n {
		return out
	}

	for i := n - 1; i < len(v); i++ {
		window := v[i-n+1 : i+1]
		var sum float64
		for _, x := range window {
			sum += x
		}
		mean := sum / float64(n)

		var sqSum float64
		for _, x := range window {
			d := x - mean
			sqSum += d * d
		}
		out[i] = math.Sqrt(sqSum / float64(n))
	}
	return out
}
