package broker

import (
	"context"
	"fmt"
	"sort"

	"time"

	"github.com/shopspring/decimal"

	"swingbot/internal/domain"
)

// ClosedTrade is a completed round-trip (entry + exit) recorded by
// PaperBroker. The caller (internal/backtest, internal/engine) converts
// these into store.Trade rows and/or feeds them to backtest/metrics.go.
type ClosedTrade struct {
	Symbol     string
	Strategy   string
	EntryTime  time.Time
	EntryPrice float64
	ExitTime   time.Time
	ExitPrice  float64
	Qty        decimal.Decimal
	Fees       float64 // entry fee + exit fee, combined
	PnLQuote   float64
	PnLPct     float64
	ExitReason string // "signal" | "stop"
}

// openPosition is PaperBroker's internal book for a live position. It
// embeds domain.Position and adds the bookkeeping (entry fee, order type
// that opened it) PaperBroker needs to compute PnL on exit — none of which
// belongs on the shared domain.Position type.
type openPosition struct {
	domain.Position
	EntryFee float64
}

// pendingOrder is a market order accepted by Submit but not yet filled.
// PaperBroker fills it on the NEXT call to Advance, using that day's open
// (SPEC.md Bölüm 6.6.1) — this is the mechanism that realizes İ2 (signal
// at t, trade at t+1) inside the broker rather than trusting callers to
// respect the delay themselves.
type pendingOrder struct {
	req domain.OrderRequest
}

// PaperBroker simulates order execution against historical or live candle
// data (SPEC.md Bölüm 5.5, 6.6.1). It backs both backtest ("backtest"
// mode, fed the full historical series up front) and paper trading
// ("paper" mode, fed candles as they close) — the same fill model runs in
// both, which is what makes a paper-trading track record predictive of
// how the strategy would have backtested over the same days.
//
// PaperBroker is long-only: every strategy in SPEC.md Bölüm 6.4 only ever
// opens long positions, so Submit rejects a "stop_market" order on the buy
// side and rejects a second "market" buy while a position (or a pending
// entry) is already open for that symbol. That is a deliberate simplifying
// invariant for Faz 1, not an accidental limitation — a violation of it
// signals a bug in the caller (strategy or, later, risk.Gate), not a
// legitimate use case to silently support.
type PaperBroker struct {
	mode         string
	clock        *BacktestClock
	costs        Costs
	strategyName string

	cash float64

	candles map[string][]domain.Candle // symbol -> kronolojik mumlar
	cursor  map[string]int             // symbol -> index of latest known bar at/ before clock.Now()
	started bool

	positions    map[string]openPosition
	pendingStops map[string]float64 // symbol -> stop level reserved for the next entry fill
	pending      []pendingOrder

	seen        map[string]domain.Order // ClientOrderID -> order (İ5 idempotency)
	trades      []ClosedTrade
	seq         int
	lastExitFee float64 // fee of the most recent settleExit call; see fillPending
}

// NewPaperBroker constructs a PaperBroker. candles must contain full
// chronological OHLCV history (warmup bars included) for every symbol the
// caller intends to trade; PaperBroker never fetches data itself. mode
// must be "backtest" or "paper" (SPEC.md Bölüm 5.5). strategyName is
// stamped onto every domain.Position/ClosedTrade this broker produces —
// a single PaperBroker instance runs exactly one strategy at a time,
// matching how internal/backtest.Run and the daily paper engine both use
// it (SPEC.md İ1: one Strategy per run).
//
// Returns ErrCostsNotConfigured if costs is not both fee_rate>0 and
// slippage_bps>0 (İ4) — this holds even if the caller's config.Config was
// already validated, so a PaperBroker built directly (e.g. in a test)
// cannot accidentally simulate cost-free.
func NewPaperBroker(mode string, candles map[string][]domain.Candle, initialCash float64, costs Costs, clock *BacktestClock, strategyName string) (*PaperBroker, error) {
	if mode != "backtest" && mode != "paper" {
		return nil, fmt.Errorf("broker: paper broker mode must be \"backtest\" or \"paper\", got %q", mode)
	}
	if err := costs.Validate(); err != nil {
		return nil, err
	}
	if initialCash <= 0 {
		return nil, fmt.Errorf("broker: initial cash must be positive, got %v", initialCash)
	}

	cursor := make(map[string]int, len(candles))
	for sym := range candles {
		cursor[sym] = -1
	}

	return &PaperBroker{
		mode:         mode,
		clock:        clock,
		costs:        costs,
		strategyName: strategyName,
		cash:         initialCash,
		candles:      candles,
		cursor:       cursor,
		positions:    make(map[string]openPosition),
		pendingStops: make(map[string]float64),
		seen:         make(map[string]domain.Order),
	}, nil
}

