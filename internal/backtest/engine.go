package backtest

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"swingbot/internal/broker"
	"swingbot/internal/config"
	"swingbot/internal/domain"
	"swingbot/internal/risk"
	"swingbot/internal/strategy"
)

// RiskGate turns a raw entry Signal into a sized-or-rejected Decision
// (SPEC.md Bölüm 6.5.2). *risk.Gate (internal/risk, risk-management-
// engineer/Ajan 9) satisfies this directly — its Evaluate method already
// has exactly this shape — so Config.RiskGate normally just IS a
// *risk.Gate. The interface exists only so tests can supply a lightweight
// double (e.g. the golden buy-and-hold test's allInGate) without needing
// a full config.RiskConfig/domain.Market to drive risk.Sizer's formula.
type RiskGate interface {
	Evaluate(signal domain.Signal, in risk.GateInput) risk.Decision
}

// Config configures a single Run.
type Config struct {
	Strategy strategy.Strategy
	// Candles must hold full chronological OHLCV history (warmup bars
	// included) for every symbol Strategy might trade. The engine never
	// fetches data itself — that is internal/datafeed's job; the caller
	// (cmd/swingbot's `backtest` command) loads this from internal/store.
	Candles map[string][]domain.Candle
	// Universe, if non-empty, restricts which symbols are ever offered to
	// Strategy, overriding the per-day availability default (see
	// buildUniverse). Real per-date universe construction is
	// universe-scoring-engineer's job (Ajan 8, Faz 2) — until it lands,
	// callers either pass a fixed Universe or accept the "every symbol
	// with enough history today" default.
	Universe []string
	// Markets supplies each symbol's exchange filters (StepSize,
	// MinNotional) to RiskGate's sizing step. A symbol missing from this
	// map sizes against a zero-value domain.Market — risk.Sizer treats a
	// zero StepSize as "no rounding" and a zero MinNotional as "no floor",
	// so an incomplete map degrades gracefully rather than erroring.
	Markets map[string]domain.Market

	InitialCash float64
	Costs       broker.Costs
	// RiskGate defaults to risk.NewGate(config.RiskConfig{}, risk.NewSizer(config.RiskConfig{}))
	// if nil — i.e. SPEC.md Bölüm 8's documented defaults (risk.Gate/Sizer
	// fall back to those for any zero config field).
	RiskGate RiskGate
	// Breaker, if set, is checked once per day (after that day's fills,
	// before evaluating signals) and fed every closed trade, so a
	// backtest can trip and block new entries exactly like paper/live
	// would (SPEC.md Bölüm 6.5.3). Optional: nil means the breaker is
	// always considered closed.
	Breaker *risk.Breaker

	Mode string // "backtest" (default) — kept for ClosedTrade/Position.Strategy stamping parity with paper/live
}

// EquityPoint is one day's point-in-time portfolio mark, recorded right
// after that day's fills/stops are settled (SPEC.md Bölüm 6.7 adım 14).
type EquityPoint struct {
	Date     time.Time
	Equity   float64
	Cash     float64
	Exposure float64 // domain.Portfolio.Exposure(), 0..1
}

// Rejection records a signal RiskGate refused, so a report/panel can show
// "neden işlem yapılmadı" (SPEC.md Bölüm 6.5.2) instead of the entry
// silently vanishing.
type Rejection struct {
	AsOf   time.Time
	Symbol string
	Reason string
}

// Result is everything a single Run produced: enough to persist a
// store.Run row and to render the Bölüm 7.3 HTML report.
type Result struct {
	Strategy   string
	Params     map[string]any
	Start, End time.Time
	Costs      broker.Costs

	Equity     []EquityPoint
	Trades     []broker.ClosedTrade
	Rejections []Rejection
	Warnings   []string

	Metrics Metrics
}

