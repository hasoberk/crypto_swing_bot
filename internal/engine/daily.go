// Package engine implements SPEC.md Bölüm 6.7's daily loop for paper
// trading: it wires the SAME strategy/risk/broker code the backtest engine
// (internal/backtest) exercises against internal/broker.PaperBroker, fed by
// live-updated candle data instead of a fixed historical slice (İ1 — SPEC.md
// Bölüm 1.2: "Backtest, paper trading ve canlı işlem aynı strateji ve risk
// kodunu çalıştırır").
//
// # Restart resilience by reconstruction, not by snapshotting
//
// PaperBroker holds all of its state (cash, open positions, pending orders)
// in memory and only ever starts from a fixed initial cash figure — it has
// no "resume from this portfolio" constructor. Rather than bolt state
// snapshotting onto a package this agent does not own
// (internal/broker/paper.go), Engine treats that as a feature: every call
// to reconstruct (used by both RunOnce and a cold start after a crash)
// rebuilds a fresh PaperBroker from calendar day zero and REPLAYS every
// order this engine has ever actually placed, using store.Proposal rows
// (the only durable record of what was decided) as the script. Two
// consequences fall out of that on purpose:
//   - A process restart is not a special case. The exact same reconstruct
//     call that runs at the top of every single day's RunOnce is what a
//     cold start after a crash runs too — there is no separate "recovery
//     path" to keep in sync with the normal one (İ1's spirit, applied to
//     this package's own internal seam).
//   - Replay is driven by facts already committed to the store (decided
//     proposals), never by re-running Strategy.Evaluate against today's
//     data for a past day — a strategy parameter change between restarts
//     must never silently rewrite history.
//
// The one known gap this leaves: a resting stop_market amendment that is
// itself never rejected/approved by a human (a trailing-stop tightening,
// SPEC.md domain.Signal Kind==SignalStop) is persisted as a
// side="stop_update" store.Proposal purely so reconstruct can replay it —
// SPEC.md Bölüm 4.1's proposals.side comment only documents "long"/"exit"
// for the two states a human ever approves/rejects, but the column has no
// DB-level CHECK constraint, so this is a safe, additive, engine-internal
// convention rather than a schema change. See resubmitDecidedProposal.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"swingbot/internal/backtest"
	"swingbot/internal/broker"
	"swingbot/internal/config"
	"swingbot/internal/datafeed"
	"swingbot/internal/domain"
	"swingbot/internal/notify"
	"swingbot/internal/risk"
	"swingbot/internal/store"
	"swingbot/internal/strategy"
	"swingbot/internal/universe"
)

// Config bundles every dependency the daily loop needs. Every field is
// mandatory unless its doc comment says otherwise — New returns an error
// rather than letting a nil dependency panic three steps into RunOnce.
type Config struct {
	Store    *store.Store
	Feed     *datafeed.Feed // backs step 2/3 (datafeed.Update/Verify)
	Strategy strategy.Strategy
	Notifier notify.Notifier

	// RiskGate is shared, unmodified risk.Gate/Sizer code (İ1) — construct
	// it exactly as `swingbot backtest` does:
	// risk.NewGate(cfg.Risk, risk.NewSizer(cfg.Risk)).
	RiskGate *risk.Gate
	// BreakerCfg is the `breaker:` section of config.yaml. Engine builds a
	// fresh *risk.Breaker from it on every reconstruct call (see the
	// package doc comment) rather than taking a long-lived *risk.Breaker
	// from the caller — a breaker's trade/error history must always be
	// exactly what reconstruction just replayed, never anything a caller
	// might have accumulated separately.
	BreakerCfg config.BreakerConfig

	Costs       broker.Costs
	InitialCash float64

	UniverseParams  universe.FilterParams
	UniverseWeights universe.Weights

	Timeframe string // e.g. "1d" — config.yaml data.timeframe
	Quote     string // e.g. "USDT" — config.yaml exchange.quote; also the BTC benchmark's quote asset

	ApprovalTTL  time.Duration // config.yaml execution.approval_ttl_hours; default 4h
	RunAtUTC     string        // config.yaml execution.run_at_utc; default "00:05"
	PollInterval time.Duration // how often the approval wait re-checks Approvals(); default 30s

	// Clock drives scheduling (when to run) and the approval-wait poll
	// loop. Defaults to broker.SystemClock{} — tests inject a fake to
	// avoid real sleeping.
	Clock broker.Clock
}

func (c Config) withDefaults() Config {
	if c.Timeframe == "" {
		c.Timeframe = "1d"
	}
	if c.Quote == "" {
		c.Quote = "USDT"
	}
	if c.ApprovalTTL <= 0 {
		c.ApprovalTTL = 4 * time.Hour
	}
	if c.RunAtUTC == "" {
		c.RunAtUTC = "00:05"
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 30 * time.Second
	}
	if c.Clock == nil {
		c.Clock = broker.SystemClock{}
	}
	return c
}

func (c Config) validate() error {
	switch {
	case c.Store == nil:
		return fmt.Errorf("engine: Config.Store is required")
	case c.Feed == nil:
		return fmt.Errorf("engine: Config.Feed is required")
	case c.Strategy == nil:
		return fmt.Errorf("engine: Config.Strategy is required")
	case c.Notifier == nil:
		return fmt.Errorf("engine: Config.Notifier is required")
	case c.RiskGate == nil:
		return fmt.Errorf("engine: Config.RiskGate is required")
	case c.InitialCash <= 0:
		return fmt.Errorf("engine: Config.InitialCash must be positive")
	}
	return c.Costs.Validate() // İ4
}

// breakerStateKey is the system_state key SPEC.md Bölüm 6.5.3 writes to
// when the breaker trips. cmd/swingbot/main.go established this exact
// string + JSON-encoded risk.State wire format for `breaker status|reset`
// before this package existed; Engine reuses it rather than inventing a
// second encoding for the same value.
const breakerStateKey = "breaker"

