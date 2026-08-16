package datafeed

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"swingbot/internal/domain"
	"swingbot/internal/exchange"
	"swingbot/internal/store"
)

// survivorshipBiasWarning is SPEC.md Bölüm 6.1's mandated note: because this
// system starts from zero, it has no record of symbols delisted before it
// began running. Every report that could inform a trading decision
// (`data verify`, and eventually backtest reports per Bölüm 7.3) must carry
// this line rather than silently presenting today's `markets` table as a
// complete historical universe.
const survivorshipBiasWarning = "survivorship bias içerir: proje sıfırdan başladığı için bu sistem çalışmaya başlamadan önce delist edilmiş semboller markets tablosunda yoktur; yalnızca bundan sonraki delistler active=0/delisted_at ile kaydedilir. Gösterilen geçmiş performans gerçekte olduğundan iyi görünebilir."

// Feed is the entry point for everything this package does: syncing the
// markets table against the exchange's current symbol list, backfilling and
// incrementally updating candles, and verifying stored data quality. One
// Feed is built per (exchange, store, timeframe) combination — SPEC.md
// Bölüm 8's config.data.timeframe is normally "1d".
type Feed struct {
	exchange exchange.Exchange
	store    *store.Store

	timeframe     string
	pageSize      int
	quoteFilter   string // "" disables filtering; SPEC.md default is "USDT"
	backfillYears int

	logger *slog.Logger
	now    func() time.Time // overridable for tests; always UTC
}

// Option configures a Feed built by NewFeed.
type Option func(*Feed)

// WithPageSize overrides the default page size (1000). Values outside
// [500, 1000] are clamped — see clampPageSize's doc comment.
func WithPageSize(n int) Option { return func(f *Feed) { f.pageSize = clampPageSize(n) } }

// WithQuoteFilter restricts the symbols Backfill/Update/Verify operate on
// (when no explicit symbol list is given) to markets whose Quote equals q.
// Pass "" to disable filtering entirely.
func WithQuoteFilter(q string) Option { return func(f *Feed) { f.quoteFilter = q } }

// WithBackfillYears overrides the default backfill lookback (3 years, per
// SPEC.md Bölüm 6.1 / config.yaml's data.backfill_years).
func WithBackfillYears(years int) Option {
	return func(f *Feed) {
		if years > 0 {
			f.backfillYears = years
		}
	}
}

// WithLogger overrides the *slog.Logger used for quality-issue and
// delisting log lines. Defaults to slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(f *Feed) {
		if l != nil {
			f.logger = l
		}
	}
}

// WithClock overrides Feed's notion of "now" (tests only). Defaults to
// time.Now, always normalized to UTC per SPEC.md Bölüm 4.2.
func WithClock(now func() time.Time) Option {
	return func(f *Feed) {
		if now != nil {
			f.now = now
		}
	}
}

