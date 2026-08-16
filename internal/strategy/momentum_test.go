package strategy

import (
	"math"
	"reflect"
	"testing"
	"time"

	"swingbot/internal/domain"
)

// testMomentumParams returns small, fast lookbacks so tests don't need
// hundreds of bars, and zeroes out the vol_30/liq weights so ranking is
// driven purely by the (monotonic, easy to reason about) growth rate baked
// into each synthetic series by trendingCloses.
func testMomentumParams() MomentumParams {
	return MomentumParams{
		TopN:             3,
		ExitRank:         5,
		RebalanceWeekday: time.Monday,
		ATRPeriod:        3,
		ATRStopMult:      2,
		MomLong:          10,
		MomShort:         5,
		VolLookback:      5,
		LiqLookback:      5,
		Weights: map[string]float64{
			"mom_90": 0.6,
			"mom_30": 0.4,
			"vol_30": 0,
			"liq":    0,
		},
	}
}

// momentumUniverse builds 8 symbols (S0..S7) with strictly increasing
// daily growth rates (S7 grows fastest, S0 slowest/flattest), so the
// expected rank order is exactly S7,S6,S5,S4,S3,S2,S1,S0 for any weighting
// that puts positive weight on mom_90/mom_30 and zero on vol_30/liq.
func momentumUniverse(t *testing.T, asOf time.Time, n int) (universe []string, series map[string][]domain.Candle) {
	t.Helper()
	series = map[string][]domain.Candle{}
	for i := 0; i < 8; i++ {
		sym := symbolName(i)
		universe = append(universe, sym)
		dailyReturn := 0.001 * float64(i+1)
		closes := trendingCloses(n, 100, dailyReturn, 0.001)
		series[sym] = makeCandles(closes, asOf, 0.01, 1_000_000)
	}
	return universe, series
}

func symbolName(i int) string {
	return string(rune('A'+i/26)) + string(rune('0'+i%10)) + "/USDT"
}

func TestMomentum_EntersTopNByScore(t *testing.T) {
	m := NewMomentum(testMomentumParams())
	universe, series := momentumUniverse(t, monday2024, 30)

	in := Input{
		AsOf:      monday2024,
		Series:    series,
		Universe:  universe,
		Portfolio: domain.Portfolio{Positions: map[string]domain.Position{}},
	}
	sigs, err := m.Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if got := countKind(sigs, domain.SignalEnter); got != 3 {
		t.Fatalf("expected 3 enter signals, got %d (%+v)", got, sigs)
	}
	if countKind(sigs, domain.SignalExit) != 0 {
		t.Fatalf("expected no exit signals with an empty portfolio, got %+v", sigs)
	}

	wantTop := []string{symbolName(7), symbolName(6), symbolName(5)}
	for _, sym := range wantTop {
		sig, ok := findSignal(sigs, sym, domain.SignalEnter)
		if !ok {
			t.Fatalf("expected an enter signal for %s (top-3 fastest growth), got %+v", sym, sigs)
		}
		if sig.Reason == "" {
			t.Errorf("%s: Reason must be non-empty (İ6)", sym)
		}
		if sig.Metrics == nil || len(sig.Metrics) == 0 {
			t.Errorf("%s: Metrics must be populated (İ6)", sym)
		}
		wantStop := sig.RefPrice - testMomentumParams().ATRStopMult*sig.Metrics["atr_14"]
		if math.Abs(sig.StopPrice-wantStop) > 1e-9 {
			t.Errorf("%s: StopPrice = %v, want entry - k*ATR = %v", sym, sig.StopPrice, wantStop)
		}
		if sig.StopPrice >= sig.RefPrice {
			t.Errorf("%s: StopPrice (%v) should be below RefPrice (%v)", sym, sig.StopPrice, sig.RefPrice)
		}
	}

	// Scores must be strictly decreasing in rank order (S7 > S6 > S5).
	s7, _ := findSignal(sigs, symbolName(7), domain.SignalEnter)
	s6, _ := findSignal(sigs, symbolName(6), domain.SignalEnter)
	s5, _ := findSignal(sigs, symbolName(5), domain.SignalEnter)
	if !(s7.Score > s6.Score && s6.Score > s5.Score) {
		t.Errorf("expected strictly decreasing scores by growth rate, got S7=%v S6=%v S5=%v", s7.Score, s6.Score, s5.Score)
	}

	for _, sym := range []string{symbolName(0), symbolName(1), symbolName(2), symbolName(3), symbolName(4)} {
		if _, ok := findSignal(sigs, sym, domain.SignalEnter); ok {
			t.Errorf("did not expect an enter signal for %s (outside top-3)", sym)
		}
	}
}

