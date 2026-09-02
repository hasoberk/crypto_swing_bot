// Walk-forward validation (SPEC.md Bölüm 11.2) plus the machinery Bölüm 11
// as a whole needs around it: parameter search on a TRAINING slice, honest
// out-of-sample measurement on the following TEST slice, the two chained
// end to end into one combined track record.
//
// Ownership note: this file (and sensitivity.go / thresholds.go / locked.go
// alongside it, all package backtest) is validation-analysis-engineer's
// (Ajan 10, Faz 3) territory — it consumes backtest.Run, backtest.Metrics
// and backtest.BuyAndHoldCurve (backtest-engine-architect / Ajan 6) exactly
// as a report or the CLI would, and adds no new lower-level engine
// behavior of its own.
//
// Binding rule this file must not violate (İ2, SPEC.md Bölüm 1.2): a
// training window may never see test-window data, and vice versa. See
// sliceCandlesForWindow's doc comment for how that is enforced here, on
// top of the day-level İ2 guarantee backtest.Run already provides inside
// each window.
package backtest

import (
	"context"
	"fmt"
	"time"

	"swingbot/internal/broker"
	"swingbot/internal/domain"
	"swingbot/internal/risk"
	"swingbot/internal/strategy"
)

// ParamSet is one point in a parameter search: parameter name -> value.
// Keys are whatever the caller's StrategyFactory understands — this
// package stays strategy-agnostic (SPEC.md Bölüm 5.4's Strategy interface
// has no constructor, only Params() as OUTPUT), so bridging "name -> a
// concrete xxxParams struct field" is the CLI layer's job (see
// cmd/swingbot's walkforward wiring, paramSetStrategyFactory).
type ParamSet map[string]float64

// Clone returns an independent copy of p.
func (p ParamSet) Clone() ParamSet {
	out := make(ParamSet, len(p))
	for k, v := range p {
		out[k] = v
	}
	return out
}

// StrategyFactory builds a fresh strategy.Strategy for a given ParamSet.
// It must be side-effect-free and safe to call repeatedly (once per
// ParamGrid entry per window) — the same constraints strategy.Strategy
// itself carries (SPEC.md Bölüm 5.4: no I/O, deterministic).
type StrategyFactory func(ParamSet) (strategy.Strategy, error)

// Objective scores a training-window Metrics result for parameter
// selection. Higher is better.
type Objective func(Metrics) float64

// DefaultObjective is Calmar (CAGR / |MaxDrawdown|), falling back to
// TotalReturn when Calmar is undefined (MaxDrawdown == 0 — e.g. a training
// slice with too few trades to ever draw down). SPEC.md Bölüm 7.4 is
// explicit about why Calmar, not Sharpe, drives decisions: "Sharpe'ı bilgi
// olarak raporla ama karar verirken Calmar ve maksimum düşüşe bak."
func DefaultObjective(m Metrics) float64 {
	if m.MaxDrawdown < 0 {
		return m.Calmar
	}
	return m.TotalReturn
}

// Window is one walk-forward step's train/test date range (SPEC.md Bölüm
// 11.2: "365 gün eğitim → 90 gün test → 90 gün ileri kaydır").
// [TrainStart,TrainEnd) selects parameters; [TestStart,TestEnd) applies
// them out-of-sample. TestStart always equals TrainEnd: training on data
// the test period will also be scored against is look-ahead bias at the
// parameter-selection level, the same family of bug İ2 forbids at the
// signal level.
type Window struct {
	TrainStart, TrainEnd time.Time
	TestStart, TestEnd   time.Time
}

// BuildWindows lays out rolling (not anchored) walk-forward windows over
// [dataStart, dataEnd] per SPEC.md Bölüm 11.2's defaults
// (trainDays=365, testDays=90, stepDays=90). The first window's TrainStart
// is dataStart; each subsequent window's TrainStart advances by stepDays —
// window i+1 does NOT keep accumulating window i's training data, it
// slides past it ("ileri kaydır"). Only windows whose TestEnd does not
// exceed dataEnd are returned, so every returned window has a complete
// test period; a date range too short for even one full window returns
// nil, not a truncated one.
func BuildWindows(dataStart, dataEnd time.Time, trainDays, testDays, stepDays int) []Window {
	if trainDays <= 0 || testDays <= 0 || stepDays <= 0 || !dataEnd.After(dataStart) {
		return nil
	}
	var windows []Window
	trainStart := dataStart
	for {
		trainEnd := trainStart.AddDate(0, 0, trainDays)
		testStart := trainEnd
		testEnd := testStart.AddDate(0, 0, testDays)
		if testEnd.After(dataEnd) {
			break
		}
		windows = append(windows, Window{
			TrainStart: trainStart, TrainEnd: trainEnd,
			TestStart: testStart, TestEnd: testEnd,
		})
		trainStart = trainStart.AddDate(0, 0, stepDays)
	}
	return windows
}

