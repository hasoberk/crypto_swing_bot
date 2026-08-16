// Package strategy defines the Strategy interface (SPEC.md Bölüm 5.4) that
// every trading strategy (momentum, trendfollow, ...) implements.
//
// Binding rule (SPEC.md Bölüm 3, İ1): this package depends only on
// internal/domain and internal/indicator. It MUST NOT import store,
// exchange, broker or backtest — a strategy that could see those packages
// could accidentally do I/O, see wall-clock time, or peek at data the
// backtest engine hasn't handed it yet, breaking İ1 (one code path) and İ2
// (signal at t, trade at t+1). That asymmetry is what lets the compiler
// enforce determinism, not just a code review comment.
package strategy

import (
	"time"

	"swingbot/internal/domain"
)

// Input is everything a Strategy sees on a given evaluation date.
//
// Series[s] contains warmup-inclusive history for symbol s. Its LAST
// element is the AS-OF bar and is guaranteed CLOSED — the caller (the
// backtest engine, or the live/paper daily engine) never hands over an
// unclosed bar. Series[s] is also guaranteed to have cap == len: a
// Strategy that tries to read past the end of the slice (e.g.
// series[len(series)] or a re-slice past len) hits a Go runtime bounds
// panic instead of silently reading future data. That is the concrete,
// compiler/runtime-enforced mechanism behind İ2 — it does not rely on the
// strategy author remembering not to peek ahead.
type Input struct {
	AsOf      time.Time
	Series    map[string][]domain.Candle // symbol -> kronolojik mumlar (warmup dahil)
	Universe  []string                   // bu tarihte işlem yapılabilir semboller
	Portfolio domain.Portfolio
}

// Strategy turns market history into trading intent. Implementations must
// be pure and deterministic:
//   - No I/O: no HTTP, no database, no file access.
//   - No wall-clock reads (time.Now()) — AsOf is the only notion of "now".
//   - Same Input in, same []domain.Signal out, every time. Any randomness
//     must be seeded explicitly and the seed exposed via Params() so a run
//     is reproducible from its recorded parameters alone.
//
// These constraints are what let a Strategy be unit-tested with synthetic
// candle series and reused unchanged across backtest, paper and live modes
// (SPEC.md İ1) — the same interface, only Clock/Broker differ underneath.
type Strategy interface {
	// Name identifies the strategy, e.g. for runs.strategy and trades.strategy.
	Name() string

	// WarmupBars is the number of bars the longest indicator this strategy
	// uses needs before it produces a real (non-NaN) value. The engine
	// will not include a symbol in Universe until it has at least this
	// many closed bars of history.
	WarmupBars() int

	// Params returns every parameter (and, if applicable, random seed)
	// that determines this strategy's behavior. Recorded verbatim into
	// runs.params_json for reproducibility.
	Params() map[string]any

	// Evaluate produces this AsOf date's raw signals. It must not mutate
	// in.Series, in.Universe or in.Portfolio.
	Evaluate(in Input) ([]domain.Signal, error)
}