// orderErrorsStateKey is the system_state key Engine persists risk.Breaker's
// order-error history under, for the max_order_errors_24h trip condition
// (SPEC.md Bölüm 6.5.3 rule 4) to survive a process restart.
//
// risk.Breaker.RecordOrderError only ever appends to an in-memory slice
// (see that package's doc comment) and reconstruct always builds a brand
// new *risk.Breaker (see Config.BreakerCfg's doc comment) — without this,
// a crash right after an order failure would make the next reconstruction
// forget it ever happened, letting the breaker evaluate rule 4 more
// optimistically than it should right when a restart is already the
// riskiest possible moment to do so.
//
// This mirrors persistBreakerState's own choice of system_state over a new
// table/migration: order volume here is small (order FAILURES, not every
// order), so a JSON blob under one more system_state key is simpler than
// wiring internal/store's orders table (currently unused by this package
// for anything) into the breaker's restart path.
const orderErrorsStateKey = "order_errors"

// Engine drives SPEC.md Bölüm 6.7's daily loop. It holds no portfolio
// state of its own between calls — see the package doc comment — so a
// zero-value Engine plus a fresh New call is exactly as capable as one
// that has been running for months.
type Engine struct {
	cfg Config
}

// New validates cfg and returns a ready Engine. It performs no I/O.
func New(cfg Config) (*Engine, error) {
	cfg = cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Engine{cfg: cfg}, nil
}

// daySnapshot is the reconstructed state reconstruct() hands to a single
// RunOnce/ResumePending call: a PaperBroker replayed all the way through
// the calendar it was given, plus everything derived from that replay
// (breaker state, per-symbol cooldown clock, and — for the calendar's
// LAST day only — the trades that closed on it, so RunOnce can report
// "stop tetiklendi" for TODAY without re-notifying about years of replayed
// history on every restart).
type daySnapshot struct {
	pb        *broker.PaperBroker
	calendar  []time.Time
	candles   map[string][]domain.Candle
	dateIndex map[string]map[int64]int
	markets   map[string]domain.Market

	breaker    *risk.Breaker
	lastExitAt map[string]time.Time
	// orderErrors is the trailing-24h-pruned order-error history reconstruct
	// loaded from orderErrorsStateKey and fed into breaker — see
	// recordOrderError, the only place this is appended to afterwards.
	orderErrors []risk.OrderError

	asOf               time.Time // calendar[len(calendar)-1]
	portfolio          domain.Portfolio
	todaysClosedTrades []broker.ClosedTrade
}

// reconstruct rebuilds the entire paper-trading state from the store, per
// the package doc comment. It is the single seam every entry point
// (RunOnce, ResumePending) goes through — there is deliberately no faster
// "just apply today's delta" path, so a restart and a normal day are
// indistinguishable to the rest of this package.
func (e *Engine) reconstruct(ctx context.Context) (*daySnapshot, error) {
	rows, err := e.cfg.Store.ListMarkets(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("engine: list markets: %w", err)
	}
	markets, symbols := marketsBySymbol(rows, e.cfg.Quote)
	if len(symbols) == 0 {
		return nil, fmt.Errorf("engine: quote %q için hiç piyasa yok — önce `swingbot data backfill` çalıştırın", e.cfg.Quote)
	}

	candles, err := e.cfg.Store.GetCandlesForSymbols(ctx, symbols, e.cfg.Timeframe, time.Time{}, time.Time{})
	if err != nil {
		return nil, fmt.Errorf("engine: get candles: %w", err)
	}
	if len(candles) == 0 {
		return nil, fmt.Errorf("engine: veritabanında mum verisi yok — önce `swingbot data backfill` çalıştırın")
	}

	calendar := buildCalendar(candles)
	if len(calendar) == 0 {
		return nil, fmt.Errorf("engine: mum takvimi boş")
	}
	dateIndex := buildDateIndex(candles)

	clock := broker.NewBacktestClock(calendar[0])
	pb, err := broker.NewPaperBroker("paper", candles, e.cfg.InitialCash, e.cfg.Costs, clock, e.cfg.Strategy.Name())
	if err != nil {
		return nil, fmt.Errorf("engine: construct paper broker: %w", err)
	}

	decided, err := e.loadDecidedProposalsByDay(ctx)
	if err != nil {
		return nil, fmt.Errorf("engine: load decided proposals: %w", err)
	}

	brk := risk.NewBreaker(e.cfg.BreakerCfg)
	orderErrors, err := e.loadOrderErrors(ctx, e.cfg.Clock.Now())
	if err != nil {
		return nil, fmt.Errorf("engine: load order errors: %w", err)
	}
	for _, oe := range orderErrors {
		brk.RecordOrderError(oe)
	}
	lastExitAt := make(map[string]time.Time)
	tradesSeen := 0
	var todaysClosedTrades []broker.ClosedTrade

	for i, day := range calendar {
		if err := pb.Advance(ctx, day); err != nil {
			return nil, fmt.Errorf("engine: advance to %s: %w", day, err)
		}
		for _, p := range decided[day.UnixMilli()] {
			if _, err := resubmitDecidedProposal(ctx, pb, p); err != nil {
				// A historical resubmit failing must not abort the whole
				// reconstruction — the position/order it concerns is
				// simply absent from today's book, same as
				// PaperBroker.Submit's own data-gap tolerance.
				continue
			}
		}

		newTrades := pb.ClosedTradesSince(tradesSeen)
		tradesSeen += len(newTrades)
		for _, tr := range newTrades {
			lastExitAt[tr.Symbol] = tr.ExitTime
			brk.RecordTrade(risk.TradeResult{ClosedAt: tr.ExitTime, PnL: tr.PnLQuote})
		}

		portfolio, err := pb.Portfolio(ctx)
		if err != nil {
			return nil, fmt.Errorf("engine: portfolio at %s: %w", day, err)
		}
		brk.Check(portfolio.Equity, day)

		if i == len(calendar)-1 {
			todaysClosedTrades = newTrades
		}
	}

	finalPortfolio, err := pb.Portfolio(ctx)
	if err != nil {
		return nil, fmt.Errorf("engine: final portfolio: %w", err)
	}

	return &daySnapshot{
		pb: pb, calendar: calendar, candles: candles, dateIndex: dateIndex, markets: markets,
		breaker: brk, lastExitAt: lastExitAt, orderErrors: orderErrors,
		asOf: calendar[len(calendar)-1], portfolio: finalPortfolio, todaysClosedTrades: todaysClosedTrades,
	}, nil
}

