package indicator

import "swingbot/internal/domain"

// trueRange computes the per-bar true range series for c.
//
// TR[i] = max(high[i]-low[i], |high[i]-close[i-1]|, |low[i]-close[i-1]|)
// for i >= 1. TR[0] has no previous close to compare against, so it falls
// back to high[0]-low[0] (the standard convention for the first bar).
func trueRange(c []domain.Candle) []float64 {
	tr := make([]float64, len(c))
	for i := range c {
		hl := c[i].High - c[i].Low
		if i == 0 {
			tr[i] = hl
			continue
		}
		hc := absF(c[i].High - c[i-1].Close)
		lc := absF(c[i].Low - c[i-1].Close)
		tr[i] = maxF(hl, maxF(hc, lc))
	}
	return tr
}

func absF(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// ATR computes the Average True Range of c over period n using Wilder's
// smoothing method (SPEC.md Bölüm 6.3 — a plain moving average of the true
// range is explicitly NOT sufficient).
//
// The first available value, ATR[n-1], is the simple average of the first
// n true-range values. From there Wilder smoothing applies:
//
//	ATR[i] = (ATR[i-1]*(n-1) + TR[i]) / n
//
// For i < n-1, or when n <= 0, or len(c) == 0, the result is math.NaN().
func ATR(c []domain.Candle, n int) []float64 {
	out := nanSlice(len(c))
	if n <= 0 || len(c) < n {
		return out
	}

	tr := trueRange(c)

	var sum float64
	for i := 0; i < n; i++ {
		sum += tr[i]
	}
	out[n-1] = sum / float64(n)

	for i := n; i < len(c); i++ {
		out[i] = (out[i-1]*float64(n-1) + tr[i]) / float64(n)
	}
	return out
}