// Regime buckets a period by how its BTC benchmark performed over it
// (SPEC.md Bölüm 14 "Rejim değişimi", Bölüm 11.4's "en az bir rejimde"
// threshold).
type Regime string

const (
	RegimeBull     Regime = "bull"
	RegimeBear     Regime = "bear"
	RegimeSideways Regime = "sideways"
)

// DefaultBullThreshold / DefaultBearThreshold are the (positive-magnitude)
// BTC-return cutoffs ClassifyRegime uses when the caller does not override
// them. Fixed and documented here — not tuned against this project's own
// walk-forward results — same "decide before you look" discipline SPEC.md
// Bölüm 11.4 requires of the go/no-go thresholds themselves.
const (
	DefaultBullThreshold = 0.20
	DefaultBearThreshold = 0.20
)

// ClassifyRegime labels a period bull if BTC's return over it is at least
// bullThreshold, bear if it is at most -bearThreshold, sideways otherwise.
// bearThreshold is given as a positive magnitude.
func ClassifyRegime(btcReturn, bullThreshold, bearThreshold float64) Regime {
	switch {
	case btcReturn >= bullThreshold:
		return RegimeBull
	case btcReturn <= -bearThreshold:
		return RegimeBear
	default:
		return RegimeSideways
	}
}

// WindowResult is one walk-forward window's full outcome.
type WindowResult struct {
	Window Window
	// ChosenParams is the ParamSet Objective ranked highest over the
	// training slice.
	ChosenParams ParamSet
	// TrainMetrics is that ParamSet's IN-SAMPLE performance — useful for
	// spotting a choice that only ever worked in-sample, but it is never
	// what SPEC.md Bölüm 11.4's thresholds are graded against.
	TrainMetrics Metrics
	// TestMetrics/TestEquity/TestTrades are the OUT-OF-SAMPLE result: the
	// chosen ParamSet applied, unmodified, to data it never influenced the
	// selection of. This is what "counts".
	TestMetrics Metrics
	TestEquity  []EquityPoint
	TestTrades  []broker.ClosedTrade
	// TestBenchBTC is the BTC buy-and-hold curve over exactly the test
	// slice, seeded with the SAME initial cash as TestEquity[0] (İ3: every
	// result travels with a benchmark) — also what ClassifyRegime grades
	// this window's Regime from.
	TestBenchBTC []EquityPoint
	Regime       Regime
}

// WalkForwardConfig configures RunWalkForward.
type WalkForwardConfig struct {
	NewStrategy StrategyFactory
	// ParamGrid is every ParamSet tried on each window's training slice.
	// A nil/empty grid means "no search": RunWalkForward calls
	// NewStrategy(ParamSet{}) once per window and applies it out-of-sample
	// unchanged — still a legitimate walk-forward run, since separating
	// train/test in TIME (SPEC.md Bölüm 11.2) does not itself require a
	// parameter search; that is layered on top by Bölüm 11.3.
	ParamGrid []ParamSet
	// Objective ranks ParamGrid entries on their training-slice Metrics.
	// Defaults to DefaultObjective (Calmar) if nil.
	Objective Objective

	Candles  map[string][]domain.Candle
	Universe []string
	Markets  map[string]domain.Market

	InitialCash float64
	Costs       broker.Costs

	// NewRiskGate/NewBreaker build a FRESH gate/breaker for every single
	// sub-backtest (each training-slice ParamGrid trial AND each test
	// slice), so one run's rejections, cooldowns or a tripped breaker can
	// never leak into a supposedly-independent later run. Both default to
	// backtest.Run's own zero-value defaults (nil RiskGate/Breaker) when
	// left nil.
	NewRiskGate func() RiskGate
	NewBreaker  func() *risk.Breaker

	// TrainDays/TestDays/StepDays: SPEC.md Bölüm 11.2 defaults are
	// 365/90/90; callers (e.g. the CLI) should default to those, not this
	// package's zero value.
	TrainDays, TestDays, StepDays int

	// BenchmarkSymbol is the "BTC al-tut" comparison symbol (İ3). Defaults
	// to "BTC/USDT" if empty.
	BenchmarkSymbol string

	// BullThreshold/BearThreshold feed ClassifyRegime. 0 => the Default*
	// constants above.
	BullThreshold, BearThreshold float64
}

