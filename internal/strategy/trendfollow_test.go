package strategy

import (
	"math"
	"reflect"
	"testing"

	"swingbot/internal/domain"
)

// testTrendfollowParams keeps the SPEC.md defaults but shrinks nothing —
// trendfollow's entry logic genuinely needs SMA(200), so tests build
// enough bars for it.
func testTrendfollowParams() TrendfollowParams {
	return DefaultTrendfollowParams()
}

func TestTrendfollow_EntersOnBreakoutAboveRisingSMA(t *testing.T) {
	tf := NewTrendfollow(testTrendfollowParams())
	sym := "UP/USDT"
	n := tf.WarmupBars() + 5
	closes := trendingCloses(n, 100, 0.01, 0.001) // steady uptrend, small noise
	series := makeCandles(closes, monday2024, 0.005, 1_000_000)

	// Sanity: the synthetic series really is a breakout (last close is the
	// max of the last 20) and really is above SMA(200) — otherwise this
	// test would prove nothing.
	last := closes[len(closes)-1]
	window := closes[len(closes)-20:]
	for _, c := range window {
		if c > last {
			t.Fatalf("test setup bug: last close %v is not the max of the last 20 (%v)", last, c)
		}
	}

	in := Input{
		AsOf:      series[len(series)-1].OpenTime,
		Series:    map[string][]domain.Candle{sym: series},
		Universe:  []string{sym},
		Portfolio: domain.Portfolio{Positions: map[string]domain.Position{}},
	}
	sigs, err := tf.Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	sig, ok := findSignal(sigs, sym, domain.SignalEnter)
	if !ok {
		t.Fatalf("expected an enter signal for a clean breakout above a rising SMA(200), got %+v", sigs)
	}
	if sig.Reason == "" {
		t.Errorf("Reason must be non-empty (İ6)")
	}
	if len(sig.Metrics) == 0 {
		t.Errorf("Metrics must be populated (İ6)")
	}
	wantStop := sig.RefPrice - testTrendfollowParams().ATRStopMult*sig.Metrics["atr_14"]
	if math.Abs(sig.StopPrice-wantStop) > 1e-9 {
		t.Errorf("StopPrice = %v, want close - k*ATR = %v", sig.StopPrice, wantStop)
	}
}

func TestTrendfollow_NoEntry_BelowLongSMA(t *testing.T) {
	tf := NewTrendfollow(testTrendfollowParams())
	sym := "DOWN/USDT"
	n := tf.WarmupBars() + 5
	closes := trendingCloses(n, 100, -0.01, 0.001) // steady downtrend
	series := makeCandles(closes, monday2024, 0.005, 1_000_000)

	in := Input{
		AsOf:      series[len(series)-1].OpenTime,
		Series:    map[string][]domain.Candle{sym: series},
		Universe:  []string{sym},
		Portfolio: domain.Portfolio{Positions: map[string]domain.Position{}},
	}
	sigs, err := tf.Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if _, ok := findSignal(sigs, sym, domain.SignalEnter); ok {
		t.Errorf("did not expect an enter signal for a symbol trading below its SMA(200): %+v", sigs)
	}
}

func TestTrendfollow_NoEntry_NoBreakout(t *testing.T) {
	tf := NewTrendfollow(testTrendfollowParams())
	sym := "SIDE/USDT"
	n := tf.WarmupBars() + 40
	// A long-running uptrend (so SMA(200) is comfortably below price) that
	// then goes flat/oscillating for the last 25 bars, so the AS-OF close
	// is not the 20-day max.
	up := trendingCloses(n-25, 100, 0.01, 0.001)
	base := up[len(up)-1]
	side := make([]float64, 25)
	for i := range side {
		// oscillate with a peak mid-window so the LAST close is below it
		side[i] = base * (1 + 0.05*math.Sin(float64(i)*0.5))
	}
	closes := append(up, side...)
	series := makeCandles(closes, monday2024, 0.005, 1_000_000)

	last := closes[len(closes)-1]
	window := closes[len(closes)-20:]
	isMax := true
	for _, c := range window {
		if c > last {
			isMax = false
		}
	}
	if isMax {
		t.Fatalf("test setup bug: expected last close NOT to be the 20-day max")
	}

	in := Input{
		AsOf:      series[len(series)-1].OpenTime,
		Series:    map[string][]domain.Candle{sym: series},
		Universe:  []string{sym},
		Portfolio: domain.Portfolio{Positions: map[string]domain.Position{}},
	}
	sigs, err := tf.Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if _, ok := findSignal(sigs, sym, domain.SignalEnter); ok {
		t.Errorf("did not expect an enter signal without a 20-day breakout: %+v", sigs)
	}
}

