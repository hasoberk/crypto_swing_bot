package universe

import (
	"context"
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"swingbot/internal/domain"
	"swingbot/internal/store"
)

// BuildResult is Build's output: the ranked, capped universe as of one date
// plus every exclusion (filter rejections and, if universe.max_symbols cut
// it, rank-based capping too) with its reason. This backs both `swingbot
// universe show` and SPEC.md Bölüm 6.7 step 6 (engine/daily.go's
// universe.Build(asOf) call, once that package exists).
type BuildResult struct {
	AsOf     time.Time
	Universe []ScoredSymbol
	Excluded []Excluded
}

// Symbols returns just the symbol strings of the ranked, capped universe in
// rank order — the shape strategy.Input.Universe (SPEC.md Bölüm 5.4)
// expects.
func (r *BuildResult) Symbols() []string {
	out := make([]string, len(r.Universe))
	for i, s := range r.Universe {
		out[i] = s.Symbol
	}
	return out
}

// candleLookbackDays picks how far back Build reads candles from the store:
// the larger of the effective warmup floor (calendar days, since timeframe
// is daily in practice — SPEC.md Bölüm 8's data.timeframe default) and the
// liquidity lookback, plus a margin so a symbol whose stored history starts
// partway through the window still has its full lookback available once
// fetched.
func candleLookbackDays(p FilterParams) int {
	days := p.effectiveWarmupBars()
	if p.LiquidityLookbackDays > days {
		days = p.LiquidityLookbackDays
	}
	return days + 30
}

// Build assembles the tradable universe as of asOf: it reads markets and
// candles from st (bounded to OpenTime <= asOf — SPEC.md Bölüm 6.2 / İ2's
// look-ahead prohibition, enforced here via store.GetCandles's `to`
// parameter, not by trusting the caller), then runs Filter and Score.
//
// Build intentionally fetches candles for every market whose Quote matches
// params.Quote and skips the rest outright (rather than constructing a
// Candidate for them and letting Filter reject them with ReasonWrongQuote):
// those markets were never backfilled for this quote asset in the first
// place (datafeed.Feed's own quoteFilter, SPEC.md Bölüm 6.1), so fetching
// their candles would just be wasted I/O. Filter's own ReasonWrongQuote path
// is still exercised — and unit-tested — directly, independent of this
// optimization.
func Build(ctx context.Context, st *store.Store, timeframe string, asOf time.Time, params FilterParams, weights Weights) (*BuildResult, error) {
	p := params.withDefaults()

	markets, err := st.ListMarkets(ctx, false) // include delisted: a past asOf may need a since-delisted symbol
	if err != nil {
		return nil, fmt.Errorf("universe: build: list markets: %w", err)
	}

	lookback := candleLookbackDays(p)
	from := asOf.AddDate(0, 0, -lookback)

	wantedMarkets := make([]store.Market, 0, len(markets))
	symbols := make([]string, 0, len(markets))
	for _, m := range markets {
		if m.Quote != p.Quote {
			continue
		}
		wantedMarkets = append(wantedMarkets, m)
		symbols = append(symbols, m.Symbol)
	}

	// One round trip for every symbol's candles instead of one per symbol
	// (store enforces a single SQLite connection — see store.Open's doc
	// comment — so N sequential calls here paid N query round trips for
	// no parallelism benefit; markets is already symbol-ordered, so
	// iterating wantedMarkets below keeps candidate order deterministic).
	candlesBySymbol, err := st.GetCandlesForSymbols(ctx, symbols, timeframe, from, asOf)
	if err != nil {
		return nil, fmt.Errorf("universe: build: get candles: %w", err)
	}

	candidates := make([]Candidate, 0, len(wantedMarkets))
	for _, m := range wantedMarkets {
		candidates = append(candidates, Candidate{Market: toDomainMarket(m), Candles: candlesBySymbol[m.Symbol]})
	}

	filtered := Filter(asOf, candidates, p)
	scored := Score(filtered.Included, weights)
	scored = CapByMaxSymbols(scored, p.MaxSymbols, &filtered)

	return &BuildResult{AsOf: asOf, Universe: scored, Excluded: filtered.Excluded}, nil
}

// toDomainMarket converts store.Store's row shape into domain.Market.
// Decimal fields (tick/step/min-notional) are not used by anything in this
// package but are carried through for completeness / future callers;
// unparseable strings (should not occur — store only ever writes what
// exchange.Market.*.String() produced) fall back to a zero decimal rather
// than failing the whole universe build over a cosmetic field.
func toDomainMarket(m store.Market) domain.Market {
	tick, _ := decimal.NewFromString(m.TickSize)
	step, _ := decimal.NewFromString(m.StepSize)
	minNotional, _ := decimal.NewFromString(m.MinNotional)
	return domain.Market{
		Symbol:      m.Symbol,
		Base:        m.Base,
		Quote:       m.Quote,
		Active:      m.Active,
		TickSize:    tick,
		StepSize:    step,
		MinNotional: minNotional,
		ListedAt:    m.ListedAt,
		DelistedAt:  m.DelistedAt,
	}
}