// WalkForwardResult is RunWalkForward's output.
type WalkForwardResult struct {
	Windows []WindowResult

	// CombinedEquity/CombinedTrades/CombinedBenchBTC are every window's
	// test-slice result STITCHED together — each window's starting cash is
	// the previous window's ENDING equity (see RunWalkForward), so this
	// reads as one continuous out-of-sample track record rather than a set
	// of independent short backtests, exactly what SPEC.md Bölüm 11.2
	// means by "sonuçları birleştir".
	CombinedEquity   []EquityPoint
	CombinedTrades   []broker.ClosedTrade
	CombinedBenchBTC []EquityPoint

	// Metrics is Compute(CombinedEquity, CombinedTrades) — the walk-forward
	// analogue of a single backtest.Result.Metrics, and what SPEC.md Bölüm
	// 11.4's thresholds are graded against (never TrainMetrics).
	Metrics Metrics
}

// RunWalkForward executes SPEC.md Bölüm 11.2 end to end: lay out rolling
// windows, pick parameters on each window's training slice, apply them
// out-of-sample on the following test slice, and chain every test slice
// into one combined result.
func RunWalkForward(ctx context.Context, cfg WalkForwardConfig) (*WalkForwardResult, error) {
	if cfg.NewStrategy == nil {
		return nil, fmt.Errorf("backtest: WalkForwardConfig.NewStrategy is required")
	}
	if len(cfg.Candles) == 0 {
		return nil, fmt.Errorf("backtest: WalkForwardConfig.Candles is empty")
	}
	objective := cfg.Objective
	if objective == nil {
		objective = DefaultObjective
	}
	paramGrid := cfg.ParamGrid
	if len(paramGrid) == 0 {
		paramGrid = []ParamSet{{}}
	}
	benchSymbol := cfg.BenchmarkSymbol
	if benchSymbol == "" {
		benchSymbol = "BTC/USDT"
	}
	bullT, bearT := cfg.BullThreshold, cfg.BearThreshold
	if bullT <= 0 {
		bullT = DefaultBullThreshold
	}
	if bearT <= 0 {
		bearT = DefaultBearThreshold
	}

	calendar := buildCalendar(cfg.Candles)
	if len(calendar) == 0 {
		return nil, fmt.Errorf("backtest: no candles to derive a calendar from")
	}
	windows := BuildWindows(calendar[0], calendar[len(calendar)-1], cfg.TrainDays, cfg.TestDays, cfg.StepDays)
	if len(windows) == 0 {
		return nil, fmt.Errorf(
			"backtest: date range [%s, %s] is too short for train=%dd/test=%dd/step=%dd walk-forward windows",
			calendar[0].Format("2006-01-02"), calendar[len(calendar)-1].Format("2006-01-02"),
			cfg.TrainDays, cfg.TestDays, cfg.StepDays,
		)
	}

	res := &WalkForwardResult{}
	runningCash := cfg.InitialCash

	for _, w := range windows {
		best, bestMetrics, err := selectParams(ctx, cfg, paramGrid, objective, w)
		if err != nil {
			return nil, err
		}

		strat, err := cfg.NewStrategy(best)
		if err != nil {
			return nil, fmt.Errorf("backtest: NewStrategy(%v) for test window starting %s: %w", best, w.TestStart.Format("2006-01-02"), err)
		}
		testCandles := sliceCandlesForWindow(cfg.Candles, w.TestStart, w.TestEnd, strat.WarmupBars())
		testInitialCash := runningCash
		testRes, err := runSubBacktest(ctx, cfg, strat, testCandles, w.TestStart, w.TestEnd, testInitialCash)
		if err != nil {
			return nil, fmt.Errorf("backtest: test window %s→%s: %w", w.TestStart.Format("2006-01-02"), w.TestEnd.Format("2006-01-02"), err)
		}

		testCal := make([]time.Time, len(testRes.Equity))
		for i, p := range testRes.Equity {
			testCal[i] = p.Date
		}
		btcCurve := BuyAndHoldCurve(cfg.Candles[benchSymbol], testCal, testInitialCash, cfg.Costs)

		regime := RegimeSideways
		if len(btcCurve) > 1 && btcCurve[0].Equity > 0 {
			btcReturn := btcCurve[len(btcCurve)-1].Equity/btcCurve[0].Equity - 1
			regime = ClassifyRegime(btcReturn, bullT, bearT)
		}

		res.Windows = append(res.Windows, WindowResult{
			Window: w, ChosenParams: best, TrainMetrics: bestMetrics,
			TestMetrics: testRes.Metrics, TestEquity: testRes.Equity, TestTrades: testRes.Trades,
			TestBenchBTC: btcCurve, Regime: regime,
		})

		res.CombinedEquity = append(res.CombinedEquity, testRes.Equity...)
		res.CombinedTrades = append(res.CombinedTrades, testRes.Trades...)
		res.CombinedBenchBTC = append(res.CombinedBenchBTC, btcCurve...)

		if len(testRes.Equity) > 0 {
			// Chain: the NEXT window's test slice starts with however much
			// equity THIS window's test slice ended with, so
			// CombinedEquity is one continuous curve, not InitialCash
			// repeated at every window boundary.
			runningCash = testRes.Equity[len(testRes.Equity)-1].Equity
		}
	}

	res.Metrics = Compute(res.CombinedEquity, res.CombinedTrades)
	return res, nil
}