func TestTrendfollow_NoEntry_TooVolatile(t *testing.T) {
	tf := NewTrendfollow(testTrendfollowParams())
	sym := "WILD/USDT"
	n := tf.WarmupBars() + 5
	closes := trendingCloses(n, 100, 0.01, 0.001) // otherwise-clean breakout
	// hlSpread = 0.5 inflates every bar's true range to roughly the size
	// of the price itself, pushing ATR(14)/close far above max_atr_pct
	// (0.08 default) while leaving the close series (and thus the
	// SMA/breakout checks) untouched.
	series := makeCandles(closes, monday2024, 0.5, 1_000_000)

	in := Input{
		AsOf:      series[len(series)-1].OpenTime,
		Series:    map[string][]domain.Candle{sym: series},
		Universe:  []string{sym},
		Portfolio: domain.Portfolio{Positions: map[string]domain.Position{}},
	}
	sigs, err := tf.Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if _, ok := findSignal(sigs, sym, domain.SignalEnter); ok {
		t.Errorf("did not expect an enter signal when ATR%%/close exceeds max_atr_pct: %+v", sigs)
	}
}

func TestTrendfollow_NoEntry_AlreadyHeld(t *testing.T) {
	tf := NewTrendfollow(testTrendfollowParams())
	sym := "UP/USDT"
	n := tf.WarmupBars() + 5
	closes := trendingCloses(n, 100, 0.01, 0.001)
	series := makeCandles(closes, monday2024, 0.005, 1_000_000)

	in := Input{
		AsOf:     series[len(series)-1].OpenTime,
		Series:   map[string][]domain.Candle{sym: series},
		Universe: []string{sym},
		Portfolio: domain.Portfolio{Positions: map[string]domain.Position{
			sym: {Symbol: sym, Strategy: "trendfollow", EntryPrice: closes[0], StopPrice: 1, HighWater: closes[0]},
		}},
	}
	sigs, err := tf.Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if _, ok := findSignal(sigs, sym, domain.SignalEnter); ok {
		t.Errorf("did not expect a second enter signal for an already-held symbol: %+v", sigs)
	}
	// It should still be getting its trailing stop managed, though.
	if _, ok := findSignal(sigs, sym, domain.SignalStop); !ok {
		t.Errorf("expected a stop-update signal for the held position: %+v", sigs)
	}
}