func (b *PaperBroker) Mode() string { return b.mode }

// Advance settles day t: it fills pending market orders (submitted in
// response to the PREVIOUS day's close, per İ2) at t's open, then checks
// every still-open position's resting stop against t's low (the gap
// rule), then advances the broker's notion of "now" to t. The backtest
// engine calls this once per simulated day, before evaluating that day's
// strategy signals — see internal/backtest/engine.go.
func (b *PaperBroker) Advance(ctx context.Context, t time.Time) error {
	if b.started && !t.After(b.clock.Now()) {
		return fmt.Errorf("broker: Advance called with non-increasing time (now=%s, t=%s)", b.clock.Now(), t)
	}

	for sym := range b.candles {
		b.advanceCursor(sym, t)
	}

	filledEntries := b.fillPending(t)
	b.updateHighWater(t)
	b.checkStops(t, filledEntries)

	b.clock.Advance(t)
	b.started = true
	return nil
}

// advanceCursor moves symbol's cursor forward while the NEXT bar's
// OpenTime is still <= t. Candle histories are chronological, so this is
// amortized O(1) per Advance call in the common case of one bar/day.
func (b *PaperBroker) advanceCursor(symbol string, t time.Time) {
	series := b.candles[symbol]
	i := b.cursor[symbol]
	for i+1 < len(series) && !series[i+1].OpenTime.After(t) {
		i++
	}
	b.cursor[symbol] = i
}

// candleAt returns symbol's candle whose OpenTime == t exactly, if the
// cursor (already advanced for t by advanceCursor) currently sits on it.
// ok is false on a data gap (no bar for this symbol today) — pending
// orders and stop checks simply retry on the next Advance call rather
// than erroring, since a single missing bar for one symbol must not abort
// an entire multi-symbol backtest.
func (b *PaperBroker) candleAt(symbol string, t time.Time) (domain.Candle, bool) {
	series := b.candles[symbol]
	i, ok := b.cursor[symbol]
	if !ok || i < 0 || i >= len(series) {
		return domain.Candle{}, false
	}
	c := series[i]
	if !c.OpenTime.Equal(t) {
		return domain.Candle{}, false
	}
	return c, true
}

// fillPending settles every still-open pending market order that has a
// bar available today, applying slippage away from the trader (SPEC.md
// Bölüm 6.6.1: buy pays more, sell receives less) plus fee_rate. It
// returns the set of symbols that had an ENTRY fill today so checkStops
// can skip them (a position must not be stopped out on the same bar it
// was opened).
func (b *PaperBroker) fillPending(t time.Time) map[string]bool {
	justOpened := make(map[string]bool)

	remaining := b.pending[:0]
	for _, po := range b.pending {
		c, ok := b.candleAt(po.req.Symbol, t)
		if !ok {
			remaining = append(remaining, po) // no bar today; retry next Advance
			continue
		}

		qty, _ := po.req.Qty.Float64()
		switch po.req.Side {
		case domain.SideBuy:
			fillPrice := c.Open * (1 + b.costs.SlippageBps/10000)
			cost := fillPrice * qty
			fee := cost * b.costs.FeeRate
			b.cash -= cost + fee

			stop := b.pendingStops[po.req.Symbol]
			delete(b.pendingStops, po.req.Symbol)

			b.positions[po.req.Symbol] = openPosition{
				Position: domain.Position{
					Symbol:     po.req.Symbol,
					Qty:        po.req.Qty,
					EntryPrice: fillPrice,
					EntryTime:  t,
					StopPrice:  stop,
					HighWater:  c.High,
					Strategy:   b.strategyName,
				},
				EntryFee: fee,
			}
			justOpened[po.req.Symbol] = true
			b.markFilled(po.req.ClientOrderID, po.req.Qty, fillPrice, fee)

		case domain.SideSell:
			fillPrice := c.Open * (1 - b.costs.SlippageBps/10000)
			if b.settleExit(po.req.Symbol, fillPrice, t, "signal") {
				b.markFilled(po.req.ClientOrderID, po.req.Qty, fillPrice, b.lastExitFee)
			} else {
				// The position this exit targeted is already gone — e.g. a
				// gap-rule stop closed it on an earlier day while this
				// order sat pending through a data gap. Recording it
				// FILLED would fabricate a trade that moved no cash;
				// CANCELED is honest about what actually happened.
				b.markCanceled(po.req.ClientOrderID)
			}
		}
	}
	b.pending = remaining
	return justOpened
}

