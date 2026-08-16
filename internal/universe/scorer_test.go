package universe

import (
	"math"
	"testing"
	"time"

	"swingbot/internal/domain"
	"swingbot/internal/indicator"
)

const scoreEpsilon = 1e-9

func closeEnough(a, b float64) bool {
	if math.IsNaN(a) && math.IsNaN(b) {
		return true
	}
	return math.Abs(a-b) < scoreEpsilon
}

// buildCloses returns a 91-element close-price series (indices 0..90, asOf
// is index 90) where:
//   - close[0]   = close0
//   - close[60]  = close60  (indices 1..59 are a linear filler between the
//     two — irrelevant to every metric Score computes, since mom_90 only
//     reads indices 0/90, mom_30 and vol_30 only read indices 60..90)
//   - close[61..90] is built by applying tailMultipliers (len 30) one day
//     at a time starting from close60, so mom_30/vol_30 are exact,
//     hand-verifiable functions of tailMultipliers alone.
func buildCloses(close0, close60 float64, tailMultipliers []float64) []float64 {
	if len(tailMultipliers) != 30 {
		panic("buildCloses: tailMultipliers must have exactly 30 elements")
	}
	out := make([]float64, 91)
	out[0] = close0
	for i := 1; i <= 60; i++ {
		t := float64(i) / 60
		out[i] = close0 + t*(close60-close0)
	}
	for i, m := range tailMultipliers {
		out[61+i] = out[60+i] * m
	}
	return out
}

func constMultipliers(ratio float64) []float64 {
	out := make([]float64, 30)
	for i := range out {
		out[i] = ratio
	}
	return out
}

// alternatingMultipliers returns 30 multipliers alternating (1+r), (1-r),
// ... . The resulting daily returns are exactly +r, -r, +r, -r, ... (15
// each), so their population mean is exactly 0 and their population
// std-dev is exactly r — a hand-verifiable, non-degenerate Vol30 value.
func alternatingMultipliers(r float64) []float64 {
	out := make([]float64, 30)
	for i := range out {
		if i%2 == 0 {
			out[i] = 1 + r
		} else {
			out[i] = 1 - r
		}
	}
	return out
}

func candlesFromCloses(asOf time.Time, closes []float64) []domain.Candle {
	n := len(closes)
	out := make([]domain.Candle, n)
	for i, px := range closes {
		out[i] = domain.Candle{
			OpenTime: asOf.AddDate(0, 0, -(n - 1 - i)),
			Open:     px, High: px, Low: px, Close: px,
			Volume: 1, QuoteVolume: 1,
		}
	}
	return out
}