// selectParams runs every ParamGrid entry on w's training slice and
// returns the one Objective ranks highest, plus its training Metrics. A
// ParamSet whose sub-backtest fails outright (e.g. its warmup requirement
// leaves no evaluable days in this training window) is skipped, not
// fatal — a coarse grid may deliberately include ParamSets that only make
// sense for some windows (e.g. sweeping sma_long up to 400 against a
// 365-day train window).
func selectParams(ctx context.Context, cfg WalkForwardConfig, paramGrid []ParamSet, objective Objective, w Window) (ParamSet, Metrics, error) {
	var (
		best       ParamSet
		bestScore  float64
		bestMetric Metrics
		haveBest   bool
	)
	for _, ps := range paramGrid {
		strat, err := cfg.NewStrategy(ps)
		if err != nil {
			return nil, Metrics{}, fmt.Errorf("backtest: NewStrategy(%v) for training window starting %s: %w", ps, w.TrainStart.Format("2006-01-02"), err)
		}
		trainCandles := sliceCandlesForWindow(cfg.Candles, w.TrainStart, w.TrainEnd, strat.WarmupBars())
		trainRes, err := runSubBacktest(ctx, cfg, strat, trainCandles, w.TrainStart, w.TrainEnd, cfg.InitialCash)
		if err != nil {
			continue
		}
		score := objective(trainRes.Metrics)
		if !haveBest || score > bestScore {
			haveBest, bestScore, best, bestMetric = true, score, ps, trainRes.Metrics
		}
	}
	if !haveBest {
		return nil, Metrics{}, fmt.Errorf(
			"backtest: no ParamSet in the grid produced a usable training result for window %s→%s",
			w.TrainStart.Format("2006-01-02"), w.TrainEnd.Format("2006-01-02"),
		)
	}
	return best, bestMetric, nil
}