// loadOrderErrors reads risk.Breaker's persisted order-error history (see
// orderErrorsStateKey's doc comment), pruned to the trailing 24h as of now.
// Pruning on load (in addition to risk.Breaker.Check's own pruning of its
// in-memory copy) keeps the system_state blob itself from growing without
// bound across a long-running paper/live bot.
func (e *Engine) loadOrderErrors(ctx context.Context, now time.Time) ([]risk.OrderError, error) {
	raw, ok, err := e.cfg.Store.GetState(ctx, orderErrorsStateKey)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	var errs []risk.OrderError
	if err := json.Unmarshal([]byte(raw), &errs); err != nil {
		return nil, fmt.Errorf("decode %s state: %w", orderErrorsStateKey, err)
	}
	cutoff := now.Add(-24 * time.Hour)
	kept := errs[:0]
	for _, oe := range errs {
		if !oe.At.Before(cutoff) {
			kept = append(kept, oe)
		}
	}
	return kept, nil
}

// recordOrderError implements SPEC.md Bölüm 6.5.3 rule 4's restart
// durability: it records at both in snap.breaker's in-memory history (so
// the very next Check call in THIS run already sees it — unchanged
// behavior) and in snap.orderErrors, which it then re-prunes to the
// trailing 24h and persists to orderErrorsStateKey so a freshly
// reconstructed Engine (after a crash or a normal restart) feeds the exact
// same history back into a brand new *risk.Breaker — see reconstruct.
//
// A persistence failure here is only ever reported via notify, never
// returned: it must not mask the order failure the caller is already in
// the middle of handling, and the in-memory record (which is what THIS
// run's own Check calls consult) is unaffected either way.
func (e *Engine) recordOrderError(ctx context.Context, snap *daySnapshot, at time.Time) {
	snap.breaker.RecordOrderError(risk.OrderError{At: at})

	snap.orderErrors = append(snap.orderErrors, risk.OrderError{At: at})
	cutoff := at.Add(-24 * time.Hour)
	kept := snap.orderErrors[:0]
	for _, oe := range snap.orderErrors {
		if !oe.At.Before(cutoff) {
			kept = append(kept, oe)
		}
	}
	snap.orderErrors = kept

	raw, err := json.Marshal(snap.orderErrors)
	if err != nil {
		e.notify(ctx, notify.LevelWarning, "Sipariş hata geçmişi kaydedilemedi", err.Error())
		return
	}
	if err := e.cfg.Store.SetState(ctx, orderErrorsStateKey, string(raw)); err != nil {
		e.notify(ctx, notify.LevelWarning, "Sipariş hata geçmişi kaydedilemedi", err.Error())
	}
}

// syncTrades syncs the store's durable trades table (what /api/positions
// and /api/trades read — SPEC.md Bölüm 7.1) to pb's current truth. Callers
// invoke this at the true end of their work (after RunOnce's exit/stop and
// submitApproved steps, or unconditionally at the end of ResumePending) —
// not from inside reconstruct itself — since both of those can still submit
// new orders into pb after reconstruct's own replay loop has already run.
func (e *Engine) syncTrades(ctx context.Context, pb *broker.PaperBroker) error {
	portfolio, err := pb.Portfolio(ctx)
	if err != nil {
		return fmt.Errorf("engine: sync trades: portfolio: %w", err)
	}
	if err := e.cfg.Store.ReplaceTrades(ctx, "paper", tradeRows(portfolio, pb.ClosedTrades())); err != nil {
		return fmt.Errorf("engine: sync trades: %w", err)
	}
	return nil
}

// tradeRows converts a freshly-reconstructed PaperBroker's truth (open
// positions + every closed round-trip) into the store.Trade rows
// ReplaceTrades should sync to. IDs are deterministic within a single call
// (derived from symbol, which PaperBroker guarantees at most one open
// position for, and from closed's own slice index) — reconstruct always
// passes the complete set to ReplaceTrades, which deletes-then-inserts, so
// these only ever need to be unique within that one call, not stable across
// calls.
func tradeRows(portfolio domain.Portfolio, closed []broker.ClosedTrade) []store.Trade {
	rows := make([]store.Trade, 0, len(portfolio.Positions)+len(closed))
	for i, tr := range closed {
		exitPrice := tr.ExitPrice
		pnlQuote := tr.PnLQuote
		pnlPct := tr.PnLPct
		rows = append(rows, store.Trade{
			ID:         fmt.Sprintf("paper-closed-%d-%s", i, tr.Symbol),
			Symbol:     tr.Symbol,
			Strategy:   tr.Strategy,
			EntryTime:  tr.EntryTime,
			EntryPrice: tr.EntryPrice,
			ExitTime:   tr.ExitTime,
			ExitPrice:  &exitPrice,
			Qty:        tr.Qty.String(),
			Fees:       tr.Fees,
			PnLQuote:   &pnlQuote,
			PnLPct:     &pnlPct,
			ExitReason: tr.ExitReason,
			Mode:       "paper",
		})
	}
	for symbol, pos := range portfolio.Positions {
		rows = append(rows, store.Trade{
			ID:         "paper-open-" + symbol,
			Symbol:     symbol,
			Strategy:   pos.Strategy,
			EntryTime:  pos.EntryTime,
			EntryPrice: pos.EntryPrice,
			Qty:        pos.Qty.String(),
			Mode:       "paper",
		})
	}
	return rows
}

// loadDecidedProposalsByDay returns every proposal that ever resulted in an
// actual broker submission (APPROVED, SUBMITTED or FILLED — see the Status
// doc comments in store/proposals.go), bucketed by AsOf day and sorted by
// CreatedAt so reconstruct replays them in the order they originally
// happened.
func (e *Engine) loadDecidedProposalsByDay(ctx context.Context) (map[int64][]store.Proposal, error) {
	out := make(map[int64][]store.Proposal)
	for _, status := range []store.ProposalStatus{store.ProposalApproved, store.ProposalSubmitted, store.ProposalFilled} {
		rows, err := e.cfg.Store.ListProposalsByStatus(ctx, status)
		if err != nil {
			return nil, err
		}
		for _, p := range rows {
			key := p.AsOf.UnixMilli()
			out[key] = append(out[key], p)
		}
	}
	for key := range out {
		bucket := out[key]
		sort.Slice(bucket, func(i, j int) bool {
			if !bucket[i].CreatedAt.Equal(bucket[j].CreatedAt) {
				return bucket[i].CreatedAt.Before(bucket[j].CreatedAt)
			}
			return bucket[i].ID < bucket[j].ID
		})
	}
	return out, nil
}