func TestTrendfollow_TrailingStopOnlyMovesUp(t *testing.T) {
	tf := NewTrendfollow(testTrendfollowParams())
	sym := "TRAIL/USDT"

	// Build a run-up (so we have a real ATR and a real high-water mark),
	// then one MORE bar that pulls back (lower high, lower close) than the
	// run-up's peak.
	n := tf.WarmupBars() + 20
	closes := trendingCloses(n, 100, 0.008, 0.001)
	series := makeCandles(closes, monday2024.AddDate(0, 0, -1), 0.01, 1_000_000)

	// Position was opened earlier with a HighWater/StopPrice already
	// established from a prior, higher peak than today's bar will show.
	peakHigh := series[len(series)-1].High * 1.5 // pretend an earlier bar had gone much higher
	// priorStop is deliberately TIGHT (just under peakHigh) — well above
	// what HighWater-2.5*ATR would recompute to on a pullback day — so a
	// correct implementation must keep it instead of ratcheting down.
	priorStop := peakHigh * 0.999

	// Today's (pullback) bar: lower high than the recorded HighWater.
	pullbackClose := closes[len(closes)-1] * 0.97
	pullbackHigh := pullbackClose * 1.005
	pullbackLow := pullbackClose * 0.99
	today := domain.Candle{
		OpenTime:    series[len(series)-1].OpenTime.AddDate(0, 0, 1),
		Open:        series[len(series)-1].Close,
		High:        pullbackHigh,
		Low:         pullbackLow,
		Close:       pullbackClose,
		Volume:      1_000_000,
		QuoteVolume: pullbackClose * 1_000_000,
	}
	fullSeries := append(append([]domain.Candle(nil), series...), today)

	in := Input{
		AsOf:     today.OpenTime,
		Series:   map[string][]domain.Candle{sym: fullSeries},
		Universe: []string{sym},
		Portfolio: domain.Portfolio{Positions: map[string]domain.Position{
			sym: {Symbol: sym, Strategy: "trendfollow", EntryPrice: closes[0], StopPrice: priorStop, HighWater: peakHigh},
		}},
	}
	sigs, err := tf.Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	sig, ok := findSignal(sigs, sym, domain.SignalStop)
	if !ok {
		t.Fatalf("expected a stop-update signal, got %+v", sigs)
	}
	if sig.StopPrice != priorStop {
		t.Errorf("stop must never move down: recorded prior stop=%v, HighWater-derived stop would be lower, got StopPrice=%v", priorStop, sig.StopPrice)
	}
	if hw := sig.Metrics["high_water"]; hw != peakHigh {
		t.Errorf("HighWater must not decrease on a pullback day: got %v, want unchanged %v", hw, peakHigh)
	}

	// Now push a NEW all-time high through and confirm the stop DOES move
	// up to track it. Derived directly from peakHigh (with a large margin)
	// so it is unambiguously higher regardless of floating-point rounding
	// in how peakHigh itself was computed.
	newHighClose := peakHigh * 2
	newHigh := domain.Candle{
		OpenTime:    today.OpenTime.AddDate(0, 0, 1),
		Open:        today.Close,
		High:        newHighClose * 1.01,
		Low:         today.Close,
		Close:       newHighClose,
		Volume:      1_000_000,
		QuoteVolume: newHighClose * 1_000_000,
	}
	fullSeries2 := append(append([]domain.Candle(nil), fullSeries...), newHigh)
	in2 := Input{
		AsOf:     newHigh.OpenTime,
		Series:   map[string][]domain.Candle{sym: fullSeries2},
		Universe: []string{sym},
		Portfolio: domain.Portfolio{Positions: map[string]domain.Position{
			sym: {Symbol: sym, Strategy: "trendfollow", EntryPrice: closes[0], StopPrice: priorStop, HighWater: peakHigh},
		}},
	}
	sigs2, err := tf.Evaluate(in2)
	if err != nil {
		t.Fatalf("Evaluate (new high): %v", err)
	}
	sig2, ok := findSignal(sigs2, sym, domain.SignalStop)
	if !ok {
		t.Fatalf("expected a stop-update signal, got %+v", sigs2)
	}
	if sig2.StopPrice <= priorStop {
		t.Errorf("expected the stop to ratchet UP after a new all-time high: prior=%v, got=%v", priorStop, sig2.StopPrice)
	}
	if hw := sig2.Metrics["high_water"]; hw != newHigh.High {
		t.Errorf("HighWater should track the new high: got %v, want %v", hw, newHigh.High)
	}
}