// sliceCandlesForWindow returns, per symbol, the chronological candles
// with OpenTime in [from-lookback, to) — lookback is enough calendar days
// before `from` to cover warmupBars of history (crypto candles are 24/7
// with essentially no calendar gaps, so warmup bars ≈ calendar days; +7 is
// a small safety margin for any residual data gaps SPEC.md Bölüm 6.1's
// quality checks did not already strip out).
//
// This — NOT just backtest.Run's own day-level İ2 guarantee — is what
// keeps a training window's parameter choice from ever seeing a single
// bar of its following test window: `to` is a hard exclusive upper bound
// on what NewStrategy(ps) is handed, so it is a Go slice bounds violation
// (impossible, not just disallowed) for a training run to read test-window
// data, exactly the same class of runtime guarantee truncateSeries gives
// day-by-day inside a single Run.
func sliceCandlesForWindow(candles map[string][]domain.Candle, from, to time.Time, warmupBars int) map[string][]domain.Candle {
	lookbackStart := from.AddDate(0, 0, -(warmupBars + 7))
	out := make(map[string][]domain.Candle, len(candles))
	for sym, series := range candles {
		var sliced []domain.Candle
		for _, c := range series {
			if !c.OpenTime.Before(lookbackStart) && c.OpenTime.Before(to) {
				sliced = append(sliced, c)
			}
		}
		if len(sliced) > 0 {
			out[sym] = sliced
		}
	}
	return out
}

// runSubBacktest runs a single window (training or test) with a fresh
// RiskGate/Breaker, then trims the result down to exactly [from,to) —
// Run's own calendar may start a little later than `from` (it always
// evaluates from calendar[warmup+1] onward) or, thanks to
// sliceCandlesForWindow's lookback buffer, a stray extra day could in
// principle appear; filtering here is what makes the [from,to) contract
// exact rather than "close, modulo warmup slop".
func runSubBacktest(ctx context.Context, cfg WalkForwardConfig, strat strategy.Strategy, candles map[string][]domain.Candle, from, to time.Time, initialCash float64) (*Result, error) {
	if len(candles) == 0 {
		return nil, fmt.Errorf("no candles in [%s, %s)", from.Format("2006-01-02"), to.Format("2006-01-02"))
	}

	var riskGate RiskGate
	if cfg.NewRiskGate != nil {
		riskGate = cfg.NewRiskGate()
	}
	var brk *risk.Breaker
	if cfg.NewBreaker != nil {
		brk = cfg.NewBreaker()
	}

	res, err := Run(ctx, Config{
		Strategy: strat, Candles: candles, Universe: cfg.Universe, Markets: cfg.Markets,
		InitialCash: initialCash, Costs: cfg.Costs, RiskGate: riskGate, Breaker: brk,
		Mode: "backtest",
	})
	if err != nil {
		return nil, err
	}

	res.Equity = filterEquity(res.Equity, from, to)
	res.Trades = filterTradesByEntry(res.Trades, from, to)
	res.Metrics = Compute(res.Equity, res.Trades)
	return res, nil
}

func filterEquity(equity []EquityPoint, from, to time.Time) []EquityPoint {
	var out []EquityPoint
	for _, p := range equity {
		if !p.Date.Before(from) && p.Date.Before(to) {
			out = append(out, p)
		}
	}
	return out
}

func filterTradesByEntry(trades []broker.ClosedTrade, from, to time.Time) []broker.ClosedTrade {
	var out []broker.ClosedTrade
	for _, tr := range trades {
		if !tr.EntryTime.Before(from) && tr.EntryTime.Before(to) {
			out = append(out, tr)
		}
	}
	return out
}

// CartesianParamGrid expands axes into the cartesian product of every
// axis's values — the ParamGrid a naive "search every combination"
// caller wants. maxCombos guards against an accidental combinatorial
// explosion from the CLI (e.g. 5 axes × 10 values = 100,000 sub-backtests
// per window); a request over the limit is refused outright rather than
// silently truncated, since a silently-truncated grid would quietly
// change which parameters get considered without saying so.
func CartesianParamGrid(axes []ParamAxis, maxCombos int) ([]ParamSet, error) {
	if len(axes) == 0 {
		return nil, nil
	}
	if maxCombos <= 0 {
		maxCombos = 500
	}
	total := 1
	for _, a := range axes {
		if len(a.Values) == 0 {
			continue
		}
		total *= len(a.Values)
		if total > maxCombos {
			return nil, fmt.Errorf("backtest: parameter grid has >%d combinations (%d axes) — narrow --param before retrying", maxCombos, len(axes))
		}
	}

	grid := []ParamSet{{}}
	for _, a := range axes {
		if len(a.Values) == 0 {
			continue
		}
		var next []ParamSet
		for _, base := range grid {
			for _, v := range a.Values {
				ps := base.Clone()
				ps[a.Name] = v
				next = append(next, ps)
			}
		}
		grid = next
	}
	return grid, nil
}
