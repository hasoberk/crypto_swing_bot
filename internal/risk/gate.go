package risk

import (
	"time"

	"swingbot/internal/config"
	"swingbot/internal/domain"
)

// Gate-level reject reason codes, matching every row of SPEC.md Bölüm
// 6.5.2's table except below_min_notional, which is defined in sizer.go
// (it is produced by the Sizer and simply forwarded here) since it is a
// sizing outcome, not a gate precondition.
const (
	ReasonMaxPositions     = "max_positions"
	ReasonMaxExposure      = "max_exposure"
	ReasonAlreadyOpen      = "already_open"
	ReasonCooldown         = "cooldown"
	ReasonBreakerOpen      = "breaker_open"
	ReasonInsufficientCash = "insufficient_cash"
	// ReasonMarketMismatch is not in SPEC.md's table: it is a defensive
	// check for a caller bug (GateInput.Market belongs to a different
	// symbol than the signal being evaluated). Sizing against the wrong
	// market's StepSize/MinNotional would silently produce a wrong
	// quantity, so Gate refuses instead of guessing.
	ReasonMarketMismatch = "market_mismatch"
)

// defaults mirror SPEC.md Bölüm 8's config.yaml defaults for the risk
// section's gate parameters (see sizer.go's defaults block for the
// sizing-only ones).
const (
	defaultMaxPositions  = 5
	defaultMaxExposure   = 0.80
	defaultCooldownHours = 24
)

// Decision is the gate's verdict for a single domain.Signal. It is
// recorded regardless of outcome — SPEC.md Bölüm 6.5.2: "Reddedilen
// sinyaller de kaydedilir. Panelde 'neden işlem yapılmadı' görünür
// olmalı." Approved decisions and rejected decisions use the same struct
// so a caller can log/store both uniformly.
type Decision struct {
	Signal domain.Signal
	// Approved reports whether the signal passed every applicable check.
	// Exit/stop_update signals are always Approved (see Gate's doc
	// comment) — Reason is only meaningful when Approved is false.
	Approved bool
	// Reason is a stable, machine-checkable code (one of the Reason*
	// consts in this file or in sizer.go) explaining a rejection. Empty
	// when Approved is true.
	Reason string
	// Size is the sizing outcome, populated whenever evaluation reached
	// the sizing stage (i.e. every SignalEnter that passed the
	// position/exposure/cooldown/breaker/cash checks). Zero value for
	// exit/stop_update signals and for enter signals rejected before
	// sizing was attempted.
	Size SizeResult
}

// GateInput bundles everything Gate.Evaluate needs beyond the signal
// itself: the current portfolio, the target market's exchange filters (for
// sizing and the min-notional/cash checks), and the pieces of state SPEC.md
// Bölüm 6.5.2's rules depend on that the gate has no way to derive on its
// own (wall-clock time, cooldown history, breaker status).
type GateInput struct {
	Portfolio domain.Portfolio
	// Market must describe signal.Symbol; Evaluate rejects with
	// ReasonMarketMismatch otherwise.
	Market domain.Market
	// Now is the evaluation instant, used for the cooldown check. Callers
	// must pass this explicitly (Clock.Now()) rather than have Gate call
	// time.Now() itself, to keep evaluation deterministic in backtests.
	Now time.Time
	// LastExitAt is the time of the most recent closed position in
	// signal.Symbol, or the zero time if the symbol has never had one (or
	// none within any lookback the caller cares about). Used for the
	// cooldown rule: SPEC.md Bölüm 6.5.2 — "Son 24 saatte aynı sembolde
	// çıkış yok".
	LastExitAt time.Time
	// BreakerOpen reports whether the circuit breaker currently forbids
	// new entries — typically Breaker.Open().
	BreakerOpen bool
}