// TestScore_MatchesSpecFormula builds three symbols whose raw momentum/
// volatility/liquidity components are hand-derivable from how their close
// series and MedianQuoteVolume30 were constructed (see buildCloses'
// contract), then checks Score's raw components, z-scores (computed here
// via the same indicator.ZScore this package uses — legitimate as an
// "expected" oracle since that function has its own dedicated unit tests)
// and final score against SPEC.md Bölüm 6.2's formula:
//
//	score = w1*z(mom_90) + w2*z(mom_30) - w3*z(vol_30) + w4*z(liq)
//
// This is a wiring test: it exists to catch a swapped index, a dropped
// negative sign on vol_30, or a base-10 vs. natural log mistake — not to
// re-derive momentum/indicator math from scratch (that is indicator's job).
func TestScore_MatchesSpecFormula(t *testing.T) {
	asOf := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)

	closesA := buildCloses(100, 140, constMultipliers(1.30)) // mom_30=0.30, vol_30=0 (constant daily ratio)
	closesB := buildCloses(95, 100, constMultipliers(1.10))  // mom_30=0.10, vol_30=0
	closesC := buildCloses(110, 120, alternatingMultipliers(0.05))

	volA, volB, volC := 8_000_000.0, 6_000_000.0, 15_000_000.0

	included := []Included{
		{Symbol: "A", MedianQuoteVolume30: volA, Candles: candlesFromCloses(asOf, closesA)},
		{Symbol: "B", MedianQuoteVolume30: volB, Candles: candlesFromCloses(asOf, closesB)},
		{Symbol: "C", MedianQuoteVolume30: volC, Candles: candlesFromCloses(asOf, closesC)},
	}
	weights := Weights{Mom90: 0.4, Mom30: 0.3, Vol30: 0.2, Liq: 0.1}

	// Independently derived expected raw values.
	expMom90 := map[string]float64{
		"A": closesA[90]/closesA[0] - 1,
		"B": closesB[90]/closesB[0] - 1,
		"C": closesC[90]/closesC[0] - 1,
	}
	expMom30 := map[string]float64{
		"A": closesA[90]/closesA[60] - 1,
		"B": closesB[90]/closesB[60] - 1,
		"C": closesC[90]/closesC[60] - 1,
	}
	expVol30 := map[string]float64{"A": 0, "B": 0, "C": 0.05}
	expLiq := map[string]float64{"A": math.Log(volA), "B": math.Log(volB), "C": math.Log(volC)}

	order := []string{"A", "B", "C"}
	rawMom90 := []float64{expMom90["A"], expMom90["B"], expMom90["C"]}
	rawMom30 := []float64{expMom30["A"], expMom30["B"], expMom30["C"]}
	rawVol30 := []float64{expVol30["A"], expVol30["B"], expVol30["C"]}
	rawLiq := []float64{expLiq["A"], expLiq["B"], expLiq["C"]}
	zMom90 := indicator.ZScore(rawMom90)
	zMom30 := indicator.ZScore(rawMom30)
	zVol30 := indicator.ZScore(rawVol30)
	zLiq := indicator.ZScore(rawLiq)

	expScore := map[string]float64{}
	for i, sym := range order {
		expScore[sym] = weights.Mom90*zMom90[i] + weights.Mom30*zMom30[i] - weights.Vol30*zVol30[i] + weights.Liq*zLiq[i]
	}

	got := Score(included, weights)
	if len(got) != 3 {
		t.Fatalf("expected 3 scored symbols, got %d", len(got))
	}

	bySymbol := map[string]ScoredSymbol{}
	for _, s := range got {
		bySymbol[s.Symbol] = s
	}

	for _, sym := range order {
		s, ok := bySymbol[sym]
		if !ok {
			t.Fatalf("missing symbol %s in Score output", sym)
		}
		c := s.Components
		if !closeEnough(c.Mom90, expMom90[sym]) {
			t.Errorf("%s: Mom90 = %v, want %v", sym, c.Mom90, expMom90[sym])
		}
		if !closeEnough(c.Mom30, expMom30[sym]) {
			t.Errorf("%s: Mom30 = %v, want %v", sym, c.Mom30, expMom30[sym])
		}
		if !closeEnough(c.Vol30, expVol30[sym]) {
			t.Errorf("%s: Vol30 = %v, want %v", sym, c.Vol30, expVol30[sym])
		}
		if !closeEnough(c.Liq, expLiq[sym]) {
			t.Errorf("%s: Liq = %v, want %v", sym, c.Liq, expLiq[sym])
		}
		if !closeEnough(s.Score, expScore[sym]) {
			t.Errorf("%s: Score = %v, want %v", sym, s.Score, expScore[sym])
		}
	}

	// Rank order: best score first, Rank fields 1..n in that order.
	for i := 1; i < len(got); i++ {
		if got[i-1].Score < got[i].Score {
			t.Fatalf("scores not sorted descending: %+v", got)
		}
		if got[i-1].Rank != i || got[i].Rank != i+1 {
			t.Fatalf("unexpected Rank fields: %+v", got)
		}
	}
}