// resubmitDecidedProposal turns a decided store.Proposal back into the
// broker.Submit call(s) it originally caused. It is used both by
// reconstruct's replay loop and — for a proposal decided just now — by
// RunOnce's step 13, so there is exactly one place that knows how a
// Proposal's fields map onto domain.OrderRequest.
func resubmitDecidedProposal(ctx context.Context, pb *broker.PaperBroker, p store.Proposal) (orderID string, err error) {
	qty, err := decimal.NewFromString(p.Qty)
	if err != nil {
		return "", fmt.Errorf("proposal %s: invalid qty %q: %w", p.ID, p.Qty, err)
	}

	switch p.Side {
	case "long":
		entry, err := pb.Submit(ctx, domain.OrderRequest{
			ClientOrderID: clientOrderID(p.ID, "entry"), Symbol: p.Symbol, Side: domain.SideBuy, Type: "market", Qty: qty,
		})
		if err != nil {
			return "", err
		}
		if p.StopPrice == nil || *p.StopPrice <= 0 {
			return entry.ID, fmt.Errorf("proposal %s: missing stop price", p.ID)
		}
		if _, err := pb.Submit(ctx, domain.OrderRequest{
			ClientOrderID: clientOrderID(p.ID, "stop"), Symbol: p.Symbol, Side: domain.SideSell, Type: "stop_market",
			Price: decimal.NewFromFloat(*p.StopPrice),
		}); err != nil {
			return entry.ID, err
		}
		return entry.ID, nil

	case "exit":
		o, err := pb.Submit(ctx, domain.OrderRequest{
			ClientOrderID: clientOrderID(p.ID, "exit"), Symbol: p.Symbol, Side: domain.SideSell, Type: "market", Qty: qty,
		})
		return o.ID, err

	case "stop_update":
		// See the package doc comment: this is an engine-internal
		// extension of proposals.side used purely so a trailing-stop
		// tightening survives reconstruction, never shown to the operator
		// via notify.ProposeTrade.
		if p.StopPrice == nil || *p.StopPrice <= 0 {
			return "", fmt.Errorf("proposal %s: stop_update missing stop price", p.ID)
		}
		o, err := pb.Submit(ctx, domain.OrderRequest{
			ClientOrderID: clientOrderID(p.ID, "stop"), Symbol: p.Symbol, Side: domain.SideSell, Type: "stop_market",
			Price: decimal.NewFromFloat(*p.StopPrice),
		})
		return o.ID, err

	default:
		return "", fmt.Errorf("proposal %s: unknown side %q", p.ID, p.Side)
	}
}

// clientOrderID derives a stable, deterministic ClientOrderID (İ5) from a
// proposal ID and an order role ("entry", "stop", "exit") — stable so
// reconstruct's replay always derives the exact same ID for the exact same
// historical order.
func clientOrderID(proposalID, kind string) string {
	return fmt.Sprintf("px-%s-%s", proposalID, kind)
}