// NewFeed builds a Feed. timeframe must be parseable by ParseTimeframe
// (e.g. "1d").
func NewFeed(ex exchange.Exchange, st *store.Store, timeframe string, opts ...Option) *Feed {
	f := &Feed{
		exchange:      ex,
		store:         st,
		timeframe:     timeframe,
		pageSize:      1000,
		quoteFilter:   "USDT",
		backfillYears: 3,
		logger:        slog.Default(),
		now:           func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// SymbolReport summarizes one symbol's outcome from a Backfill or Update
// pass.
type SymbolReport struct {
	Symbol         string
	Pages          int
	CandlesWritten int // count of "clean" candles upserted (quality-rejected candles are not included)
	Issues         []QualityIssue
}

// BackfillReport summarizes a full Feed.Backfill call.
type BackfillReport struct {
	Symbols  []SymbolReport
	Delisted []string // symbols newly marked active=0 by this run's market sync
}

// UpdateReport summarizes a full Feed.Update call.
type UpdateReport struct {
	Symbols  []SymbolReport
	Delisted []string
}

// SymbolVerifyResult is one symbol's outcome from Feed.Verify.
type SymbolVerifyResult struct {
	Symbol      string
	Timeframe   string
	CandleCount int
	Issues      []QualityIssue
}

// VerifyReport is Feed.Verify's output — the payload behind
// `swingbot data verify`.
type VerifyReport struct {
	GeneratedAt    time.Time
	Symbols        []SymbolVerifyResult
	TotalIssues    int
	CriticalIssues int
	Warnings       []string // always includes survivorshipBiasWarning
	OK             bool     // true iff CriticalIssues == 0 (SPEC.md Bölüm 6.1 kabul kriteri: "sıfır kritik hata")
}

// SyncMarkets fetches the exchange's current symbol list and reconciles it
// against the markets table:
//
//   - every fetched symbol is upserted (SPEC.md Bölüm 6.1: this is the only
//     source of truth for tick/step/min-notional filters and the current
//     active flag);
//   - any symbol that is active in the store but absent from this fetch is
//     marked delisted (active=0, delisted_at=now) via store.MarkDelisted —
//     never deleted. Its candles are untouched.
//
// It returns the symbols newly marked delisted by this call.
func (f *Feed) SyncMarkets(ctx context.Context) ([]string, error) {
	fetched, err := f.exchange.FetchMarkets(ctx)
	if err != nil {
		return nil, fmt.Errorf("datafeed: sync markets: fetch: %w", err)
	}

	now := f.now().UTC()
	seen := make(map[string]bool, len(fetched))
	for _, m := range fetched {
		seen[m.Symbol] = true
		sm := store.Market{
			Symbol:      m.Symbol,
			Base:        m.Base,
			Quote:       m.Quote,
			Active:      m.Active,
			TickSize:    m.TickSize.String(),
			StepSize:    m.StepSize.String(),
			MinNotional: m.MinNotional.String(),
			ListedAt:    m.ListedAt,
			DelistedAt:  m.DelistedAt,
			UpdatedAt:   now,
		}
		if err := f.store.UpsertMarket(ctx, sm); err != nil {
			return nil, fmt.Errorf("datafeed: sync markets: upsert %s: %w", m.Symbol, err)
		}
	}

	existingActive, err := f.store.ListMarkets(ctx, true)
	if err != nil {
		return nil, fmt.Errorf("datafeed: sync markets: list active: %w", err)
	}

	var delisted []string
	for _, m := range existingActive {
		if seen[m.Symbol] {
			continue
		}
		if err := f.store.MarkDelisted(ctx, m.Symbol, now); err != nil {
			return nil, fmt.Errorf("datafeed: sync markets: mark delisted %s: %w", m.Symbol, err)
		}
		delisted = append(delisted, m.Symbol)
		f.logger.Warn("datafeed: symbol delisted (no longer in FetchMarkets)", "symbol", m.Symbol, "delisted_at", now)
	}
	return delisted, nil
}

// defaultSymbols returns the symbols Backfill/Update/Verify operate on when
// no explicit list is given: every active market matching f.quoteFilter
// (SPEC.md Bölüm 8's config.exchange.quote, normally "USDT").
func (f *Feed) defaultSymbols(ctx context.Context) ([]string, error) {
	markets, err := f.store.ListMarkets(ctx, true)
	if err != nil {
		return nil, fmt.Errorf("datafeed: list markets: %w", err)
	}
	out := make([]string, 0, len(markets))
	for _, m := range markets {
		if f.quoteFilter != "" && m.Quote != f.quoteFilter {
			continue
		}
		out = append(out, m.Symbol)
	}
	return out, nil
}

// logIssue writes one QualityIssue to f.logger at a level matching its
// severity. Critical issues (rejected candles) log at Error; warn issues log
// at Warn. Nothing about a quality issue is ever dropped silently — this is
// the "sessiz hata yasak" rule applied to data, not just to hard errors.
func (f *Feed) logIssue(iss QualityIssue) {
	attrs := []any{"symbol", iss.Symbol, "timeframe", iss.Timeframe, "open_time", iss.OpenTime, "kind", iss.Kind, "message", iss.Message}
	if iss.Severity == SeverityCritical {
		f.logger.Error("datafeed: quality check rejected candle", attrs...)
	} else {
		f.logger.Warn("datafeed: quality check flagged candle", attrs...)
	}
}

// fetchSymbol pages symbol's candles from since forward, validates each page
// (ValidateCandles) and persists the clean result (store.UpsertCandles) one
// page at a time. Shared by Backfill (since = resume point or backfill
// horizon) and Update (since = last stored bar minus a small rewind window).
func (f *Feed) fetchSymbol(ctx context.Context, symbol string, since time.Time) (SymbolReport, error) {
	report := SymbolReport{Symbol: symbol}

	tfDur, err := ParseTimeframe(f.timeframe)
	if err != nil {
		return report, err
	}

	err = FetchPages(ctx, f.exchange, symbol, f.timeframe, since, f.pageSize, f.now, func(page []domain.Candle) error {
		report.Pages++
		nowTs := f.now().UTC()
		clean, issues := ValidateCandles(symbol, f.timeframe, tfDur, page, nowTs)
		report.Issues = append(report.Issues, issues...)
		for _, iss := range issues {
			f.logIssue(iss)
		}
		if len(clean) == 0 {
			return nil
		}
		if err := f.store.UpsertCandles(ctx, symbol, f.timeframe, clean); err != nil {
			return fmt.Errorf("store upsert candles %s: %w", symbol, err)
		}
		report.CandlesWritten += len(clean)
		return nil
	})
	if err != nil {
		return report, fmt.Errorf("datafeed: fetch %s %s: %w", symbol, f.timeframe, err)
	}
	return report, nil
}

// Backfill implements SPEC.md Bölüm 6.1's "Backfill (ilk dolum)": for every
// symbol in opts.Symbols (or every active quoteFilter market if empty), page
// candles from the later of (opts.Years ago) or "the bar after whatever is
// already stored" up to now, persisting each page immediately.
//
// Resumability: because fetchSymbol persists per page and this function
// recomputes each symbol's starting point from store.MaxOpenTime, an
// interrupted Backfill (process killed, network dropped) can simply be
// re-run — it picks up exactly where it left off with no separate
// checkpoint bookkeeping (see FetchPages' doc comment).
//
// Sessiz hata yasak: the first symbol that returns a hard error (not a
// quality issue — those are collected in the report, not fatal) stops the
// whole run and returns that error. A partially completed Backfill is safe
// to resume; silently limping past a broken symbol and reporting success is
// not.
func (f *Feed) Backfill(ctx context.Context, opts BackfillOptions) (*BackfillReport, error) {
	delisted, err := f.SyncMarkets(ctx)
	if err != nil {
		return nil, fmt.Errorf("datafeed: backfill: %w", err)
	}

	symbols := opts.Symbols
	if len(symbols) == 0 {
		symbols, err = f.defaultSymbols(ctx)
		if err != nil {
			return nil, fmt.Errorf("datafeed: backfill: %w", err)
		}
	}

	years := opts.Years
	if years <= 0 {
		years = f.backfillYears
	}

	tfDur, err := ParseTimeframe(f.timeframe)
	if err != nil {
		return nil, fmt.Errorf("datafeed: backfill: %w", err)
	}

	report := &BackfillReport{Delisted: delisted}
	for _, symbol := range symbols {
		since := f.now().UTC().AddDate(-years, 0, 0)

		last, ok, err := f.store.MaxOpenTime(ctx, symbol, f.timeframe)
		if err != nil {
			return report, fmt.Errorf("datafeed: backfill: max open time %s: %w", symbol, err)
		}
		if ok {
			resume := last.Add(tfDur)
			if resume.After(since) {
				since = resume
			}
		}

		sr, err := f.fetchSymbol(ctx, symbol, since)
		report.Symbols = append(report.Symbols, sr)
		if err != nil {
			f.logger.Error("datafeed: backfill failed, stopping (sessiz hata yasak)", "symbol", symbol, "error", err)
			return report, err
		}
	}
	return report, nil
}

// BackfillOptions configures Feed.Backfill.
type BackfillOptions struct {
	Symbols []string // empty -> every active quoteFilter market
	Years   int      // <= 0 -> f's configured default (SPEC.md default 3)
}

// updateRewindBars is SPEC.md Bölüm 6.1's "Son 3 mumu yeniden çek ve üzerine
// yaz" requirement expressed as a count of bars to step back from the last
// stored bar before resuming.
const updateRewindBars = 3

// Update implements SPEC.md Bölüm 6.1's "Artımlı güncelleme": for every
// symbol, resume from MAX(open_time) rewound by updateRewindBars-1 bars (so
// the last 3 stored candles are re-fetched and overwritten in place —
// exchanges sometimes correct a recently closed candle), then page forward
// to now. Symbols with no stored data yet are backfilled from scratch
// (f.backfillYears), so Update also works as a bootstrap for a newly listed
// symbol without requiring a separate Backfill call.
//
// Idempotency (Faz 0 kabul kriteri: "iki kez koş, satır sayısı değişmiyor"):
// UpsertCandles overwrites on the (symbol, timeframe, open_time) primary
// key, and FetchPages/ValidateCandles never persist a candle that is not yet
// closed, so re-running Update with no new closed bar available writes zero
// new rows — only existing rows get their OHLCV columns refreshed in place.
//
// Sessiz hata yasak: same fail-fast contract as Backfill — a hard error on
// any symbol stops the run immediately rather than letting the rest of the
// update proceed on top of an unverified/incomplete state.
func (f *Feed) Update(ctx context.Context) (*UpdateReport, error) {
	delisted, err := f.SyncMarkets(ctx)
	if err != nil {
		return nil, fmt.Errorf("datafeed: update: %w", err)
	}

	symbols, err := f.defaultSymbols(ctx)
	if err != nil {
		return nil, fmt.Errorf("datafeed: update: %w", err)
	}

	tfDur, err := ParseTimeframe(f.timeframe)
	if err != nil {
		return nil, fmt.Errorf("datafeed: update: %w", err)
	}

	report := &UpdateReport{Delisted: delisted}
	for _, symbol := range symbols {
		var since time.Time

		last, ok, err := f.store.MaxOpenTime(ctx, symbol, f.timeframe)
		if err != nil {
			return report, fmt.Errorf("datafeed: update: max open time %s: %w", symbol, err)
		}
		if !ok {
			since = f.now().UTC().AddDate(-f.backfillYears, 0, 0)
		} else {
			since = last.Add(-time.Duration(updateRewindBars-1) * tfDur)
		}

		sr, err := f.fetchSymbol(ctx, symbol, since)
		report.Symbols = append(report.Symbols, sr)
		if err != nil {
			f.logger.Error("datafeed: update failed, stopping (sessiz hata yasak — eski veriyle karar verilmesine izin verilmez)", "symbol", symbol, "error", err)
			return report, err
		}
	}
	return report, nil
}

// Verify implements SPEC.md Bölüm 6.1's "swingbot data verify": it re-runs
// ValidateCandles against every candle already stored for each symbol (or
// every active quoteFilter market, if symbols is empty) and aggregates the
// findings. Unlike Backfill/Update, Verify never mutates the store — it is a
// read-only audit.
//
// OK is true iff CriticalIssues == 0, matching Faz 0's acceptance
// criterion ("`data verify` sıfır kritik hata veriyor"). Warnings always
// includes survivorshipBiasWarning, per SPEC.md Bölüm 6.1's explicit
// instruction to surface that limitation in every report that could
// influence a decision, not just backtest reports.
func (f *Feed) Verify(ctx context.Context, symbols []string) (*VerifyReport, error) {
	var err error
	if len(symbols) == 0 {
		symbols, err = f.defaultSymbols(ctx)
		if err != nil {
			return nil, fmt.Errorf("datafeed: verify: %w", err)
		}
	}

	tfDur, err := ParseTimeframe(f.timeframe)
	if err != nil {
		return nil, fmt.Errorf("datafeed: verify: %w", err)
	}

	report := &VerifyReport{
		GeneratedAt: f.now().UTC(),
		Warnings:    []string{survivorshipBiasWarning},
	}

	nowTs := f.now().UTC()
	for _, symbol := range symbols {
		candles, err := f.store.GetCandles(ctx, symbol, f.timeframe, time.Time{}, time.Time{})
		if err != nil {
			return report, fmt.Errorf("datafeed: verify: get candles %s: %w", symbol, err)
		}
		_, issues := ValidateCandles(symbol, f.timeframe, tfDur, candles, nowTs)

		report.Symbols = append(report.Symbols, SymbolVerifyResult{
			Symbol:      symbol,
			Timeframe:   f.timeframe,
			CandleCount: len(candles),
			Issues:      issues,
		})
		report.TotalIssues += len(issues)
		if HasCritical(issues) {
			for _, iss := range issues {
				if iss.Severity == SeverityCritical {
					report.CriticalIssues++
				}
			}
		}
	}
	report.OK = report.CriticalIssues == 0
	return report, nil
}
