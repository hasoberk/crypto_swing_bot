package universe

import (
	"math"
	"sort"

	"swingbot/internal/domain"
	"swingbot/internal/indicator"
)

// Weights are the score's per-component multipliers, SPEC.md Bölüm 6.2:
//
//	score = w1*z(mom_90) + w2*z(mom_30) - w3*z(vol_30) + w4*z(liq)
//
// They come from config.yaml's strategy.momentum.weights and MUST NOT be
// tuned inside this package — SPEC.md Bölüm 14's overfitting warning names
// "az parametreyi sürekli ayarlama" explicitly, and this file's job is to
// apply whatever weights the caller passes, not to search for better ones.
type Weights struct {
	Mom90 float64
	Mom30 float64
	Vol30 float64 // applied with a NEGATIVE sign in Score — see ScoreComponents.Vol30 doc comment
	Liq   float64
}

// DefaultWeights mirrors config.example.yaml's strategy.momentum.weights
// (SPEC.md Bölüm 8): { mom_90: 0.4, mom_30: 0.3, vol_30: 0.2, liq: 0.1 }.
// WeightsFromMap falls back to these per-key when config's map omits a key
// entirely (e.g. a config.yaml written before a new component existed);
// it is a documented default, not a tuned one.
var DefaultWeights = Weights{Mom90: 0.4, Mom30: 0.3, Vol30: 0.2, Liq: 0.1}

// WeightsFromMap converts config's strategy.momentum.weights map (keyed
// "mom_90", "mom_30", "vol_30", "liq") into a Weights, starting from
// DefaultWeights and overriding only the keys present in m.
func WeightsFromMap(m map[string]float64) Weights {
	w := DefaultWeights
	if v, ok := m["mom_90"]; ok {
		w.Mom90 = v
	}
	if v, ok := m["mom_30"]; ok {
		w.Mom30 = v
	}
	if v, ok := m["vol_30"]; ok {
		w.Vol30 = v
	}
	if v, ok := m["liq"]; ok {
		w.Liq = v
	}
	return w
}

// ScoreComponents are every value that went into one symbol's score, kept
// separately (both raw and cross-sectional z-score form) so a caller can
// serialize them into metrics_json verbatim — SPEC.md İ6 / Bölüm 6.2:
// "Skor bileşenlerini ayrı ayrı sakla ... panelde neden o coinin seçildiğini
// görebilesin."
type ScoreComponents struct {
	Mom90               float64 // raw 90-day return
	Mom30               float64 // raw 30-day return
	Vol30               float64 // raw std dev of daily returns, last 30 bars
	Liq                 float64 // ln(median 30-day quote_volume)
	MedianQuoteVolume30 float64 // the un-logged figure, for human display

	ZMom90 float64
	ZMom30 float64
	ZVol30 float64 // NOTE: applied with a negative sign in Score — high volatility should LOWER the score
	ZLiq   float64
}

// ToMetrics flattens ScoreComponents into the map[string]float64 shape
// domain.Signal.Metrics / proposals.metrics_json expect, so a strategy that
// selects a symbol from this package's output can copy its "why" straight
// through without re-deriving it.
func (c ScoreComponents) ToMetrics() map[string]float64 {
	return map[string]float64{
		"mom_90":                 c.Mom90,
		"mom_30":                 c.Mom30,
		"vol_30":                 c.Vol30,
		"liq":                    c.Liq,
		"median_quote_volume_30": c.MedianQuoteVolume30,
		"z_mom_90":               c.ZMom90,
		"z_mom_30":               c.ZMom30,
		"z_vol_30":               c.ZVol30,
		"z_liq":                  c.ZLiq,
	}
}

// ScoredSymbol is one Included symbol after Score, ranked against every
// other symbol in the same call (cross-sectional — SPEC.md Bölüm 16's
// sözlük entry for "kesitsel").
type ScoredSymbol struct {
	Symbol     string
	Rank       int // 1 = highest score
	Score      float64
	Components ScoreComponents
}

// Score ranks included (Filter's survivors) by SPEC.md Bölüm 6.2's
// cross-sectional formula. It assumes every entry has at least
// minCandlesForScoring candles — Filter's effectiveWarmupBars guarantees
// that for anything it lets through, so a caller that bypasses Filter and
// calls Score directly with shorter series will get NaN components for
// that symbol (indicator.ROC/StdDev's documented NaN-during-warmup
// behavior), not a panic.
//
// Score never mutates weights and never derives them from the data being
// scored — see Weights' doc comment.
func Score(included []Included, weights Weights) []ScoredSymbol {
	n := len(included)
	if n == 0 {
		return nil
	}

	rawMom90 := make([]float64, n)
	rawMom30 := make([]float64, n)
	rawVol30 := make([]float64, n)
	rawLiq := make([]float64, n)

	for i, c := range included {
		closes := closesOf(c.Candles)
		rawMom90[i] = lastOf(indicator.ROC(closes, 90))
		rawMom30[i] = lastOf(indicator.ROC(closes, 30))

		dailyReturns := indicator.ROC(closes, 1)
		rawVol30[i] = lastOf(indicator.StdDev(dailyReturns, 30))

		rawLiq[i] = math.Log(c.MedianQuoteVolume30)
	}

	zMom90 := indicator.ZScore(rawMom90)
	zMom30 := indicator.ZScore(rawMom30)
	zVol30 := indicator.ZScore(rawVol30)
	zLiq := indicator.ZScore(rawLiq)

	out := make([]ScoredSymbol, n)
	for i, c := range included {
		comp := ScoreComponents{
			Mom90:               rawMom90[i],
			Mom30:               rawMom30[i],
			Vol30:               rawVol30[i],
			Liq:                 rawLiq[i],
			MedianQuoteVolume30: c.MedianQuoteVolume30,
			ZMom90:              zMom90[i],
			ZMom30:              zMom30[i],
			ZVol30:              zVol30[i],
			ZLiq:                zLiq[i],
		}
		score := weights.Mom90*comp.ZMom90 +
			weights.Mom30*comp.ZMom30 -
			weights.Vol30*comp.ZVol30 +
			weights.Liq*comp.ZLiq

		out[i] = ScoredSymbol{Symbol: c.Symbol, Score: score, Components: comp}
	}

	// Best score first. NaN scores (possible only in the degenerate
	// cross-section where a component is constant across every symbol —
	// see indicator.ZScore's doc comment) sort last rather than panicking
	// or landing unpredictably, since Go's < on NaN is always false.
	sort.SliceStable(out, func(i, j int) bool {
		if math.IsNaN(out[i].Score) {
			return false
		}
		if math.IsNaN(out[j].Score) {
			return true
		}
		return out[i].Score > out[j].Score
	})
	for i := range out {
		out[i].Rank = i + 1
	}
	return out
}

func closesOf(candles []domain.Candle) []float64 {
	out := make([]float64, len(candles))
	for i, c := range candles {
		out[i] = c.Close
	}
	return out
}

func lastOf(v []float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	return v[len(v)-1]
}