// RunOnce executes SPEC.md Bölüm 6.7's 15-step daily cycle once, for
// "today" as derived from cfg.Clock.Now(). It is idempotent for a given
// calendar day: if an equity_snapshot already exists for that day (i.e. a
// previous call already completed it), RunOnce returns nil immediately
// without re-evaluating the strategy or re-sending any notification.
func (e *Engine) RunOnce(ctx context.Context) error {
	now := e.cfg.Clock.Now()
	asOf := candleDay(now)

	already, err := e.alreadyProcessed(ctx, asOf)
	if err != nil {
		return fmt.Errorf("engine: already-processed check: %w", err)
	}
	if already {
		return nil
	}

	// 2. datafeed.Update()
	if _, err := e.cfg.Feed.Update(ctx); err != nil {
		e.notify(ctx, notify.LevelCritical, "Veri güncelleme hatası", err.Error())
		return fmt.Errorf("engine: datafeed update: %w", err)
	}

	// 3. datafeed.Verify() — başarısızsa dur + bildir.
	verify, err := e.cfg.Feed.Verify(ctx, nil)
	if err != nil {
		e.notify(ctx, notify.LevelCritical, "Veri doğrulama hatası", err.Error())
		return fmt.Errorf("engine: datafeed verify: %w", err)
	}
	if !verify.OK {
		detail := fmt.Sprintf("%d kritik veri kalitesi bulgusu — günlük döngü durduruldu.", verify.CriticalIssues)
		e.notify(ctx, notify.LevelCritical, "Veri kalitesi hatası", detail)
		return fmt.Errorf("engine: data verify: %d kritik hata (SPEC.md Bölüm 6.7 adım 3)", verify.CriticalIssues)
	}

	// 4. broker.Portfolio() + 5. breaker.Check(portfolio) — both are a
	// side effect of reconstruct's replay loop reaching `asOf` as its last
	// day (see daySnapshot's doc comment).
	snap, err := e.reconstruct(ctx)
	if err != nil {
		return fmt.Errorf("engine: reconstruct: %w", err)
	}
	if !snap.asOf.Equal(asOf) {
		return fmt.Errorf("engine: en güncel mum tarihi %s, beklenen %s ile eşleşmiyor (data update/backfill kontrol edin)",
			snap.asOf.Format("2006-01-02"), asOf.Format("2006-01-02"))
	}

	if err := e.persistBreakerState(ctx, snap.breaker); err != nil {
		return fmt.Errorf("engine: persist breaker state: %w", err)
	}
	e.notifyStopTriggers(ctx, snap.todaysClosedTrades)

	// 6. universe.Build(asOf)
	params := e.cfg.UniverseParams
	params.WarmupBars = e.cfg.Strategy.WarmupBars()
	uResult, err := universe.Build(ctx, e.cfg.Store, e.cfg.Timeframe, asOf, params, e.cfg.UniverseWeights)
	if err != nil {
		return fmt.Errorf("engine: universe.Build: %w", err)
	}

	// 7. strategy.Evaluate(input)
	series, uni := seriesAndUniverse(snap.candles, snap.dateIndex, uResult.Symbols(), asOf)
	signals, err := e.cfg.Strategy.Evaluate(strategy.Input{AsOf: asOf, Series: series, Universe: uni, Portfolio: snap.portfolio})
	if err != nil {
		return fmt.Errorf("engine: %s.Evaluate: %w", e.cfg.Strategy.Name(), err)
	}

	var exits, entries []domain.Signal
	for _, s := range signals {
		if s.Kind == domain.SignalEnter {
			entries = append(entries, s)
		} else {
			exits = append(exits, s)
		}
	}

	// 8. stop ve çıkış sinyallerini ÖNCE işle.
	for _, s := range exits {
		if err := e.processExitOrStop(ctx, snap, s, now, asOf); err != nil {
			e.notify(ctx, notify.LevelWarning, fmt.Sprintf("Emir hatası · %s", s.Symbol), err.Error())
			e.recordOrderError(ctx, snap, now)
		}
	}
	snap.breaker.Check(snap.portfolio.Equity, now) // re-evaluate breaker.max_order_errors_24h before sizing entries

	// 9. risk.Gate + risk.Size → sinyaller → öneriler; 10. proposals
	// tablosuna PENDING yaz; 11. notify.ProposeTrade.
	var pending []store.Proposal
	for _, s := range entries {
		row, notifyErr := e.proposeEntry(ctx, snap, s, now, asOf)
		if notifyErr != nil {
			e.notify(ctx, notify.LevelWarning, fmt.Sprintf("Telegram bildirimi gönderilemedi · %s", s.Symbol), notifyErr.Error())
		}
		if row != nil {
			pending = append(pending, *row)
		}
	}

	// 12. onay bekle (timeout: approval_ttl).
	approved, err := e.awaitDecisions(ctx, pending)
	if err != nil {
		return fmt.Errorf("engine: await decisions: %w", err)
	}

	// 13. onaylananlar → broker.Submit().
	for _, p := range approved {
		e.submitApproved(ctx, snap, p)
	}

	if err := e.syncTrades(ctx, snap.pb); err != nil {
		return fmt.Errorf("engine: %w", err)
	}

	// 14. equity_snapshots'a yaz (İ3: benchmark ile birlikte).
	finalPortfolio, err := snap.pb.Portfolio(ctx)
	if err != nil {
		return fmt.Errorf("engine: final portfolio: %w", err)
	}
	btcCurve, top10Curve := e.benchmarkCurves(snap)
	var benchBTC, benchTop10 *float64
	if len(btcCurve) > 0 {
		v := btcCurve[len(btcCurve)-1].Equity
		benchBTC = &v
	}
	if len(top10Curve) > 0 {
		v := top10Curve[len(top10Curve)-1].Equity
		benchTop10 = &v
	}
	if err := e.cfg.Store.InsertEquitySnapshot(ctx, store.EquitySnapshot{
		Mode: "paper", TS: asOf, Equity: finalPortfolio.Equity, Cash: finalPortfolio.Cash,
		Exposure: finalPortfolio.Exposure(), BenchBTC: benchBTC, BenchTop10: benchTop10,
	}); err != nil {
		return fmt.Errorf("engine: insert equity snapshot: %w", err)
	}

	// 15. günlük özet bildirimi (benchmark ile birlikte — İ3).
	e.notifyDailySummary(ctx, asOf, finalPortfolio, len(finalPortfolio.Positions), benchBTC, benchTop10)

	return nil
}

// alreadyProcessed reports whether RunOnce already completed asOf in a
// previous call (its equity_snapshot row already exists) — see RunOnce's
// doc comment.
func (e *Engine) alreadyProcessed(ctx context.Context, asOf time.Time) (bool, error) {
	snaps, err := e.cfg.Store.ListEquitySnapshots(ctx, "paper")
	if err != nil {
		return false, err
	}
	if len(snaps) == 0 {
		return false, nil
	}
	return !snaps[len(snaps)-1].TS.Before(asOf), nil
}

// persistBreakerState writes brk's current State to system_state under
// breakerStateKey (SPEC.md Bölüm 6.5.3), and — only on the false→true
// transition — sends İ7's mandatory critical notification. It reads the
// previously persisted state first specifically to detect that transition;
// an already-open breaker is re-persisted (idempotent, keeps
// `breaker status` in sync with the freshest reconstruction) without
// spamming a fresh notification every single day it stays open.
func (e *Engine) persistBreakerState(ctx context.Context, brk *risk.Breaker) error {
	wasOpen := false
	if raw, ok, err := e.cfg.Store.GetState(ctx, breakerStateKey); err == nil && ok {
		var prev risk.State
		if jsonErr := json.Unmarshal([]byte(raw), &prev); jsonErr == nil {
			wasOpen = prev.Open
		}
	}

	state := brk.State()
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := e.cfg.Store.SetState(ctx, breakerStateKey, string(raw)); err != nil {
		return err
	}

	if state.Open && !wasOpen {
		e.notify(ctx, notify.LevelCritical, "Devre kesici açıldı", notify.FormatBreakerTripped(state.Reason, state.Detail, state.At))
	}
	return nil
}

// notifyStopTriggers sends SPEC.md Bölüm 6.8's mandatory "stop tetiklenmesi"
// notification for every position closed on today's reconstruction pass.
// It never fires for a trade replayed from history (see daySnapshot's doc
// comment: todaysClosedTrades only ever holds the FINAL calendar day's
// trades), so a restart never re-announces old stop-outs.
func (e *Engine) notifyStopTriggers(ctx context.Context, trades []broker.ClosedTrade) {
	for _, tr := range trades {
		switch tr.ExitReason {
		case "stop":
			e.notify(ctx, notify.LevelWarning, fmt.Sprintf("Stop tetiklendi · %s", tr.Symbol),
				notify.FormatStopTriggered(tr.Symbol, tr.ExitPrice, tr.PnLQuote, tr.PnLPct, e.cfg.Quote))
		case "signal":
			e.notify(ctx, notify.LevelInfo, fmt.Sprintf("Pozisyon kapandı · %s", tr.Symbol),
				notify.FormatStopTriggered(tr.Symbol, tr.ExitPrice, tr.PnLQuote, tr.PnLPct, e.cfg.Quote))
		}
	}
}