// updateHighWater marks every open position's HighWater to the running
// max of today's high (SPEC.md domain.Position: "trailing stop için
// görülen en yüksek fiyat"). PaperBroker is the only owner of Position
// state, so it is the one that has to keep HighWater current — a Strategy
// only ever reads it (via Input.Portfolio) to compute a new StopPrice and
// emit SignalStop; it has no way to write it back itself. A position
// opened today already has HighWater seeded to today's high in
// fillPending, so this is a no-op for it.
func (b *PaperBroker) updateHighWater(t time.Time) {
	for sym, pos := range b.positions {
		c, ok := b.candleAt(sym, t)
		if !ok {
			continue
		}
		if c.High > pos.HighWater {
			pos.HighWater = c.High
			b.positions[sym] = pos
		}
	}
}

// checkStops closes any open position (other than one opened today, see
// justOpenedToday) whose resting stop was breached by today's low,
// applying the gap rule: if the open itself is already through the stop,
// the fill is the open, not the stop level (SPEC.md Bölüm 6.6.1).
func (b *PaperBroker) checkStops(t time.Time, justOpenedToday map[string]bool) {
	// Sorted iteration keeps fills deterministic regardless of Go's
	// randomized map iteration order (SPEC.md Bölüm 10 determinizm testi).
	symbols := make([]string, 0, len(b.positions))
	for sym := range b.positions {
		symbols = append(symbols, sym)
	}
	sort.Strings(symbols)

	for _, sym := range symbols {
		if justOpenedToday[sym] {
			continue
		}
		pos, ok := b.positions[sym]
		if !ok || pos.StopPrice <= 0 {
			continue
		}
		c, ok := b.candleAt(sym, t)
		if !ok {
			continue
		}
		if c.Low > pos.StopPrice {
			continue
		}

		gapFill := c.Open
		if pos.StopPrice < gapFill {
			gapFill = pos.StopPrice
		}
		fillPrice := gapFill * (1 - b.costs.SlippageBps/10000)
		b.settleExit(sym, fillPrice, t, "stop") // checkStops just confirmed sym is open, above
	}
}

// settleExit closes qty of an open position at fillPrice, updates cash,
// and records a ClosedTrade, leaving the fee it charged in b.lastExitFee
// so fillPending's "signal" exit branch can record it on the matching
// Order without settleExit needing to also thread an Order through. reason
// is "signal" (explicit exit order) or "stop" (gap-rule trigger detected
// by checkStops).
//
// settleExit always closes the FULL position — it takes no qty parameter
// on purpose. PaperBroker (and everything that submits to it) never does
// partial exits (SPEC.md Bölüm 6.6.2's "kısmi dolumları destekle" is a
// LiveBroker-only requirement), so trusting a caller-supplied qty here
// would only ever be a way for a caller bug to silently under/over-close
// a position; using pos.Qty directly makes that class of bug impossible.
// ok is false (a no-op, no cash/trade side effects) if symbol has no open
// position — callers must check ok before treating the exit as filled.
func (b *PaperBroker) settleExit(symbol string, fillPrice float64, t time.Time, reason string) bool {
	pos, ok := b.positions[symbol]
	if !ok {
		return false
	}
	qtyF, _ := pos.Qty.Float64()

	proceeds := fillPrice * qtyF
	fee := proceeds * b.costs.FeeRate
	b.cash += proceeds - fee
	b.lastExitFee = fee

	entryCost := pos.EntryPrice*qtyF + pos.EntryFee
	pnl := (proceeds - fee) - entryCost
	pnlPct := 0.0
	if entryCost != 0 {
		pnlPct = pnl / entryCost
	}

	b.trades = append(b.trades, ClosedTrade{
		Symbol:     symbol,
		Strategy:   b.strategyName,
		EntryTime:  pos.EntryTime,
		EntryPrice: pos.EntryPrice,
		ExitTime:   t,
		ExitPrice:  fillPrice,
		Qty:        pos.Qty,
		Fees:       pos.EntryFee + fee,
		PnLQuote:   pnl,
		PnLPct:     pnlPct,
		ExitReason: reason,
	})
	delete(b.positions, symbol)
	return true
}

