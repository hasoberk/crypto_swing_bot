package indicator

// EMA computes the exponential moving average of v with period n.
//
// The series is seeded with a simple average of the first n values (a
// common, well-defined convention): EMA[n-1] = mean(v[0..n-1]). From there
// EMA[i] = v[i]*k + EMA[i-1]*(1-k), with k = 2/(n+1).
//
// For i < n-1, or when n <= 0, the result is math.NaN().
func EMA(v []float64, n int) []float64 {
	out := nanSlice(len(v))
	if n <= 0 || len(v) < n {
		return out
	}

	k := 2.0 / float64(n+1)

	var sum float64
	for i := 0; i < n; i++ {
		sum += v[i]
	}
	out[n-1] = sum / float64(n)

	for i := n; i < len(v); i++ {
		out[i] = v[i]*k + out[i-1]*(1-k)
	}
	return out
}