// processExitOrStop submits the broker order a SignalExit or SignalStop
// signal implies (SPEC.md Bölüm 6.7 adım 8), mirroring
// internal/backtest/engine.go's applyExitOrStop exactly (İ1) — the only
// difference is that a live exit is also persisted as a store.Proposal row
// (status SUBMITTED immediately, no approval wait: SPEC.md Bölüm 6.5.2/
// risk.Gate's doc comment — risk-REDUCING signals never wait on a human)
// so reconstruct can replay it and the panel can show it in "öneri
// geçmişi".
func (e *Engine) processExitOrStop(ctx context.Context, snap *daySnapshot, s domain.Signal, now, asOf time.Time) error {
	id := newProposalID()

	switch s.Kind {
	case domain.SignalStop:
		if s.StopPrice <= 0 {
			return fmt.Errorf("stop_update: geçersiz StopPrice")
		}
		order, err := snap.pb.Submit(ctx, domain.OrderRequest{
			ClientOrderID: clientOrderID(id, "stop"), Symbol: s.Symbol, Side: domain.SideSell, Type: "stop_market",
			Price: decimal.NewFromFloat(s.StopPrice),
		})
		if err != nil {
			return err
		}
		stop := s.StopPrice
		return e.cfg.Store.InsertProposal(ctx, store.Proposal{
			ID: id, CreatedAt: now, AsOf: asOf, Symbol: s.Symbol, Side: "stop_update", Strategy: e.cfg.Strategy.Name(),
			RefPrice: s.RefPrice, StopPrice: &stop, Qty: "0", RiskAmount: 0, Reason: s.Reason,
			MetricsJSON: metricsJSON(s.Metrics), Status: store.ProposalSubmitted, ExpiresAt: asOf, DecidedAt: now, OrderID: order.ID,
		})

	case domain.SignalExit:
		pos, open := snap.portfolio.Positions[s.Symbol]
		if !open {
			return fmt.Errorf("çıkış sinyali: %s için açık pozisyon yok", s.Symbol)
		}
		order, err := snap.pb.Submit(ctx, domain.OrderRequest{
			ClientOrderID: clientOrderID(id, "exit"), Symbol: s.Symbol, Side: domain.SideSell, Type: "market", Qty: pos.Qty,
		})
		if err != nil {
			return err
		}
		return e.cfg.Store.InsertProposal(ctx, store.Proposal{
			ID: id, CreatedAt: now, AsOf: asOf, Symbol: s.Symbol, Side: "exit", Strategy: e.cfg.Strategy.Name(),
			RefPrice: s.RefPrice, Qty: pos.Qty.String(), RiskAmount: 0, Reason: s.Reason,
			MetricsJSON: metricsJSON(s.Metrics), Status: store.ProposalSubmitted, ExpiresAt: asOf, DecidedAt: now, OrderID: order.ID,
		})

	default:
		return fmt.Errorf("processExitOrStop: beklenmeyen sinyal türü %q", s.Kind)
	}
}

// proposeEntry runs risk.Gate+Sizer on an enter signal (SPEC.md Bölüm 6.7
// adım 9). A rejected signal is recorded as an already-decided REJECTED
// proposal (SPEC.md Bölüm 6.5.2: "Reddedilen sinyaller de kaydedilir")
// and never reaches Telegram. An accepted one is persisted PENDING and
// handed to notify.ProposeTrade; row is nil only when the signal was
// rejected (nothing for the caller to wait on).
func (e *Engine) proposeEntry(ctx context.Context, snap *daySnapshot, s domain.Signal, now, asOf time.Time) (row *store.Proposal, notifyErr error) {
	market := snap.markets[s.Symbol]
	decision := e.cfg.RiskGate.Evaluate(s, risk.GateInput{
		Portfolio: snap.portfolio, Market: market, Now: now,
		LastExitAt: snap.lastExitAt[s.Symbol], BreakerOpen: snap.breaker.Open(),
	})

	id := newProposalID()
	if !decision.Approved {
		_ = e.cfg.Store.InsertProposal(ctx, store.Proposal{
			ID: id, CreatedAt: now, AsOf: asOf, Symbol: s.Symbol, Side: "long", Strategy: e.cfg.Strategy.Name(),
			Score: scorePtr(s.Score), RefPrice: s.RefPrice, StopPrice: stopPtr(s.StopPrice), Qty: "0", RiskAmount: 0,
			Reason: fmt.Sprintf("%s (%s)", decision.Reason, s.Reason), MetricsJSON: metricsJSON(s.Metrics),
			Status: store.ProposalRejected, ExpiresAt: now, DecidedAt: now,
		})
		return nil, nil
	}

	ttl := e.cfg.ApprovalTTL
	expiresAt := now.Add(ttl)
	stop := s.StopPrice
	newRow := store.Proposal{
		ID: id, CreatedAt: now, AsOf: asOf, Symbol: s.Symbol, Side: "long", Strategy: e.cfg.Strategy.Name(),
		Score: scorePtr(s.Score), RefPrice: s.RefPrice, StopPrice: &stop, Qty: decision.Size.Qty.String(),
		RiskAmount: decision.Size.RiskAmount, Reason: s.Reason, MetricsJSON: metricsJSON(s.Metrics),
		Status: store.ProposalPending, ExpiresAt: expiresAt,
	}
	if err := e.cfg.Store.InsertProposal(ctx, newRow); err != nil {
		return nil, err
	}

	notifyErr = e.cfg.Notifier.ProposeTrade(ctx, e.toNotifyProposal(newRow, s, snap, decision))
	return &newRow, notifyErr
}