func TestMomentum_SkipsAlreadyHeldOnEntry(t *testing.T) {
	m := NewMomentum(testMomentumParams())
	universe, series := momentumUniverse(t, monday2024, 30)

	top1 := symbolName(7)
	in := Input{
		AsOf:     monday2024,
		Series:   series,
		Universe: universe,
		Portfolio: domain.Portfolio{Positions: map[string]domain.Position{
			top1: {Symbol: top1, Strategy: "momentum", StopPrice: 1, EntryPrice: 1},
		}},
	}
	sigs, err := m.Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if _, ok := findSignal(sigs, top1, domain.SignalEnter); ok {
		t.Errorf("did not expect a duplicate enter signal for an already-held top-ranked symbol %s", top1)
	}
	// Momentum does NOT "promote" rank 4 to fill a held rank-1 slot — only
	// the top-N ranks (2, 3 here) that are not already held get an enter
	// signal (SPEC.md 6.4.1: "İlk N'i seç ... Halihazırda tutulmayan her
	// biri için SignalEnter üret").
	if got := countKind(sigs, domain.SignalEnter); got != 2 {
		t.Fatalf("expected 2 enter signals (rank 2 and 3, rank 1 already held), got %d: %+v", got, sigs)
	}
	if _, ok := findSignal(sigs, symbolName(6), domain.SignalEnter); !ok {
		t.Errorf("expected enter signal for rank-2 symbol %s", symbolName(6))
	}
	if _, ok := findSignal(sigs, symbolName(5), domain.SignalEnter); !ok {
		t.Errorf("expected enter signal for rank-3 symbol %s", symbolName(5))
	}
	if _, ok := findSignal(sigs, symbolName(4), domain.SignalEnter); ok {
		t.Errorf("did not expect rank-4 symbol %s to be promoted into the held rank-1 slot", symbolName(4))
	}
}

func TestMomentum_ExitOutsideExitRank(t *testing.T) {
	m := NewMomentum(testMomentumParams())
	universe, series := momentumUniverse(t, monday2024, 30)

	low1, low2 := symbolName(0), symbolName(1) // ranks 8 and 7 respectively
	in := Input{
		AsOf:     monday2024,
		Series:   series,
		Universe: universe,
		Portfolio: domain.Portfolio{Positions: map[string]domain.Position{
			low1: {Symbol: low1, Strategy: "momentum"},
			low2: {Symbol: low2, Strategy: "momentum"},
		}},
	}
	sigs, err := m.Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	for _, sym := range []string{low1, low2} {
		sig, ok := findSignal(sigs, sym, domain.SignalExit)
		if !ok {
			t.Fatalf("expected an exit signal for %s (rank outside exit_rank=5), got %+v", sym, sigs)
		}
		if sig.Reason == "" {
			t.Errorf("%s: exit Reason must be non-empty (İ6)", sym)
		}
		if rank := sig.Metrics["rank"]; rank <= 5 {
			t.Errorf("%s: expected Metrics[rank] > 5, got %v", sym, rank)
		}
	}
	// Top-3 by growth are still proposed for entry (unrelated to the exits
	// above — they're not currently held).
	if got := countKind(sigs, domain.SignalEnter); got != 3 {
		t.Errorf("expected 3 enter signals alongside the exits, got %d: %+v", got, sigs)
	}
}

func TestMomentum_HeldWithinExitRank_NoExit(t *testing.T) {
	m := NewMomentum(testMomentumParams())
	universe, series := momentumUniverse(t, monday2024, 30)

	sym := symbolName(4) // rank 4, inside exit_rank=5
	in := Input{
		AsOf:     monday2024,
		Series:   series,
		Universe: universe,
		Portfolio: domain.Portfolio{Positions: map[string]domain.Position{
			sym: {Symbol: sym, Strategy: "momentum"},
		}},
	}
	sigs, err := m.Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if _, ok := findSignal(sigs, sym, domain.SignalExit); ok {
		t.Errorf("did not expect an exit signal for %s (rank 4 is within exit_rank=5): %+v", sym, sigs)
	}
}

func TestMomentum_IgnoresPositionsFromOtherStrategies(t *testing.T) {
	m := NewMomentum(testMomentumParams())
	universe, series := momentumUniverse(t, monday2024, 30)

	sym := symbolName(0) // rank 8, would exit if it were a momentum position
	in := Input{
		AsOf:     monday2024,
		Series:   series,
		Universe: universe,
		Portfolio: domain.Portfolio{Positions: map[string]domain.Position{
			sym: {Symbol: sym, Strategy: "trendfollow"},
		}},
	}
	sigs, err := m.Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if _, ok := findSignal(sigs, sym, domain.SignalExit); ok {
		t.Errorf("momentum must not manage a position opened by another strategy: %+v", sigs)
	}
	// It's also held (by trendfollow), so momentum must not try to enter it either.
	if _, ok := findSignal(sigs, sym, domain.SignalEnter); ok {
		t.Errorf("must not propose entering a symbol already held by another strategy: %+v", sigs)
	}
}

