// Package risk implements SPEC.md Bölüm 6.5: position sizing, the signal
// gate, and the circuit breaker. Together these are what stands between a
// Strategy's raw domain.Signal and an actual order — a Strategy never knows
// how much capital is available (see domain.Signal's doc comment); this
// package is where that decision is made, checked, and — when a signal is
// rejected — explained (İ6).
//
// Binding rule: this package imports only internal/domain, internal/config
// and the standard library (+ decimal). No store, no exchange, no network,
// no clock reads (time.Time values are always passed in by the caller).
// That keeps risk pure and deterministic: the same inputs always produce
// the same Decision/SizeResult/State, in backtest, paper and live alike
// (İ1). Any persistence of a Decision or a breaker State (proposals table,
// system_state) is the caller's (engine's) job, not this package's.
package risk

import (
	"github.com/shopspring/decimal"

	"swingbot/internal/config"
	"swingbot/internal/domain"
)

// Sizer-level reject reason codes (SPEC.md Bölüm 6.5.1). ReasonBelowMinNotional
// is also one of the seven gate rules in Bölüm 6.5.2's table; the other two
// codes here are sizing-specific failure modes not named in that table but
// still required to carry a reason (İ6, and the binding rule in this
// agent's brief: "her ret kararı bir gerekçe koduyla kaydedilmeli").
const (
	// ReasonInvalidStopDistance: entry <= stop (or entry <= 0), so no
	// well-defined stop distance exists to size against. This can only
	// happen from malformed strategy output; it is not one of the normal
	// operating rejections.
	ReasonInvalidStopDistance = "invalid_stop_distance"
	// ReasonZeroQty: after StepSize rounding and the max_position_pct /
	// cash caps, the resulting quantity is <= 0.
	ReasonZeroQty = "zero_qty"
	// ReasonBelowMinNotional: qty * entry < market.MinNotional.
	ReasonBelowMinNotional = "below_min_notional"
)

// defaults mirror SPEC.md Bölüm 8's config.yaml defaults. They apply only
// when the corresponding config field is zero (unset), so an operator who
// deliberately configures e.g. risk_per_trade: 0.02 always gets 0.02 — a
// zero is the one value that cannot be told apart from "not configured",
// and SPEC.md Bölüm 6.5.1/6.5.2/6.5.3 explicitly documents what "varsayılan"
// (default) each parameter falls back to in that case.
const (
	defaultRiskPerTrade   = 0.01
	defaultMaxPositionPct = 0.25
)

// SizeResult is the outcome of sizing a single enter signal against a
// portfolio and market. When Accepted is false, Reason is always non-empty
// (one of the Reason* consts in this file) — SPEC.md Bölüm 6.5.1/6.5.2
// require every rejection to be explainable, not just silently zero.
type SizeResult struct {
	// Qty is the computed order quantity, already rounded down to
	// market.StepSize. Zero when Accepted is false.
	Qty decimal.Decimal
	// RiskAmount is the actual quote-currency amount at risk to the stop
	// (Qty * (entry - stop)) using the FINAL (post-cap, post-rounding)
	// Qty — this can be less than equity*risk_per_trade if the position
	// was trimmed by cash or max_position_pct.
	RiskAmount float64
	// Notional is Qty * entry, in quote currency.
	Notional float64
	// Accepted reports whether Qty is usable to place an order.
	Accepted bool
	// Reason is a stable machine-checkable code explaining rejection; see
	// the Reason* consts. Empty when Accepted is true.
	Reason string
	// CappedByCash reports whether qty was trimmed because
	// qty*entry would otherwise have exceeded available cash.
	CappedByCash bool
	// CappedByMaxPositionPct reports whether qty was trimmed because it
	// would otherwise have exceeded max_position_pct of equity.
	CappedByMaxPositionPct bool
}