// markFilled updates the recorded Order for clientOrderID to FILLED. It is
// a no-op if the ID is unknown (should not happen: every pending order
// originated from a Submit call that recorded one).
func (b *PaperBroker) markFilled(clientOrderID string, qty decimal.Decimal, avgPrice, fee float64) {
	o, ok := b.seen[clientOrderID]
	if !ok {
		return
	}
	o.FilledQty = qty
	o.AvgPrice = decimal.NewFromFloat(avgPrice)
	o.Fee = decimal.NewFromFloat(fee)
	o.Status = "FILLED"
	b.seen[clientOrderID] = o
}

// markCanceled marks a pending order as CANCELED without touching cash,
// fees or the trade log — used when an exit order's target position is
// already gone by the time its bar arrives (see fillPending's sell case).
func (b *PaperBroker) markCanceled(clientOrderID string) {
	o, ok := b.seen[clientOrderID]
	if !ok {
		return
	}
	o.Status = "CANCELED"
	b.seen[clientOrderID] = o
}

// Submit accepts req (SPEC.md Bölüm 5.5). "market" orders are queued and
// filled on the next Advance call (İ2); "stop_market" orders set or
// replace the resting stop on an existing position, or reserve the stop
// level for a same-symbol entry that is still pending (so the level is
// attached to the position the instant it opens).
//
// Resubmitting an already-seen ClientOrderID returns the previously
// recorded order unchanged instead of creating a second one (İ5).
func (b *PaperBroker) Submit(ctx context.Context, req domain.OrderRequest) (domain.Order, error) {
	if existing, ok := b.seen[req.ClientOrderID]; ok {
		return existing, nil
	}
	if _, ok := b.candles[req.Symbol]; !ok {
		return domain.Order{}, fmt.Errorf("broker: submit %s: unknown symbol (no candle data loaded)", req.Symbol)
	}

	order := domain.Order{
		ID:            fmt.Sprintf("paper-%06d", b.nextSeq()),
		ClientOrderID: req.ClientOrderID,
		Symbol:        req.Symbol,
		Side:          req.Side,
		Type:          req.Type,
		Qty:           req.Qty,
		Price:         req.Price,
		Status:        "PENDING",
		CreatedAt:     b.clock.Now(),
	}

	switch req.Type {
	case "market":
		if !req.Qty.IsPositive() {
			return domain.Order{}, fmt.Errorf("broker: submit %s: market order qty must be positive", req.Symbol)
		}
		if req.Side == domain.SideBuy {
			if _, open := b.positions[req.Symbol]; open {
				return domain.Order{}, fmt.Errorf("broker: submit %s: already has an open position (long-only, one position per symbol)", req.Symbol)
			}
			for _, po := range b.pending {
				if po.req.Symbol == req.Symbol && po.req.Side == domain.SideBuy {
					return domain.Order{}, fmt.Errorf("broker: submit %s: an entry order is already pending for this symbol", req.Symbol)
				}
			}
		} else {
			if _, open := b.positions[req.Symbol]; !open {
				return domain.Order{}, fmt.Errorf("broker: submit %s: no open position to exit", req.Symbol)
			}
			for _, po := range b.pending {
				if po.req.Symbol == req.Symbol && po.req.Side == domain.SideSell {
					return domain.Order{}, fmt.Errorf("broker: submit %s: an exit order is already pending for this symbol", req.Symbol)
				}
			}
		}
		b.pending = append(b.pending, pendingOrder{req: req})

	case "stop_market":
		if req.Side != domain.SideSell {
			return domain.Order{}, fmt.Errorf("broker: submit %s: stop_market must be a sell (long-only)", req.Symbol)
		}
		stopPrice, _ := req.Price.Float64()
		if stopPrice <= 0 {
			return domain.Order{}, fmt.Errorf("broker: submit %s: stop_market requires a positive Price", req.Symbol)
		}
		if pos, open := b.positions[req.Symbol]; open {
			pos.StopPrice = stopPrice
			b.positions[req.Symbol] = pos
		} else {
			b.pendingStops[req.Symbol] = stopPrice
		}
		order.Status = "ACCEPTED"

	default:
		return domain.Order{}, fmt.Errorf("broker: submit %s: unsupported order type %q (want \"market\" or \"stop_market\")", req.Symbol, req.Type)
	}

	b.seen[req.ClientOrderID] = order
	return order, nil
}