func TestTrendfollow_ExitsBelowSMAExit(t *testing.T) {
	tf := NewTrendfollow(testTrendfollowParams())
	sym := "REVERSAL/USDT"

	// Run up first so SMA(200) sits below price, then crash the last bar
	// hard enough to close below SMA(50).
	n := tf.WarmupBars() + 60
	up := trendingCloses(n-1, 100, 0.006, 0.001)
	lastUp := up[len(up)-1]
	crash := lastUp * 0.5 // deep one-bar crash
	closes := append(up, crash)
	series := makeCandles(closes, monday2024, 0.01, 1_000_000)

	in := Input{
		AsOf:     series[len(series)-1].OpenTime,
		Series:   map[string][]domain.Candle{sym: series},
		Universe: []string{sym},
		Portfolio: domain.Portfolio{Positions: map[string]domain.Position{
			sym: {Symbol: sym, Strategy: "trendfollow", EntryPrice: 50, StopPrice: 10, HighWater: lastUp},
		}},
	}
	sigs, err := tf.Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	sig, ok := findSignal(sigs, sym, domain.SignalExit)
	if !ok {
		t.Fatalf("expected an exit signal after closing below SMA(exit), got %+v", sigs)
	}
	if sig.Reason == "" {
		t.Errorf("exit Reason must be non-empty (İ6)")
	}
	if _, ok := findSignal(sigs, sym, domain.SignalStop); !ok {
		t.Errorf("a held position should still get a stop-update signal even on an exit day: %+v", sigs)
	}
}

func TestTrendfollow_IgnoresPositionsFromOtherStrategies(t *testing.T) {
	tf := NewTrendfollow(testTrendfollowParams())
	sym := "OTHER/USDT"
	n := tf.WarmupBars() + 60
	up := trendingCloses(n-1, 100, 0.006, 0.001)
	crash := up[len(up)-1] * 0.5
	closes := append(up, crash)
	series := makeCandles(closes, monday2024, 0.01, 1_000_000)

	in := Input{
		AsOf:     series[len(series)-1].OpenTime,
		Series:   map[string][]domain.Candle{sym: series},
		Universe: []string{sym},
		Portfolio: domain.Portfolio{Positions: map[string]domain.Position{
			sym: {Symbol: sym, Strategy: "momentum", EntryPrice: 50, StopPrice: 10, HighWater: 50},
		}},
	}
	sigs, err := tf.Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if _, ok := findSignal(sigs, sym, domain.SignalStop); ok {
		t.Errorf("trendfollow must not manage a position opened by another strategy: %+v", sigs)
	}
	if _, ok := findSignal(sigs, sym, domain.SignalExit); ok {
		t.Errorf("trendfollow must not manage a position opened by another strategy: %+v", sigs)
	}
	// And since it's already held (by momentum), trendfollow must not try
	// to enter it either.
	if _, ok := findSignal(sigs, sym, domain.SignalEnter); ok {
		t.Errorf("must not propose entering a symbol already held by another strategy: %+v", sigs)
	}
}

func TestTrendfollow_Determinism(t *testing.T) {
	tf := NewTrendfollow(testTrendfollowParams())
	sym := "UP/USDT"
	n := tf.WarmupBars() + 5
	closes := trendingCloses(n, 100, 0.01, 0.001)
	series := makeCandles(closes, monday2024, 0.005, 1_000_000)

	in := Input{
		AsOf:      series[len(series)-1].OpenTime,
		Series:    map[string][]domain.Candle{sym: series},
		Universe:  []string{sym},
		Portfolio: domain.Portfolio{Positions: map[string]domain.Position{}},
	}
	got1, err := tf.Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate #1: %v", err)
	}
	got2, err := tf.Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate #2: %v", err)
	}
	if !reflect.DeepEqual(got1, got2) {
		t.Fatalf("Evaluate is not deterministic:\nrun1=%+v\nrun2=%+v", got1, got2)
	}
}