// Sizer turns a domain.Signal (Kind == SignalEnter) plus the current
// portfolio and the target market's exchange filters into an order
// quantity, per SPEC.md Bölüm 6.5.1:
//
//	risk_tutari    = equity * risk_per_trade
//	stop_mesafesi  = entry - stop
//	ham_qty        = risk_tutari / stop_mesafesi
//	qty            = ham_qty, StepSize'a AŞAĞI yuvarlanmış
//
// followed by: clip to available cash, reject below MinNotional, reject
// qty<=0, and cap a single position to max_position_pct of equity.
//
// Why size off the stop distance rather than a fixed notional: entering
// with a fixed dollar amount takes a large risk on a volatile coin and a
// small risk on a calm one for the identical position size. Dividing the
// risk budget by the stop distance equalizes the dollar risk taken on
// every trade, which is the only way "risk_per_trade" means the same thing
// across a universe of coins with wildly different volatility.
type Sizer struct {
	Cfg config.RiskConfig
}

// NewSizer constructs a Sizer bound to cfg (internal/risk section of
// config.yaml).
func NewSizer(cfg config.RiskConfig) *Sizer {
	return &Sizer{Cfg: cfg}
}

// Size computes the order quantity for signal against portfolio and
// market. Callers are responsible for only calling Size on SignalEnter
// signals (risk.Gate does this for you) — Size itself does not check
// signal.Kind, it simply treats signal.RefPrice as the entry price and
// signal.StopPrice as the stop, unconditionally.
func (s *Sizer) Size(signal domain.Signal, portfolio domain.Portfolio, market domain.Market) SizeResult {
	riskPerTrade := s.Cfg.RiskPerTrade
	if riskPerTrade <= 0 {
		riskPerTrade = defaultRiskPerTrade
	}
	maxPositionPct := s.Cfg.MaxPositionPct
	if maxPositionPct <= 0 {
		maxPositionPct = defaultMaxPositionPct
	}

	entry := signal.RefPrice
	stop := signal.StopPrice
	stopDistance := entry - stop
	if entry <= 0 || stopDistance <= 0 {
		return SizeResult{Reason: ReasonInvalidStopDistance}
	}

	riskAmount := portfolio.Equity * riskPerTrade
	rawQty := riskAmount / stopDistance

	step := market.StepSize
	qty := floorToStep(decimal.NewFromFloat(rawQty), step)

	var cappedByPositionPct, cappedByCash bool

	// Cap a single position to max_position_pct of equity.
	maxNotionalByPosition := portfolio.Equity * maxPositionPct
	maxQtyByPosition := floorToStep(decimal.NewFromFloat(maxNotionalByPosition/entry), step)
	if qty.GreaterThan(maxQtyByPosition) {
		qty = maxQtyByPosition
		cappedByPositionPct = true
	}

	// Trim to available cash.
	maxQtyByCash := floorToStep(decimal.NewFromFloat(portfolio.Cash/entry), step)
	if qty.GreaterThan(maxQtyByCash) {
		qty = maxQtyByCash
		cappedByCash = true
	}

	if qty.Sign() <= 0 {
		return SizeResult{
			Reason:                 ReasonZeroQty,
			CappedByCash:           cappedByCash,
			CappedByMaxPositionPct: cappedByPositionPct,
		}
	}

	qtyFloat, _ := qty.Float64()
	notional := qtyFloat * entry

	minNotional, _ := market.MinNotional.Float64()
	if minNotional > 0 && notional < minNotional {
		// Qty/Notional are deliberately left zero here, matching this
		// struct's own doc comment ("Zero when Accepted is false") and
		// the other two rejection paths above — a caller must never be
		// able to read a non-zero Qty off a SizeResult without also
		// checking Accepted.
		return SizeResult{
			Reason:                 ReasonBelowMinNotional,
			CappedByCash:           cappedByCash,
			CappedByMaxPositionPct: cappedByPositionPct,
		}
	}

	return SizeResult{
		Qty:                    qty,
		RiskAmount:             qtyFloat * stopDistance,
		Notional:               notional,
		Accepted:               true,
		CappedByCash:           cappedByCash,
		CappedByMaxPositionPct: cappedByPositionPct,
	}
}

// floorToStep rounds qty DOWN to the nearest multiple of step. A
// non-positive step (unset StepSize) is treated as "no rounding" rather
// than a division by zero.
func floorToStep(qty, step decimal.Decimal) decimal.Decimal {
	if step.Sign() <= 0 {
		return qty
	}
	return qty.Div(step).Floor().Mul(step)
}