// Gate applies every rule in SPEC.md Bölüm 6.5.2, in order, to a single
// SignalEnter and turns it into an approved (sizeable) or rejected
// Decision. All rules must pass for a signal to be approved.
//
// Exit and stop_update signals ALWAYS pass through approved, without any
// check. This is deliberate and load-bearing, not an oversight:
//   - SPEC.md Bölüm 6.7 step 8 requires exit/stop signals to be processed
//     BEFORE entries in the daily loop, to free up cash first.
//   - SPEC.md Bölüm 6.5.3 / İ7: the circuit breaker blocks new entries but
//     "çıkışlar ve stop'lar ÇALIŞMAYA DEVAM EDER" — it protects you from
//     NEW risk, it does not lock you into risk you already hold. Gating
//     exits on BreakerOpen (or on max_positions/max_exposure/cooldown,
//     none of which make sense for a signal that reduces exposure) would
//     silently invert that guarantee. Do not add exit-side checks here.
type Gate struct {
	Cfg   config.RiskConfig
	Sizer *Sizer
}

// NewGate constructs a Gate bound to cfg and sizer.
func NewGate(cfg config.RiskConfig, sizer *Sizer) *Gate {
	return &Gate{Cfg: cfg, Sizer: sizer}
}

// Evaluate applies the gate to signal and returns a Decision. See the Gate
// doc comment for why exit/stop_update signals always return Approved.
func (g *Gate) Evaluate(signal domain.Signal, in GateInput) Decision {
	if signal.Kind != domain.SignalEnter {
		return Decision{Signal: signal, Approved: true}
	}

	if in.Market.Symbol != "" && in.Market.Symbol != signal.Symbol {
		return Decision{Signal: signal, Approved: false, Reason: ReasonMarketMismatch}
	}

	maxPositions := g.Cfg.MaxPositions
	if maxPositions <= 0 {
		maxPositions = defaultMaxPositions
	}
	maxExposure := g.Cfg.MaxExposure
	if maxExposure <= 0 {
		maxExposure = defaultMaxExposure
	}
	cooldownHours := g.Cfg.CooldownHours
	if cooldownHours <= 0 {
		cooldownHours = defaultCooldownHours
	}

	// Breaker check first: cheapest, and the most operationally important
	// ("did the system just tell us to stop opening new risk?").
	if in.BreakerOpen {
		return Decision{Signal: signal, Approved: false, Reason: ReasonBreakerOpen}
	}

	if len(in.Portfolio.Positions) >= maxPositions {
		return Decision{Signal: signal, Approved: false, Reason: ReasonMaxPositions}
	}

	if in.Portfolio.Exposure() >= maxExposure {
		return Decision{Signal: signal, Approved: false, Reason: ReasonMaxExposure}
	}

	if _, open := in.Portfolio.Positions[signal.Symbol]; open {
		return Decision{Signal: signal, Approved: false, Reason: ReasonAlreadyOpen}
	}

	if !in.LastExitAt.IsZero() {
		cooldownUntil := in.LastExitAt.Add(time.Duration(cooldownHours) * time.Hour)
		if in.Now.Before(cooldownUntil) {
			return Decision{Signal: signal, Approved: false, Reason: ReasonCooldown}
		}
	}

	// "Nakit yeterli" — checked here as "can we even afford the exchange's
	// minimum order" BEFORE sizing. Sizing itself may further trim qty to
	// fit cash without that being a rejection (SPEC.md Bölüm 6.5.1: "qty *
	// entry > mevcut nakit ise nakde göre kırp" — trimming, not
	// rejecting). insufficient_cash is reserved for the case where even
	// the smallest tradeable order does not fit.
	if minNotional, _ := in.Market.MinNotional.Float64(); minNotional > 0 && in.Portfolio.Cash < minNotional {
		return Decision{Signal: signal, Approved: false, Reason: ReasonInsufficientCash}
	}

	size := g.Sizer.Size(signal, in.Portfolio, in.Market)
	if !size.Accepted {
		return Decision{Signal: signal, Approved: false, Reason: size.Reason, Size: size}
	}

	return Decision{Signal: signal, Approved: true, Size: size}
}
