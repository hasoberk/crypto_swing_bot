// Package broker defines the Clock and Broker interfaces (SPEC.md Bölüm
// 5.3, 5.5) and the PaperBroker implementation used by both backtest and
// paper trading (SPEC.md Bölüm 6.6.1).
//
// Binding rule (SPEC.md Bölüm 3, İ1): this package depends only on
// internal/domain. The concrete Clock/Broker implementation is the ONLY
// thing that differs between backtest, paper and live — internal/backtest
// and internal/engine drive the same Strategy/risk code against whichever
// implementation they are handed here.
package broker

import (
	"context"
	"errors"
	"time"

	"swingbot/internal/domain"
)

// Clock abstracts "now" (SPEC.md Bölüm 5.3) so the exact same engine loop
// runs against historical time (backtest) or wall-clock time (paper/live).
type Clock interface {
	Now() time.Time
	// Sleep blocks the caller for d in live/paper mode. A backtest clock
	// returns immediately — a backtest must run at full speed, not real
	// time.
	Sleep(d time.Duration)
}

// Broker is the single order-execution surface strategy/risk code talks
// to (SPEC.md Bölüm 5.5). PaperBroker (this package) implements it for
// both backtest and paper trading; LiveBroker (internal/broker/live.go,
// Ajan 13) implements it for real order execution. Neither strategy nor
// risk code imports this package's concrete types — they only see this
// interface, which is what makes İ1 (one code path) hold at compile time.
type Broker interface {
	Mode() string // "backtest" | "paper" | "live"
	Portfolio(ctx context.Context) (domain.Portfolio, error)
	Submit(ctx context.Context, req domain.OrderRequest) (domain.Order, error)
	Cancel(ctx context.Context, symbol, orderID string) error
}

// Costs are the mandatory, never-zero commission/slippage assumptions
// every simulated or live fill applies (SPEC.md İ4). config.CostsConfig
// already refuses to validate a zero-cost setup; Validate here is
// defense-in-depth so a PaperBroker constructed directly (e.g. from a
// test or from a future caller that bypasses internal/config) cannot
// silently run cost-free either.
type Costs struct {
	FeeRate     float64 // e.g. 0.001 = %0.1, çift yön (SPEC.md Bölüm 6.6.1)
	SlippageBps float64 // e.g. 15 = %0.15
}

// ErrCostsNotConfigured is İ4: a broker constructed with zero fees or zero
// slippage is refused outright rather than silently simulating a
// too-good-to-be-true fill.
var ErrCostsNotConfigured = errors.New("broker: fee_rate and slippage_bps must both be greater than zero (İ4 — maliyetler varsayılan olarak kötümser olmalı)")

// Validate returns ErrCostsNotConfigured if either cost component is
// non-positive.
func (c Costs) Validate() error {
	if c.FeeRate <= 0 || c.SlippageBps <= 0 {
		return ErrCostsNotConfigured
	}
	return nil
}
