package indicator

import "math"

// ZScore computes the cross-sectional z-score of xs: it normalizes the
// values of MULTIPLE symbols at a SINGLE point in time against each
// other, not a time series of a single symbol (SPEC.md Bölüm 6.3, 6.2).
//
//	ZScore[i] = (xs[i] - mean(xs)) / popStdDev(xs)
//
// If xs is empty, an empty slice is returned. If the population standard
// deviation of xs is 0 (fewer than 2 elements, or all elements equal),
// every element's z-score is undefined and math.NaN() is returned for the
// whole slice rather than dividing by zero.
func ZScore(xs []float64) []float64 {
	out := nanSlice(len(xs))
	if len(xs) == 0 {
		return out
	}

	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean := sum / float64(len(xs))

	var sqSum float64
	for _, x := range xs {
		d := x - mean
		sqSum += d * d
	}
	std := math.Sqrt(sqSum / float64(len(xs)))
	if std == 0 {
		return out
	}

	for i, x := range xs {
		out[i] = (x - mean) / std
	}
	return out
}
