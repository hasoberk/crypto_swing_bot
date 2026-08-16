// Package backtest is the day-by-day simulation loop (SPEC.md Bölüm 6.7,
// run in "replay" form) that proves a Strategy's historical performance,
// plus the metrics and HTML report that make that performance readable.
//
// Binding rule (SPEC.md İ1): backtest, paper and live all drive the exact
// same strategy.Strategy and (once it lands) risk code. This package's
// Engine only differs from the live/paper daily engine (internal/engine,
// Ajan 11) in which broker.Clock/broker.Broker it is wired to — see
// engine.go's doc comment.
package backtest

import (
	"encoding/json"

	"swingbot/internal/broker"
	"swingbot/internal/config"
)

// CostsFromConfig converts config.CostsConfig into the broker.Costs a
// PaperBroker needs. It is the one place backtest translates between the
// config schema (SPEC.md Bölüm 8) and the broker package's own type,
// keeping broker free of a config import.
func CostsFromConfig(c config.CostsConfig) broker.Costs {
	return broker.Costs{FeeRate: c.FeeRate, SlippageBps: c.SlippageBps}
}

// CostsJSON marshals costs for the runs.costs_json column (SPEC.md Bölüm
// 4.1) — the cost assumptions a run used, kept alongside its metrics for
// reproducibility.
func CostsJSON(costs broker.Costs) (string, error) {
	b, err := json.Marshal(costs)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