// TestScore_VolatilityLowersScore isolates the vol_30 sign: two symbols
// share identical momentum and liquidity raw values (so those three
// components contribute identically to both scores) and differ only in
// vol_30. If the implementation ever dropped SPEC.md Bölüm 6.2's negative
// sign on vol_30 ("negatif ağırlık — oynaklığı cezalandır"), the calmer
// symbol would stop out-scoring the volatile one and this test would fail.
func TestScore_VolatilityLowersScore(t *testing.T) {
	asOf := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)

	// baseline: only present to keep the 3-way cross-section from being
	// degenerate (indicator.ZScore returns all-NaN when every value in a
	// component is identical).
	closesBaseline := buildCloses(100, 150, constMultipliers(1.333333))
	// calm and volatile: identical close0/close60/close90, so mom_90 and
	// mom_30 are IDENTICAL between them; only the path in between (and
	// therefore vol_30) differs.
	closesVolatile := buildCloses(100, 120, alternatingMultipliers(0.05))
	close90 := closesVolatile[90]
	// constMultipliers takes a PER-DAY ratio; close90/120 is the total
	// 30-day ratio, so the per-day ratio is its 30th root.
	closesCalm := buildCloses(100, 120, constMultipliers(math.Pow(close90/120, 1.0/30)))

	if math.Abs(closesCalm[90]-closesVolatile[90]) > scoreEpsilon {
		t.Fatalf("test setup bug: calm/volatile close[90] mismatch: %v vs %v", closesCalm[90], closesVolatile[90])
	}

	included := []Included{
		{Symbol: "baseline", MedianQuoteVolume30: 9_000_000, Candles: candlesFromCloses(asOf, closesBaseline)},
		{Symbol: "calm", MedianQuoteVolume30: 10_000_000, Candles: candlesFromCloses(asOf, closesCalm)},
		{Symbol: "volatile", MedianQuoteVolume30: 10_000_000, Candles: candlesFromCloses(asOf, closesVolatile)},
	}
	weights := Weights{Mom90: 0.4, Mom30: 0.3, Vol30: 0.2, Liq: 0.1}

	got := Score(included, weights)
	bySymbol := map[string]ScoredSymbol{}
	for _, s := range got {
		bySymbol[s.Symbol] = s
	}

	calm, volatile := bySymbol["calm"], bySymbol["volatile"]
	if !closeEnough(calm.Components.Mom90, volatile.Components.Mom90) {
		t.Fatalf("test setup bug: calm/volatile Mom90 differ: %v vs %v", calm.Components.Mom90, volatile.Components.Mom90)
	}
	if !closeEnough(calm.Components.Mom30, volatile.Components.Mom30) {
		t.Fatalf("test setup bug: calm/volatile Mom30 differ: %v vs %v", calm.Components.Mom30, volatile.Components.Mom30)
	}
	if calm.Components.Vol30 >= volatile.Components.Vol30 {
		t.Fatalf("test setup bug: expected calm.Vol30 < volatile.Vol30, got %v vs %v", calm.Components.Vol30, volatile.Components.Vol30)
	}

	if calm.Score <= volatile.Score {
		t.Fatalf("expected the calmer symbol to score higher (vol_30 must be penalized): calm=%v volatile=%v", calm.Score, volatile.Score)
	}
}

func TestScore_EmptyInput(t *testing.T) {
	if got := Score(nil, DefaultWeights); got != nil {
		t.Fatalf("expected nil for empty input, got %+v", got)
	}
}

func TestWeightsFromMap(t *testing.T) {
	w := WeightsFromMap(map[string]float64{"mom_90": 0.5, "liq": 0.2})
	want := Weights{Mom90: 0.5, Mom30: DefaultWeights.Mom30, Vol30: DefaultWeights.Vol30, Liq: 0.2}
	if w != want {
		t.Fatalf("WeightsFromMap = %+v, want %+v", w, want)
	}

	def := WeightsFromMap(nil)
	if def != DefaultWeights {
		t.Fatalf("WeightsFromMap(nil) = %+v, want DefaultWeights %+v", def, DefaultWeights)
	}
}