// toNotifyProposal renders the notify.Proposal SPEC.md Bölüm 6.8's message
// template needs from a just-decided store row plus the signal/portfolio
// context it was sized against.
func (e *Engine) toNotifyProposal(row store.Proposal, s domain.Signal, snap *daySnapshot, decision risk.Decision) notify.Proposal {
	maxPositions := e.cfg.RiskGate.Cfg.MaxPositions
	if maxPositions <= 0 {
		maxPositions = 5 // risk.Gate's own default (SPEC.md Bölüm 8)
	}
	riskPct := 0.0
	if snap.portfolio.Equity > 0 {
		riskPct = decision.Size.RiskAmount / snap.portfolio.Equity
	}
	exposureAfter := 0.0
	if snap.portfolio.Equity > 0 {
		exposureAfter = (snap.portfolio.Equity - snap.portfolio.Cash + decision.Size.Notional) / snap.portfolio.Equity
	}

	base := s.Symbol
	if i := strings.IndexByte(s.Symbol, '/'); i > 0 {
		base = s.Symbol[:i]
	}

	return notify.Proposal{
		ID: row.ID, AsOf: row.AsOf, Symbol: row.Symbol, Strategy: row.Strategy, Side: row.Side,
		RefPrice: row.RefPrice, RefPriceAsOf: row.AsOf, StopPrice: s.StopPrice,
		QtyDisplay: fmt.Sprintf("%s %s", decision.Size.Qty.String(), base), NotionalQuote: decision.Size.Notional,
		QuoteAsset: e.cfg.Quote, RiskAmount: decision.Size.RiskAmount, RiskPct: riskPct, Reason: s.Reason,
		PortfolioAfter: notify.PortfolioAfter{
			OpenPositions: len(snap.portfolio.Positions) + 1, MaxPositions: maxPositions,
			ExposurePct: exposureAfter, CashAfter: snap.portfolio.Cash - decision.Size.Notional,
		},
		ExpiresAt: row.ExpiresAt,
	}
}

// awaitDecisions blocks (bounded by each proposal's own ExpiresAt) until
// every proposal in pending has a decision, persisting APPROVED/REJECTED
// as they arrive and EXPIRED once a deadline passes (SPEC.md Bölüm 6.7
// adım 12). It returns the subset that ended APPROVED, for the caller to
// submit. A decision for a proposal ID not in pending (e.g. a very late
// tap on an already-expired proposal from days ago) is a no-op here — it
// is simply not this batch's concern.
//
// This is also what a restarted process calls (ResumePending) for
// proposals left PENDING by a previous run: each store.Proposal already
// carries its original ExpiresAt, so "resume with whatever time is left"
// falls out for free.
func (e *Engine) awaitDecisions(ctx context.Context, pending []store.Proposal) ([]store.Proposal, error) {
	if len(pending) == 0 {
		return nil, nil
	}
	remaining := make(map[string]store.Proposal, len(pending))
	for _, p := range pending {
		remaining[p.ID] = p
	}
	var approved []store.Proposal

	for len(remaining) > 0 {
		select {
		case <-ctx.Done():
			return approved, ctx.Err()
		case d := <-e.cfg.Notifier.Approvals():
			p, ok := remaining[d.ProposalID]
			if !ok {
				continue
			}
			status := store.ProposalRejected
			if d.Approved {
				status = store.ProposalApproved
			}
			if err := e.cfg.Store.UpdateProposalStatus(ctx, p.ID, status, d.At, ""); err != nil {
				return approved, err
			}
			delete(remaining, d.ProposalID)
			if d.Approved {
				p.Status = status
				approved = append(approved, p)
			}
		default:
			now := e.cfg.Clock.Now()
			for id, p := range remaining {
				if now.Before(p.ExpiresAt) {
					continue
				}
				if err := e.cfg.Store.UpdateProposalStatus(ctx, id, store.ProposalExpired, now, ""); err != nil {
					return approved, err
				}
				delete(remaining, id)
			}
			if len(remaining) == 0 {
				break
			}
			e.cfg.Clock.Sleep(e.cfg.PollInterval)
		}
	}
	return approved, nil
}

// submitApproved implements SPEC.md Bölüm 6.7 adım 13 for a proposal
// approved during THIS RunOnce call, submitting it into the snap already
// in memory (no second reconstruct needed — see ResumePending for the
// cold-start case, which does not have a snap yet and relies on
// reconstruct's own replay to do this instead).
func (e *Engine) submitApproved(ctx context.Context, snap *daySnapshot, p store.Proposal) {
	now := e.cfg.Clock.Now()
	orderID, err := resubmitDecidedProposal(ctx, snap.pb, p)
	if err != nil {
		_ = e.cfg.Store.UpdateProposalStatus(ctx, p.ID, store.ProposalFailed, now, "")
		e.notify(ctx, notify.LevelWarning, fmt.Sprintf("Emir hatası · %s", p.Symbol), err.Error())
		e.recordOrderError(ctx, snap, now)
		return
	}
	_ = e.cfg.Store.UpdateProposalStatus(ctx, p.ID, store.ProposalSubmitted, now, orderID)
}

// ResumePending is SPEC.md Bölüm 6.7's restart-resilience requirement: a
// process that died between writing a PENDING proposal (adım 10) and
// submitting an approved one (adım 13) must, on restart, pick up exactly
// where it left off — expiring anything past its deadline and otherwise
// resuming the same bounded wait it would have been in. Call this once,
// before entering Run's scheduling loop.
func (e *Engine) ResumePending(ctx context.Context) error {
	pending, err := e.cfg.Store.ListProposalsByStatus(ctx, store.ProposalPending)
	if err != nil {
		return fmt.Errorf("engine: list pending proposals: %w", err)
	}

	var approved []store.Proposal
	if len(pending) > 0 {
		approved, err = e.awaitDecisions(ctx, pending)
		if err != nil {
			return fmt.Errorf("engine: resume await decisions: %w", err)
		}
	}

	// Always reconstruct (and sync the trades table to it), even if there
	// was nothing PENDING: a prior run may have exited between a proposal
	// being decided and the next scheduled RunOnce syncing that decision's
	// effect, and a cold start otherwise has no other opportunity to bring
	// /api/positions and /api/trades (SPEC.md Bölüm 7.1) up to date before
	// tomorrow's run — see the package doc comment on reconstruct being the
	// single source of truth for paper-trading state, every call.
	snap, err := e.reconstruct(ctx)
	if err != nil {
		return fmt.Errorf("engine: resume reconstruct: %w", err)
	}
	if err := e.syncTrades(ctx, snap.pb); err != nil {
		return fmt.Errorf("engine: resume %w", err)
	}

	if len(approved) == 0 {
		return nil
	}
	// approved proposals are already persisted as APPROVED (awaitDecisions
	// did that) — reconstruct's replay above already picked them straight up
	// and submitted them into snap.pb, so there is nothing left to submit
	// here; just settle the bookkeeping status.
	now := e.cfg.Clock.Now()
	for _, p := range approved {
		if err := e.cfg.Store.UpdateProposalStatus(ctx, p.ID, store.ProposalSubmitted, now, ""); err != nil {
			return fmt.Errorf("engine: resume mark submitted %s: %w", p.ID, err)
		}
	}
	return nil
}