// Run replays cfg.Strategy day-by-day over cfg.Candles (SPEC.md Bölüm 6.7,
// backtest form) and returns the resulting equity curve, trades and
// metrics (with BTC and equal-weight top-10 benchmarks attached — İ3).
//
// The day loop, per calendar day t (see buildCalendar):
//  1. broker.Advance(ctx, t) — settles yesterday's pending orders at t's
//     open and checks resting stops against t's low (İ2, gap rule; see
//     internal/broker/paper.go).
//  2. Record today's equity mark.
//  3. Build Input.Series with EVERY symbol's history truncated to t
//     (len==cap, so an out-of-bounds read panics instead of silently
//     seeing tomorrow — this is İ2 enforced at the type level, not by
//     convention; see truncateSeries).
//  4. cfg.Strategy.Evaluate(input).
//  5. Stop-level updates and exits are submitted BEFORE entries (SPEC.md
//     Bölüm 6.7 adım 8) — both settle on t+1's Advance, same as entries;
//     but a strategy that shrinks its own book before growing it must be
//     honored in that order regardless of settlement timing, since a
//     future engine revision may make same-bar netting possible and this
//     ordering is the one invariant SPEC.md pins down explicitly.
//  6. Entries are sized via cfg.RiskGate and submitted as a market buy
//     plus a resting stop_market at the signal's StopPrice.
func Run(ctx context.Context, cfg Config) (*Result, error) {
	if cfg.Strategy == nil {
		return nil, fmt.Errorf("backtest: Config.Strategy is required")
	}
	if len(cfg.Candles) == 0 {
		return nil, fmt.Errorf("backtest: Config.Candles is empty")
	}
	if err := cfg.Costs.Validate(); err != nil {
		return nil, err
	}
	mode := cfg.Mode
	if mode == "" {
		mode = "backtest"
	}
	riskGate := cfg.RiskGate
	if riskGate == nil {
		riskGate = risk.NewGate(config.RiskConfig{}, risk.NewSizer(config.RiskConfig{}))
	}

	calendar := buildCalendar(cfg.Candles)
	dateIndex := buildDateIndex(cfg.Candles)

	warmup := cfg.Strategy.WarmupBars()
	if warmup < 0 {
		warmup = 0
	}
	if len(calendar) <= warmup+1 {
		return nil, fmt.Errorf("backtest: not enough calendar days (%d) for WarmupBars (%d) plus at least one evaluated day", len(calendar), warmup)
	}

	clock := broker.NewBacktestClock(calendar[0])
	pb, err := broker.NewPaperBroker(mode, cfg.Candles, cfg.InitialCash, cfg.Costs, clock, cfg.Strategy.Name())
	if err != nil {
		return nil, err
	}

	res := &Result{
		Strategy: cfg.Strategy.Name(),
		Params:   cfg.Strategy.Params(),
		Start:    calendar[warmup+1],
		End:      calendar[len(calendar)-1],
		Costs:    cfg.Costs,
	}

	for i := 0; i <= warmup; i++ {
		// Prime the broker/clock through the warmup window so the first
		// evaluated day's Advance call sees a broker already positioned
		// at the right cursor; nothing can fill yet (no orders exist).
		if err := pb.Advance(ctx, calendar[i]); err != nil {
			return nil, fmt.Errorf("backtest: priming warmup day %s: %w", calendar[i], err)
		}
	}

	lastExitAt := make(map[string]time.Time)
	tradesSeen := 0

	for i := warmup + 1; i < len(calendar); i++ {
		t := calendar[i]
		if err := pb.Advance(ctx, t); err != nil {
			return nil, fmt.Errorf("backtest: advancing to %s: %w", t, err)
		}

		portfolio, err := pb.Portfolio(ctx)
		if err != nil {
			return nil, fmt.Errorf("backtest: reading portfolio at %s: %w", t, err)
		}
		res.Equity = append(res.Equity, EquityPoint{
			Date: t, Equity: portfolio.Equity, Cash: portfolio.Cash, Exposure: portfolio.Exposure(),
		})

		// Feed every trade that closed as of today (signal exits AND
		// gap-rule stop-outs both land in pb.ClosedTrades()) into the
		// breaker and the per-symbol cooldown clock, BEFORE today's
		// entries are evaluated against either — SPEC.md Bölüm 6.5.2's
		// cooldown rule and Bölüm 6.5.3's breaker must see today's exits.
		// ClosedTradesSince (not ClosedTrades) so this daily poll copies
		// only today's new trades, not the whole ever-growing history.
		newTrades := pb.ClosedTradesSince(tradesSeen)
		for _, tr := range newTrades {
			lastExitAt[tr.Symbol] = tr.ExitTime
			if cfg.Breaker != nil {
				cfg.Breaker.RecordTrade(risk.TradeResult{ClosedAt: tr.ExitTime, PnL: tr.PnLQuote})
			}
		}
		tradesSeen += len(newTrades)

		breakerOpen := false
		if cfg.Breaker != nil {
			breakerOpen = cfg.Breaker.Check(portfolio.Equity, t).Open
		}

		series, universe := buildSeriesAndUniverse(cfg.Candles, dateIndex, cfg.Universe, t, warmup)
		if len(universe) == 0 {
			continue
		}

		signals, err := cfg.Strategy.Evaluate(strategy.Input{
			AsOf: t, Series: series, Universe: universe, Portfolio: portfolio,
		})
		if err != nil {
			return nil, fmt.Errorf("backtest: %s.Evaluate at %s: %w", cfg.Strategy.Name(), t, err)
		}

		var stopsAndExits, entries []domain.Signal
		for _, s := range signals {
			if s.Kind == domain.SignalEnter {
				entries = append(entries, s)
			} else {
				stopsAndExits = append(stopsAndExits, s)
			}
		}

		for _, s := range stopsAndExits {
			if err := applyExitOrStop(ctx, pb, s, portfolio); err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("%s %s: %v", t.Format("2006-01-02"), s.Symbol, err))
			}
		}

		for _, s := range entries {
			decision := riskGate.Evaluate(s, risk.GateInput{
				Portfolio: portfolio, Market: cfg.Markets[s.Symbol], Now: t,
				LastExitAt: lastExitAt[s.Symbol], BreakerOpen: breakerOpen,
			})
			if !decision.Approved {
				res.Rejections = append(res.Rejections, Rejection{AsOf: t, Symbol: s.Symbol, Reason: decision.Reason})
				continue
			}
			if err := submitEntry(ctx, pb, s, decision.Size.Qty); err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("%s %s: %v", t.Format("2006-01-02"), s.Symbol, err))
			}
		}
	}

	res.Trades = pb.ClosedTrades()
	res.Metrics = Compute(res.Equity, res.Trades)
	return res, nil
}