// Cancel removes a still-PENDING market order. Resting stop_market
// "orders" are amendments, not queued fills, so there is nothing to
// cancel for those beyond submitting a new stop_market to replace them.
func (b *PaperBroker) Cancel(ctx context.Context, symbol, orderID string) error {
	var clientOrderID string
	for cid, o := range b.seen {
		if o.ID == orderID && o.Symbol == symbol {
			clientOrderID = cid
			break
		}
	}
	if clientOrderID == "" {
		return fmt.Errorf("broker: cancel %s/%s: unknown order", symbol, orderID)
	}

	for i, po := range b.pending {
		if po.req.ClientOrderID == clientOrderID {
			b.pending = append(b.pending[:i], b.pending[i+1:]...)
			o := b.seen[clientOrderID]
			o.Status = "CANCELED"
			b.seen[clientOrderID] = o
			return nil
		}
	}
	return fmt.Errorf("broker: cancel %s/%s: not found among pending orders (already filled or not a queued market order)", symbol, orderID)
}

// Portfolio marks every open position to its latest known close (per
// PaperBroker's own cursor — never a future bar) and returns the current
// cash/equity snapshot (SPEC.md Bölüm 5.5).
func (b *PaperBroker) Portfolio(ctx context.Context) (domain.Portfolio, error) {
	positions := make(map[string]domain.Position, len(b.positions))
	equity := b.cash
	for sym, pos := range b.positions {
		positions[sym] = pos.Position
		last := lastClose(b.candles[sym], b.cursor[sym])
		qtyF, _ := pos.Qty.Float64()
		equity += qtyF * last
	}
	return domain.Portfolio{Cash: b.cash, Positions: positions, Equity: equity}, nil
}

func lastClose(series []domain.Candle, cursor int) float64 {
	if cursor < 0 || cursor >= len(series) {
		return 0
	}
	return series[cursor].Close
}

// ClosedTrades returns every round-trip trade settled so far, in the
// order they closed.
func (b *PaperBroker) ClosedTrades() []ClosedTrade {
	out := make([]ClosedTrade, len(b.trades))
	copy(out, b.trades)
	return out
}

// ClosedTradesSince returns trades closed at index n or later (n is a
// previously observed len(b.trades)/count), copying only that tail
// instead of the full history. For a caller that polls once per
// simulated day for "what closed today" (internal/backtest.Run, for the
// cooldown clock and the circuit breaker), calling plain ClosedTrades()
// every day would copy the whole, ever-growing trade list on every poll —
// O(days*trades) over a full backtest instead of the O(trades) this gives.
func (b *PaperBroker) ClosedTradesSince(n int) []ClosedTrade {
	if n >= len(b.trades) {
		return nil
	}
	out := make([]ClosedTrade, len(b.trades)-n)
	copy(out, b.trades[n:])
	return out
}

func (b *PaperBroker) nextSeq() int {
	b.seq++
	return b.seq
}