// benchmarkCurves computes SPEC.md Bölüm 6.7 adım 14/15's İ3 benchmarks
// (BTC buy-and-hold, equal-weight top-10) over the exact calendar/candles
// reconstruct just replayed, reusing internal/backtest's own
// (already-tested) implementations rather than a second, divergent one.
func (e *Engine) benchmarkCurves(snap *daySnapshot) (btc, top10 []backtest.EquityPoint) {
	btcSymbol := "BTC/" + e.cfg.Quote
	btc = backtest.BuyAndHoldCurve(snap.candles[btcSymbol], snap.calendar, e.cfg.InitialCash, e.cfg.Costs)
	top10 = backtest.Top10EqualWeightCurve(snap.candles, snap.calendar, e.cfg.InitialCash, e.cfg.Costs)
	return btc, top10
}

// notifyDailySummary implements SPEC.md Bölüm 6.7 adım 15.
func (e *Engine) notifyDailySummary(ctx context.Context, asOf time.Time, portfolio domain.Portfolio, openPositions int, benchBTC, benchTop10 *float64) {
	stratPct := portfolio.Equity/e.cfg.InitialCash - 1
	btcPct, top10Pct := 0.0, 0.0
	if benchBTC != nil && e.cfg.InitialCash > 0 {
		btcPct = *benchBTC/e.cfg.InitialCash - 1
	}
	if benchTop10 != nil && e.cfg.InitialCash > 0 {
		top10Pct = *benchTop10/e.cfg.InitialCash - 1
	}
	body := notify.FormatSummary(asOf, portfolio.Equity, portfolio.Cash, portfolio.Exposure(), stratPct, btcPct, top10Pct, openPositions)
	e.notify(ctx, notify.LevelInfo, "Günlük özet", body)
}

// notify is a best-effort wrapper: a failed Telegram send must never abort
// the daily cycle that is trying to report something (often an error
// itself) — it is only ever a courtesy on top of what is already durably
// recorded in the store.
func (e *Engine) notify(ctx context.Context, level notify.Level, title, body string) {
	_ = e.cfg.Notifier.Notify(ctx, level, title, body)
}

// --- small pure helpers ----------------------------------------------------

func newProposalID() string {
	return uuid.NewString()
}

func scorePtr(v float64) *float64 { return &v }
func stopPtr(v float64) *float64  { return &v }

// candleDay returns the OpenTime of the most recently CLOSED daily candle
// as of now (SPEC.md Bölüm 4.2: a candle closes at open_time+24h; Bölüm
// 6.7 adım 1 runs 5 minutes after that, so "now" is always safely past
// yesterday's close by the time this is evaluated).
func candleDay(now time.Time) time.Time {
	now = now.UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return midnight.AddDate(0, 0, -1)
}

// marketsBySymbol converts store rows into domain.Market, restricted to
// quote, mirroring cmd/swingbot/main.go's marketsBySymbol (duplicated
// rather than imported: that function is private to package main).
func marketsBySymbol(rows []store.Market, quote string) (map[string]domain.Market, []string) {
	out := make(map[string]domain.Market, len(rows))
	symbols := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.Quote != quote {
			continue
		}
		tick, err1 := decimal.NewFromString(r.TickSize)
		step, err2 := decimal.NewFromString(r.StepSize)
		minNotional, err3 := decimal.NewFromString(r.MinNotional)
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		out[r.Symbol] = domain.Market{
			Symbol: r.Symbol, Base: r.Base, Quote: r.Quote, Active: r.Active,
			TickSize: tick, StepSize: step, MinNotional: minNotional,
			ListedAt: r.ListedAt, DelistedAt: r.DelistedAt,
		}
		symbols = append(symbols, r.Symbol)
	}
	sort.Strings(symbols)
	return out, symbols
}

// buildCalendar returns the sorted, deduplicated union of every OpenTime
// across every symbol's candles — mirrors internal/backtest/engine.go's
// helper of the same name (İ1: both packages replay the identical notion
// of "calendar day").
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
// that symbol's own chronological slice.
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

// seriesAndUniverse returns, for calendar day t, every candidate symbol's
// history truncated through t plus the subset that actually has a bar
// exactly on t (a data gap for one symbol excludes only that symbol, per
// SPEC.md Bölüm 14 — not the whole day).
func seriesAndUniverse(candles map[string][]domain.Candle, dateIndex map[string]map[int64]int, candidates []string, t time.Time) (map[string][]domain.Candle, []string) {
	series := make(map[string][]domain.Candle, len(candidates))
	universe := make([]string, 0, len(candidates))
	key := t.UnixMilli()
	for _, sym := range candidates {
		i, ok := dateIndex[sym][key]
		if !ok {
			continue
		}
		series[sym] = truncateSeries(candles[sym], i)
		universe = append(universe, sym)
	}
	return series, universe
}

// truncateSeries returns full[:i+1] with capacity ALSO clamped to i+1 —
// see internal/backtest/engine.go's identical helper for why this is what
// makes İ2 a runtime guarantee.
func truncateSeries(full []domain.Candle, i int) []domain.Candle {
	return full[: i+1 : i+1]
}

// metricsJSON marshals a signal's decision metrics (İ6) for
// proposals.metrics_json. It never fails the caller: an encoding error
// (should not happen for a map[string]float64) degrades to "{}" rather
// than losing an otherwise-valid proposal.
func metricsJSON(m map[string]float64) string {
	raw, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(raw)
}
