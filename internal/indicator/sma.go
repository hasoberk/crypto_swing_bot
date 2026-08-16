package indicator

// SMA computes the simple moving average of v over a window of n.
//
// SMA[i] is the arithmetic mean of v[i-n+1 .. i]. For i < n-1 (not enough
// history yet), or when n <= 0, the result is math.NaN().
func SMA(v []float64, n int) []float64 {
	out := nanSlice(len(v))
	if n <= 0 || len(v) == 0 {
		return out
	}

	var sum float64
	for i := range v {
		sum += v[i]
		if i >= n {
			sum -= v[i-n]
		}
		if i >= n-1 {
			out[i] = sum / float64(n)
		}
	}
	return out
}