// applyExitOrStop submits the broker order a SignalExit or SignalStop
// signal implies. A SignalExit needs the position's current quantity
// (Signal itself carries no Qty — SPEC.md İ6/domain.Signal doc: sizing is
// never the strategy's job, so quantity for an exit always comes from
// what is actually held, not from the signal).
func applyExitOrStop(ctx context.Context, pb *broker.PaperBroker, s domain.Signal, portfolio domain.Portfolio) error {
	switch s.Kind {
	case domain.SignalStop:
		if s.StopPrice <= 0 {
			return fmt.Errorf("stop_update with non-positive StopPrice")
		}
		_, err := pb.Submit(ctx, domain.OrderRequest{
			ClientOrderID: clientOrderID(s.AsOf, s.Symbol, "stop"),
			Symbol:        s.Symbol,
			Side:          domain.SideSell,
			Type:          "stop_market",
			Price:         decimal.NewFromFloat(s.StopPrice),
		})
		return err

	case domain.SignalExit:
		pos, open := portfolio.Positions[s.Symbol]
		if !open {
			return fmt.Errorf("exit signal for a symbol with no open position")
		}
		_, err := pb.Submit(ctx, domain.OrderRequest{
			ClientOrderID: clientOrderID(s.AsOf, s.Symbol, "exit"),
			Symbol:        s.Symbol,
			Side:          domain.SideSell,
			Type:          "market",
			Qty:           pos.Qty,
		})
		return err

	default:
		return fmt.Errorf("unexpected signal kind %q in applyExitOrStop", s.Kind)
	}
}

