package indicator

// RSI computes the Relative Strength Index of v over period n using
// Wilder's smoothing method (the same smoothing ATR uses).
//
// The first n price changes (v[1]-v[0] .. v[n]-v[n-1]) seed the average
// gain/loss with a simple mean, producing the first RSI value at index n.
// From there:
//
//	avgGain[i] = (avgGain[i-1]*(n-1) + gain[i]) / n
//	avgLoss[i] = (avgLoss[i-1]*(n-1) + loss[i]) / n
//	RS         = avgGain[i] / avgLoss[i]
//	RSI[i]     = 100 - 100/(1+RS)
//
// If avgLoss is 0 (all gains, no losses in the smoothed window), RSI is
// defined as 100. For i < n, or when n <= 0, the result is math.NaN().
func RSI(v []float64, n int) []float64 {
	out := nanSlice(len(v))
	if n <= 0 || len(v) < n+1 {
		return out
	}

	gain := func(delta float64) float64 {
		if delta > 0 {
			return delta
		}
		return 0
	}
	loss := func(delta float64) float64 {
		if delta < 0 {
			return -delta
		}
		return 0
	}

	var sumGain, sumLoss float64
	for i := 1; i <= n; i++ {
		d := v[i] - v[i-1]
		sumGain += gain(d)
		sumLoss += loss(d)
	}
	avgGain := sumGain / float64(n)
	avgLoss := sumLoss / float64(n)
	out[n] = rsiFromAvg(avgGain, avgLoss)

	for i := n + 1; i < len(v); i++ {
		d := v[i] - v[i-1]
		avgGain = (avgGain*float64(n-1) + gain(d)) / float64(n)
		avgLoss = (avgLoss*float64(n-1) + loss(d)) / float64(n)
		out[i] = rsiFromAvg(avgGain, avgLoss)
	}
	return out
}

func rsiFromAvg(avgGain, avgLoss float64) float64 {
	if avgLoss == 0 {
		if avgGain == 0 {
			// No movement at all: neither overbought nor oversold.
			return 50
		}
		return 100
	}
	rs := avgGain / avgLoss
	return 100 - 100/(1+rs)
}