func TestMomentum_NonRebalanceDay_NoSignals(t *testing.T) {
	m := NewMomentum(testMomentumParams())
	tuesday := monday2024.AddDate(0, 0, 1)
	universe, series := momentumUniverse(t, tuesday, 30)

	in := Input{
		AsOf:     tuesday,
		Series:   series,
		Universe: universe,
		Portfolio: domain.Portfolio{Positions: map[string]domain.Position{
			symbolName(0): {Symbol: symbolName(0), Strategy: "momentum"}, // would exit on a rebalance day
		}},
	}
	sigs, err := m.Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(sigs) != 0 {
		t.Fatalf("expected no signals on a non-rebalance day, got %+v", sigs)
	}
}

func TestMomentum_Determinism(t *testing.T) {
	m := NewMomentum(testMomentumParams())
	universe, series := momentumUniverse(t, monday2024, 30)
	in := Input{
		AsOf:     monday2024,
		Series:   series,
		Universe: universe,
		Portfolio: domain.Portfolio{Positions: map[string]domain.Position{
			symbolName(0): {Symbol: symbolName(0), Strategy: "momentum"},
		}},
	}

	got1, err := m.Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate #1: %v", err)
	}
	got2, err := m.Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate #2: %v", err)
	}
	if !reflect.DeepEqual(got1, got2) {
		t.Fatalf("Evaluate is not deterministic:\nrun1=%+v\nrun2=%+v", got1, got2)
	}
}

// TestMomentum_LookAhead builds two universes that are byte-for-byte
// identical up through AsOf and differ only in bars AFTER AsOf, then feeds
// Evaluate only the (identical) prefix. If the strategy ever looked past
// the slice it was given, the pre-AsOf-identical assumption wouldn't be
// testable this way in the first place — the point of this test is a
// regression guard: any future change that starts indexing len(series)+k
// or re-slicing to a larger capacity would need the two runs to diverge,
// and they must not.
func TestMomentum_LookAhead(t *testing.T) {
	m := NewMomentum(testMomentumParams())
	const n = 30
	const future = 15

	sym := "LOOK/USDT"
	closesA := trendingCloses(n+future, 100, 0.004, 0.001)
	closesB := append([]float64(nil), closesA...)
	// Diverge only the future tail (index n..n+future-1).
	for i := n; i < n+future; i++ {
		closesB[i] = closesA[i] * 1.9 // wildly different "future"
	}

	end := monday2024.AddDate(0, 0, future) // arbitrary later Monday
	fullA := makeCandles(closesA, end, 0.01, 1_000_000)
	fullB := makeCandles(closesB, end, 0.01, 1_000_000)

	asOfTime := fullA[n-1].OpenTime
	if fullB[n-1].OpenTime != asOfTime {
		t.Fatalf("test setup bug: A/B AsOf candle timestamps differ")
	}

	seriesA := map[string][]domain.Candle{sym: fullA[:n]}
	seriesB := map[string][]domain.Candle{sym: fullB[:n]}

	inA := Input{AsOf: asOfTime, Series: seriesA, Universe: []string{sym}, Portfolio: domain.Portfolio{Positions: map[string]domain.Position{}}}
	inB := Input{AsOf: asOfTime, Series: seriesB, Universe: []string{sym}, Portfolio: domain.Portfolio{Positions: map[string]domain.Position{}}}

	// AsOf must land on a Monday for momentum to produce any signal at all.
	if asOfTime.Weekday() != time.Monday {
		t.Fatalf("test setup bug: AsOf %v is not a Monday", asOfTime)
	}

	sigsA, err := m.Evaluate(inA)
	if err != nil {
		t.Fatalf("Evaluate(A): %v", err)
	}
	sigsB, err := m.Evaluate(inB)
	if err != nil {
		t.Fatalf("Evaluate(B): %v", err)
	}
	if !reflect.DeepEqual(sigsA, sigsB) {
		t.Fatalf("look-ahead violation: identical pre-AsOf data but different future produced different signals\nA=%+v\nB=%+v", sigsA, sigsB)
	}
}

func TestMomentum_NameWarmupParams(t *testing.T) {
	m := NewMomentum(DefaultMomentumParams())
	if m.Name() != "momentum" {
		t.Errorf("Name() = %q, want momentum", m.Name())
	}
	if got, want := m.WarmupBars(), 91; got != want {
		t.Errorf("WarmupBars() = %d, want %d", got, want)
	}
	params := m.Params()
	if params["top_n"] != 5 {
		t.Errorf("Params()[top_n] = %v, want 5", params["top_n"])
	}
	if params["exit_rank"] != 10 {
		t.Errorf("Params()[exit_rank] = %v, want 10", params["exit_rank"])
	}
}