// submitEntry places the market buy plus its protective resting stop.
// PaperBroker attaches the stop to the position the instant the buy fills
// (see broker.PaperBroker.pendingStops) even though both orders are
// submitted "today" and neither fills until t+1.
func submitEntry(ctx context.Context, pb *broker.PaperBroker, s domain.Signal, qty decimal.Decimal) error {
	if s.StopPrice <= 0 {
		return fmt.Errorf("enter signal missing StopPrice (SPEC.md 5.1: zorunlu)")
	}
	if _, err := pb.Submit(ctx, domain.OrderRequest{
		ClientOrderID: clientOrderID(s.AsOf, s.Symbol, "entry"),
		Symbol:        s.Symbol,
		Side:          domain.SideBuy,
		Type:          "market",
		Qty:           qty,
	}); err != nil {
		return err
	}
	_, err := pb.Submit(ctx, domain.OrderRequest{
		ClientOrderID: clientOrderID(s.AsOf, s.Symbol, "stop"),
		Symbol:        s.Symbol,
		Side:          domain.SideSell,
		Type:          "stop_market",
		Price:         decimal.NewFromFloat(s.StopPrice),
	})
	return err
}

func clientOrderID(asOf time.Time, symbol, kind string) string {
	return fmt.Sprintf("bt-%s-%s-%s", asOf.UTC().Format("20060102"), strings.ReplaceAll(symbol, "/", ""), kind)
}

// buildCalendar returns the sorted, deduplicated union of every OpenTime
// across every symbol's candles. Symbols may be listed on different days
// (SPEC.md survivorship-bias handling keeps delisted rows, not every
// symbol trades from day one) — the calendar spans the union so a newly
// listed symbol is picked up the day it appears, not held back until
// every other symbol also has data.
func buildCalendar(candles map[string][]domain.Candle) []time.Time {
	seen := make(map[int64]time.Time)
	for _, series := range candles {
		for _, c := range series {
			seen[c.OpenTime.UnixMilli()] = c.OpenTime
		}
	}
	out := make([]time.Time, 0, len(seen))
	for _, t := range seen {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

// buildDateIndex maps each symbol's OpenTime (unix ms) to its index in
// that symbol's own chronological slice, so buildSeriesAndUniverse can
// look up "symbol's bar index as of t" in O(1) instead of scanning.
func buildDateIndex(candles map[string][]domain.Candle) map[string]map[int64]int {
	idx := make(map[string]map[int64]int, len(candles))
	for sym, series := range candles {
		m := make(map[int64]int, len(series))
		for i, c := range series {
			m[c.OpenTime.UnixMilli()] = i
		}
		idx[sym] = m
	}
	return idx
}

// buildSeriesAndUniverse returns, for calendar day t, every symbol's
// history truncated through t (see truncateSeries) and the list of
// symbols eligible to trade today: present in restrictUniverse (if given)
// or in cfg.Candles (all symbols) otherwise, AND with at least warmup+1
// closed bars as of t (SPEC.md Bölüm 5.4 WarmupBars contract). A symbol
// missing a bar exactly on t (holiday-less crypto markets make this rare,
// but a data gap is always possible) is silently excluded for that day
// rather than failing the whole run — SPEC.md Bölüm 14 treats a single
// symbol's data gap as a data-quality concern for datafeed.Verify to
// flag, not a reason to abort a multi-symbol backtest.
func buildSeriesAndUniverse(
	candles map[string][]domain.Candle,
	dateIndex map[string]map[int64]int,
	restrictUniverse []string,
	t time.Time,
	warmup int,
) (map[string][]domain.Candle, []string) {
	candidates := restrictUniverse
	if len(candidates) == 0 {
		candidates = make([]string, 0, len(candles))
		for sym := range candles {
			candidates = append(candidates, sym)
		}
		sort.Strings(candidates)
	}

	series := make(map[string][]domain.Candle, len(candidates))
	universe := make([]string, 0, len(candidates))
	key := t.UnixMilli()
	for _, sym := range candidates {
		i, ok := dateIndex[sym][key]
		if !ok || i < warmup {
			continue
		}
		series[sym] = truncateSeries(candles[sym], i)
		universe = append(universe, sym)
	}
	return series, universe
}

// truncateSeries returns full[:i+1] with its capacity ALSO clamped to
// i+1 (the Go three-index slice expression). That is what makes İ2 a
// runtime guarantee rather than a convention: full[:i+1] alone would
// still let a strategy re-slice past i+1 up to full's original capacity
// and read tomorrow's bars; clamping cap forces any such attempt —
// direct indexing or re-slicing — to panic immediately.
func truncateSeries(full []domain.Candle, i int) []domain.Candle {
	return full[: i+1 : i+1]
}
