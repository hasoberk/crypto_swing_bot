package indicator

// ROC computes the n-period rate of change of v:
//
//	ROC[i] = v[i]/v[i-n] - 1
//
// For i < n, or when n <= 0, the result is math.NaN().
func ROC(v []float64, n int) []float64 {
	out := nanSlice(len(v))
	if n <= 0 {
		return out
	}

	for i := n; i < len(v); i++ {
		out[i] = v[i]/v[i-n] - 1
	}
	return out
}
