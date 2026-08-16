package backtest

import (
	"github.com/shopspring/decimal"

	"swingbot/internal/config"
	"swingbot/internal/domain"
)

// DefaultRiskConfig returns the SPEC.md Bölüm 6.5.1/8 default sizing
// parameters, for callers (tests, `swingbot backtest` without an explicit
// RiskGate) that just want the documented defaults.
func DefaultRiskConfig() config.RiskConfig {
	return config.RiskConfig{
		RiskPerTrade:   0.01,
		MaxPositions:   5,
		MaxExposure:    0.80,
		MaxPositionPct: 0.25,
		CooldownHours:  24,
	}
}

// SimpleRiskGate implements SPEC.md Bölüm 6.5.1's position-sizing formula
// ONLY — no max_positions/max_exposure/cooldown/breaker gating, and no
// "already_open"/"cooldown" bookkeeping across days. It exists purely so
// backtest.Run can prove the engine's fill/cost mechanics correct before
// internal/risk (risk-management-engineer, Ajan 9) exists; swap it for
// risk.Sizer + risk.Gate via Config.RiskGate once that package lands —
// nothing else in this package needs to change, since Run only ever talks
// to the RiskGate interface.
type SimpleRiskGate struct {
	riskPerTrade   float64
	maxPositionPct float64
	markets        map[string]domain.Market // symbol -> filters; nil entries fall back to permissive defaults
}

// NewSimpleRiskGate builds a SimpleRiskGate from cfg. markets may be nil —
// symbols without a known domain.Market use a permissive 1e-8 step size
// and zero min notional, which is enough for a single-symbol backtest
// (e.g. the BTC buy-and-hold golden test) where exchange filters are not
// the point being tested.
func NewSimpleRiskGate(cfg config.RiskConfig, markets map[string]domain.Market) *SimpleRiskGate {
	riskPerTrade := cfg.RiskPerTrade
	if riskPerTrade <= 0 {
		riskPerTrade = 0.01
	}
	maxPositionPct := cfg.MaxPositionPct
	if maxPositionPct <= 0 {
		maxPositionPct = 0.25
	}
	return &SimpleRiskGate{riskPerTrade: riskPerTrade, maxPositionPct: maxPositionPct, markets: markets}
}

var defaultStep = decimal.New(1, -8) // 0.00000001

func (g *SimpleRiskGate) stepAndMinNotional(symbol string) (decimal.Decimal, decimal.Decimal) {
	m, ok := g.markets[symbol]
	if !ok {
		return defaultStep, decimal.Zero
	}
	step := m.StepSize
	if step.IsZero() {
		step = defaultStep
	}
	return step, m.MinNotional
}

// Size implements RiskGate per SPEC.md Bölüm 6.5.1:
//
//	risk_tutari = equity * risk_per_trade
//	stop_mesafesi = entry - stop
//	ham_qty = risk_tutari / stop_mesafesi, StepSize'a AŞAĞI yuvarlanmış
//
// plus the three drop/clip rules: cash-affordability clip, min-notional
// drop, and the max_position_pct cap.
func (g *SimpleRiskGate) Size(s domain.Signal, portfolio domain.Portfolio) (decimal.Decimal, string) {
	if s.Kind != domain.SignalEnter {
		return decimal.Zero, "not_an_entry_signal"
	}
	if s.StopPrice <= 0 || s.StopPrice >= s.RefPrice {
		return decimal.Zero, "invalid_stop"
	}
	if portfolio.Equity <= 0 {
		return decimal.Zero, "insufficient_cash"
	}

	step, minNotional := g.stepAndMinNotional(s.Symbol)
	entry := decimal.NewFromFloat(s.RefPrice)

	riskAmount := decimal.NewFromFloat(portfolio.Equity * g.riskPerTrade)
	stopDistance := decimal.NewFromFloat(s.RefPrice - s.StopPrice)
	qty := floorToStep(riskAmount.Div(stopDistance), step)
	if !qty.IsPositive() {
		return decimal.Zero, "below_min_qty"
	}

	notional := qty.Mul(entry)
	cash := decimal.NewFromFloat(portfolio.Cash)
	if notional.GreaterThan(cash) {
		qty = floorToStep(cash.Div(entry), step)
		notional = qty.Mul(entry)
	}
	if !qty.IsPositive() {
		return decimal.Zero, "insufficient_cash"
	}

	maxNotional := decimal.NewFromFloat(portfolio.Equity * g.maxPositionPct)
	if notional.GreaterThan(maxNotional) {
		qty = floorToStep(maxNotional.Div(entry), step)
		notional = qty.Mul(entry)
	}
	if !qty.IsPositive() {
		return decimal.Zero, "below_min_qty"
	}
	if notional.LessThan(minNotional) {
		return decimal.Zero, "below_min_notional"
	}

	return qty, ""
}

func floorToStep(raw, step decimal.Decimal) decimal.Decimal {
	if step.IsZero() || raw.IsNegative() {
		return decimal.Zero
	}
	return raw.Div(step).Floor().Mul(step)
}