// TestTrendfollow_LookAhead mirrors TestMomentum_LookAhead: two candle
// series identical up to AsOf, diverging only afterwards, sliced down to
// an identical-length prefix before being handed to Evaluate. Signals must
// be identical.
func TestTrendfollow_LookAhead(t *testing.T) {
	tf := NewTrendfollow(testTrendfollowParams())
	sym := "LOOK/USDT"
	n := tf.WarmupBars() + 5
	const future = 15

	closesA := trendingCloses(n+future, 100, 0.01, 0.001)
	closesB := append([]float64(nil), closesA...)
	for i := n; i < n+future; i++ {
		closesB[i] = closesA[i] * 2.5
	}

	end := monday2024.AddDate(0, 0, future)
	fullA := makeCandles(closesA, end, 0.005, 1_000_000)
	fullB := makeCandles(closesB, end, 0.005, 1_000_000)

	asOfTime := fullA[n-1].OpenTime
	if fullB[n-1].OpenTime != asOfTime {
		t.Fatalf("test setup bug: A/B AsOf candle timestamps differ")
	}

	inA := Input{AsOf: asOfTime, Series: map[string][]domain.Candle{sym: fullA[:n]}, Universe: []string{sym}, Portfolio: domain.Portfolio{Positions: map[string]domain.Position{}}}
	inB := Input{AsOf: asOfTime, Series: map[string][]domain.Candle{sym: fullB[:n]}, Universe: []string{sym}, Portfolio: domain.Portfolio{Positions: map[string]domain.Position{}}}

	sigsA, err := tf.Evaluate(inA)
	if err != nil {
		t.Fatalf("Evaluate(A): %v", err)
	}
	sigsB, err := tf.Evaluate(inB)
	if err != nil {
		t.Fatalf("Evaluate(B): %v", err)
	}
	if !reflect.DeepEqual(sigsA, sigsB) {
		t.Fatalf("look-ahead violation: identical pre-AsOf data but different future produced different signals\nA=%+v\nB=%+v", sigsA, sigsB)
	}
}

func TestTrendfollow_NameWarmupParams(t *testing.T) {
	tf := NewTrendfollow(DefaultTrendfollowParams())
	if tf.Name() != "trendfollow" {
		t.Errorf("Name() = %q, want trendfollow", tf.Name())
	}
	if got, want := tf.WarmupBars(), 200; got != want {
		t.Errorf("WarmupBars() = %d, want %d", got, want)
	}
	params := tf.Params()
	if params["sma_long"] != 200 {
		t.Errorf("Params()[sma_long] = %v, want 200", params["sma_long"])
	}
	if params["max_atr_pct"] != 0.08 {
		t.Errorf("Params()[max_atr_pct] = %v, want 0.08", params["max_atr_pct"])
	}
}

// TestTrendfollow_StopNeverEnforced documents (and is a hard regression
// guard for) SPEC.md Bölüm 6.4.2's closing "Not": the strategy itself must
// never decide that low <= StopPrice means "exit". We simulate a bar whose
// Low pierces the recorded StopPrice while the close stays comfortably
// above SMA(exit) and assert no SignalExit is produced FOR THAT REASON
// (only the engine, given the reported StopPrice, may act on it).
func TestTrendfollow_StopNeverEnforced(t *testing.T) {
	tf := NewTrendfollow(testTrendfollowParams())
	sym := "GAP/USDT"
	n := tf.WarmupBars() + 20
	closes := trendingCloses(n, 100, 0.008, 0.001)
	series := makeCandles(closes, monday2024, 0.01, 1_000_000)

	stopPrice := series[len(series)-1].Close * 0.5 // far below today's low
	last := series[len(series)-1]
	if last.Low <= stopPrice {
		t.Fatalf("test setup bug: today's low (%v) must stay above the recorded stop (%v)", last.Low, stopPrice)
	}
	// Pierce the stop intraday by rewriting today's low, while keeping the
	// close comfortably above SMA(exit) so the discretionary exit path
	// doesn't fire and confound the assertion.
	pierced := append([]domain.Candle(nil), series...)
	pierced[len(pierced)-1].Low = stopPrice * 0.9

	in := Input{
		AsOf:     pierced[len(pierced)-1].OpenTime,
		Series:   map[string][]domain.Candle{sym: pierced},
		Universe: []string{sym},
		Portfolio: domain.Portfolio{Positions: map[string]domain.Position{
			sym: {Symbol: sym, Strategy: "trendfollow", EntryPrice: closes[0], StopPrice: stopPrice, HighWater: last.Close},
		}},
	}
	sigs, err := tf.Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if _, ok := findSignal(sigs, sym, domain.SignalExit); ok {
		t.Fatalf("strategy must never itself decide low<=stop triggers an exit — that is the engine's job: %+v", sigs)
	}
	if _, ok := findSignal(sigs, sym, domain.SignalStop); !ok {
		t.Errorf("expected a stop-update signal reporting the (possibly higher) stop level: %+v", sigs)
	}
}
