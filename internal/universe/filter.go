// Package universe implements SPEC.md Bölüm 6.2: the tradable-universe
// filter and the cross-sectional scorer that ranks whatever survives it.
//
// Everything in filter.go and scorer.go is a pure function: given the
// candles/market metadata a caller already has in hand, it returns a result,
// with no store/exchange access of its own. That is what makes "her tarih
// için ayrı hesaplanır, geçmişe bakarak değil o tarihteki veriyle" (SPEC.md
// Bölüm 6.2) a property the compiler and unit tests can actually check: the
// look-ahead discipline lives entirely in what a caller puts into a
// Candidate's Candles slice (see Candidate's doc comment), not in this
// package. Build (universe.go) is the one place that talks to store.Store
// and enforces that discipline for the CLI/engine.
package universe

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"swingbot/internal/datafeed"
	"swingbot/internal/domain"
)

// --- exclusion reasons -----------------------------------------------------
//
// Every symbol Filter drops carries one of these as Excluded.Reason. Kept as
// string constants (not an enum type) so they serialize directly into
// metrics_json / CLI output without a marshalling shim — SPEC.md İ6 ("her
// karar açıklanabilir") applies to elimination decisions too, not just
// entries.

const (
	ReasonWrongQuote         = "wrong_quote"                // market.Quote != params.Quote (SPEC.md default "USDT")
	ReasonInactiveAtDate     = "inactive_at_date"            // not listed_at <= asOf < delisted_at
	ReasonUnknownListingDate = "unknown_listing_date"        // neither markets.listed_at nor a first candle available to derive age from
	ReasonTooYoung           = "listing_age_below_minimum"   // age < MinListingAgeDays
	ReasonInsufficientData   = "insufficient_warmup_candles" // fewer candles than the effective warmup floor
	ReasonLowLiquidity       = "median_quote_volume_below_minimum"
	ReasonLeveragedToken     = "leveraged_token"
	ReasonStablecoin         = "stablecoin"
	ReasonRecentQualityFlag  = "recent_quality_flag"
	ReasonCappedByMaxSymbols = "capped_by_max_symbols" // passed every filter but ranked below universe.max_symbols
)

// DefaultExcludePatterns is SPEC.md Bölüm 6.2's literal leveraged-token list,
// and matches config.example.yaml's universe.exclude_patterns default. Each
// pattern is matched as a case-insensitive substring of the full symbol
// (e.g. "UP/" matches "BTCUP/USDT" but, importantly, does NOT match
// "SUPER/USDT" — a naive "symbol contains UP" check would).
var DefaultExcludePatterns = []string{"UP/", "DOWN/", "BULL/", "BEAR/", "3L/", "3S/"}

// defaultStablecoins is SPEC.md Bölüm 6.2's "base bilinen stablecoin
// listesinde" — the set of base assets Filter treats as stablecoins
// regardless of config (config only toggles whether the check runs at all,
// via FilterParams.ExcludeStablecoins; it does not let the list itself be
// replaced, since a materially different list is a business-rule decision
// SPEC.md does not delegate to config.yaml).
var defaultStablecoins = map[string]bool{
	"USDT": true, "USDC": true, "BUSD": true, "DAI": true, "TUSD": true,
	"USDP": true, "FDUSD": true, "PYUSD": true, "GUSD": true, "USDD": true,
	"EURS": true, "EURT": true, "USTC": true, "UST": true, "SUSD": true,
	"LUSD": true, "USDK": true, "USDN": true, "OUSD": true, "USDX": true,
	"FRAX": true, "MIM": true, "USDJ": true, "HUSD": true, "EURI": true,
}

// Defaults for FilterParams fields that fall back to a SPEC-documented value
// when left at their zero value (see FilterParams.withDefaults).
const (
	DefaultMinMedianQuoteVolume = 5_000_000.0 // USDT, SPEC.md Bölüm 6.2
	DefaultMinListingAgeDays    = 180         // SPEC.md Bölüm 6.2
	DefaultLookbackDays         = 30          // "son 30 gün", used for both the liquidity median and the quality-flag window
	// minCandlesForScoring is the scorer's own hard floor (mom_90 needs
	// index len-91): independent of any caller-supplied WarmupBars, Filter
	// never lets a symbol through with fewer candles than this, so Score
	// can assume it unconditionally.
	minCandlesForScoring = 91
)

// FilterParams bundles SPEC.md Bölüm 6.2 / config.yaml's `universe:` block
// plus a couple of mechanical knobs (quote asset, timeframe duration) Filter
// needs but that section 6.2's prose leaves implicit at "USDT" / "günlük".
// Zero-value numeric fields fall back to the SPEC-documented default; see
// withDefaults.
type FilterParams struct {
	Quote                 string        // default "USDT"
	MinMedianQuoteVolume  float64       // default 5_000_000
	MinListingAgeDays     int           // default 180
	WarmupBars            int           // extra floor beyond minCandlesForScoring (e.g. a strategy's SMA(200) requirement); 0 disables it
	ExcludePatterns       []string      // default DefaultExcludePatterns
	ExcludeStablecoins    bool          // caller sets explicitly from config.yaml's universe.exclude_stablecoins (SPEC default true)
	LiquidityLookbackDays int           // default 30 ("son 30 günlük medyan quote_volume")
	QualityLookbackDays   int           // default 30 ("son 30 günde kalite bayrağı almış")
	TimeframeDur          time.Duration // default 24h (daily candles)
	MaxSymbols            int           // config.yaml's universe.max_symbols; <=0 disables the cap (see CapByMaxSymbols)
}

func (p FilterParams) withDefaults() FilterParams {
	if p.Quote == "" {
		p.Quote = "USDT"
	}
	if p.MinMedianQuoteVolume <= 0 {
		p.MinMedianQuoteVolume = DefaultMinMedianQuoteVolume
	}
	if p.MinListingAgeDays <= 0 {
		p.MinListingAgeDays = DefaultMinListingAgeDays
	}
	if len(p.ExcludePatterns) == 0 {
		p.ExcludePatterns = DefaultExcludePatterns
	}
	if p.LiquidityLookbackDays <= 0 {
		p.LiquidityLookbackDays = DefaultLookbackDays
	}
	if p.QualityLookbackDays <= 0 {
		p.QualityLookbackDays = DefaultLookbackDays
	}
	if p.TimeframeDur <= 0 {
		p.TimeframeDur = 24 * time.Hour
	}
	return p
}

// effectiveWarmupBars is the actual minimum candle count Filter enforces:
// the caller's WarmupBars (e.g. a strategy's SMA(200) requirement) combined
// with the scorer's own unconditional floor, whichever is larger.
func (p FilterParams) effectiveWarmupBars() int {
	if p.WarmupBars > minCandlesForScoring {
		return p.WarmupBars
	}
	return minCandlesForScoring
}

// Candidate is one symbol's raw inputs to Filter.
type Candidate struct {
	Market domain.Market

	// Candles must be ascending by OpenTime and contain ONLY bars with
	// OpenTime <= the asOf passed to Filter — this is the one invariant
	// this package trusts callers to uphold and is exactly what stands
	// between "evren skorlaması" and look-ahead bias (SPEC.md Bölüm 6.2 /
	// İ2). Build (universe.go) enforces it via store.GetCandles's `to`
	// bound; a caller feeding this package directly (e.g. tests, or a
	// future backtest hookup) must do the same.
	Candles []domain.Candle
}

// Included is one symbol that survived Filter, carrying the data Score
// needs plus the raw metric a human reading `universe show` cares about
// most directly (its 30-day liquidity).
type Included struct {
	Symbol              string
	Base                string
	MedianQuoteVolume30 float64
	Candles             []domain.Candle // pass-through, ascending, <= asOf
}

// Excluded is one symbol Filter dropped, with a machine-matchable Reason
// (see the Reason* constants) and a human-readable Detail — SPEC.md İ6 and
// this package's "elenen sembollerin eleme gerekçesini" acceptance
// criterion (see .claude/agents/universe-scoring-engineer.md) both apply to
// eliminations, not just to selections.
type Excluded struct {
	Symbol string
	Reason string
	Detail string
}

// Result is Filter's output.
type Result struct {
	AsOf     time.Time
	Included []Included
	Excluded []Excluded
}

// Filter applies SPEC.md Bölüm 6.2's DAHIL ET / HARİÇ TUT rules to
// candidates as of asOf. It never looks past asOf: every decision is made
// from Candidate.Market's listed_at/delisted_at and Candidate.Candles alone.
func Filter(asOf time.Time, candidates []Candidate, params FilterParams) Result {
	p := params.withDefaults()
	res := Result{AsOf: asOf}

	for _, c := range candidates {
		included, medianVol := evaluateCandidate(asOf, c, p, &res)
		if included {
			res.Included = append(res.Included, Included{
				Symbol:              c.Market.Symbol,
				Base:                c.Market.Base,
				MedianQuoteVolume30: medianVol,
				Candles:             c.Candles,
			})
		}
	}
	return res
}

// evaluateCandidate runs every DAHIL ET / HARİÇ TUT check for one candidate,
// in the order SPEC.md Bölüm 6.2 lists them (quote/leverage/stablecoin name
// checks first since they are free; metadata checks next; data-dependent
// checks — warmup, recent quality flags, liquidity — last since they need
// to walk Candles). It appends to res.Excluded and returns
// (included, medianQuoteVolume30).
func evaluateCandidate(asOf time.Time, c Candidate, p FilterParams, res *Result) (bool, float64) {
	m := c.Market
	symbol := m.Symbol

	exclude := func(reason, detail string) (bool, float64) {
		res.Excluded = append(res.Excluded, Excluded{Symbol: symbol, Reason: reason, Detail: detail})
		return false, 0
	}

	if m.Quote != p.Quote {
		return exclude(ReasonWrongQuote, "quote="+m.Quote+", beklenen "+p.Quote)
	}
	if isLeveraged(symbol, p.ExcludePatterns) {
		return exclude(ReasonLeveragedToken, "sembol kaldıraçlı token deseniyle eşleşti")
	}
	if p.ExcludeStablecoins && defaultStablecoins[strings.ToUpper(m.Base)] {
		return exclude(ReasonStablecoin, "base="+m.Base+" bilinen stablecoin listesinde")
	}
	if !m.IsListedAt(asOf) {
		return exclude(ReasonInactiveAtDate, "listed_at/delisted_at asOf'u ("+asOf.Format("2006-01-02")+") kapsamıyor")
	}

	listedAt := m.ListedAt
	if listedAt.IsZero() && len(c.Candles) > 0 {
		// markets.listed_at may be NULL ("bilinmiyorsa NULL", SPEC.md Bölüm
		// 4.1); fall back to the first stored candle as a conservative proxy
		// — backfill can only start at or after the true listing date, so
		// this never OVERESTIMATES age, only underestimates it, biasing
		// toward exclusion rather than toward silently admitting a coin
		// that is actually too young.
		listedAt = c.Candles[0].OpenTime
	}
	if listedAt.IsZero() {
		return exclude(ReasonUnknownListingDate, "listed_at bilinmiyor ve mum verisi yok, yaş doğrulanamıyor")
	}
	ageDays := int(asOf.Sub(listedAt).Hours() / 24)
	if ageDays < p.MinListingAgeDays {
		return exclude(ReasonTooYoung, "listelenme yaşı "+itoa(ageDays)+" gün (< "+itoa(p.MinListingAgeDays)+")")
	}

	needBars := p.effectiveWarmupBars()
	if len(c.Candles) < needBars {
		return exclude(ReasonInsufficientData, itoa(len(c.Candles))+" mum var, en az "+itoa(needBars)+" gerekiyor")
	}

	if issues := recentQualityIssues(symbol, c.Candles, asOf, p.QualityLookbackDays, p.TimeframeDur); len(issues) > 0 {
		return exclude(ReasonRecentQualityFlag, qualityIssueSummary(issues))
	}

	medianVol, ok := medianQuoteVolume(c.Candles, asOf, p.LiquidityLookbackDays)
	if !ok {
		return exclude(ReasonInsufficientData, "son "+itoa(p.LiquidityLookbackDays)+" günde mum yok, medyan hacim hesaplanamadı")
	}
	if medianVol < p.MinMedianQuoteVolume {
		return exclude(ReasonLowLiquidity, "30g medyan quote_volume="+ftoa(medianVol)+" (< "+ftoa(p.MinMedianQuoteVolume)+")")
	}

	return true, medianVol
}

// isLeveraged reports whether symbol matches any of patterns as a
// case-insensitive substring — see DefaultExcludePatterns' doc comment for
// why "UP/" rather than "UP" is the right shape of pattern.
func isLeveraged(symbol string, patterns []string) bool {
	up := strings.ToUpper(symbol)
	for _, pat := range patterns {
		if pat == "" {
			continue
		}
		if strings.Contains(up, strings.ToUpper(pat)) {
			return true
		}
	}
	return false
}

// recentQualityIssues re-runs datafeed.ValidateCandles (SPEC.md Bölüm 6.1)
// against just the last lookbackDays of candles, to answer "son 30 günde
// kalite bayrağı aldı mı" without a dedicated quality-issues table: the
// candles table only ever stores datafeed's "clean" output (critical issues
// are rejected before they are ever written, per datafeed.Feed.fetchSymbol),
// so re-validating the already-stored data recovers exactly the warn-level
// findings (zero_volume, outlier_jump, missing_candle) that were logged, not
// persisted, at ingestion time.
//
// now is passed to ValidateCandles as asOf+tf, not asOf itself: the asOf bar
// is by definition the most recent CLOSED bar (SPEC.md Strategy.Input doc
// comment — "Son eleman AS-OF mumudur ve KAPANMIŞTIR"), so validating with
// now=asOf would incorrectly flag it as a future/unclosed candle.
func recentQualityIssues(symbol string, candles []domain.Candle, asOf time.Time, lookbackDays int, tf time.Duration) []datafeed.QualityIssue {
	windowStart := asOf.AddDate(0, 0, -lookbackDays)
	window := make([]domain.Candle, 0, lookbackDays+1)
	for _, c := range candles {
		if !c.OpenTime.Before(windowStart) && !c.OpenTime.After(asOf) {
			window = append(window, c)
		}
	}
	if len(window) == 0 {
		return nil
	}
	_, issues := datafeed.ValidateCandles(symbol, "", tf, window, asOf.Add(tf))
	return issues
}

func qualityIssueSummary(issues []datafeed.QualityIssue) string {
	kinds := map[string]int{}
	for _, iss := range issues {
		kinds[iss.Kind]++
	}
	var parts []string
	for kind, n := range kinds {
		parts = append(parts, kind+"x"+itoa(n))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

// medianQuoteVolume computes the median (not mean — SPEC.md Bölüm 6.2:
// "tek bir pump günü ortalamayı şişirir") quote_volume of candles whose
// OpenTime falls in (asOf-lookbackDays, asOf]. ok is false if the window is
// empty.
func medianQuoteVolume(candles []domain.Candle, asOf time.Time, lookbackDays int) (float64, bool) {
	windowStart := asOf.AddDate(0, 0, -lookbackDays)
	vols := make([]float64, 0, lookbackDays+1)
	for _, c := range candles {
		if !c.OpenTime.Before(windowStart) && !c.OpenTime.After(asOf) {
			vols = append(vols, c.QuoteVolume)
		}
	}
	if len(vols) == 0 {
		return 0, false
	}
	sort.Float64s(vols)
	n := len(vols)
	if n%2 == 1 {
		return vols[n/2], true
	}
	return (vols[n/2-1] + vols[n/2]) / 2, true
}

// CapByMaxSymbols truncates scored (already sorted best-first by Score,
// SPEC.md Bölüm 8's universe.max_symbols) to at most maxSymbols entries,
// appending a ReasonCappedByMaxSymbols Excluded entry for everything cut so
// the reason surfaces in `universe show` like any other elimination. A
// maxSymbols <= 0 disables the cap (config.example.yaml defaults it to 100,
// which SPEC.md Bölüm 6.2's own filter thresholds are expected to stay
// under in practice — see this package's "20-100 sembol" acceptance
// criterion).
func CapByMaxSymbols(scored []ScoredSymbol, maxSymbols int, res *Result) []ScoredSymbol {
	if maxSymbols <= 0 || len(scored) <= maxSymbols {
		return scored
	}
	for _, s := range scored[maxSymbols:] {
		res.Excluded = append(res.Excluded, Excluded{
			Symbol: s.Symbol,
			Reason: ReasonCappedByMaxSymbols,
			Detail: "rank " + itoa(s.Rank) + " > max_symbols=" + itoa(maxSymbols),
		})
	}
	return scored[:maxSymbols]
}

// itoa/ftoa are tiny local helpers to keep the exclude(...) call sites above
// terse.
func itoa(n int) string {
	return strconv.Itoa(n)
}

func ftoa(f float64) string {
	if math.IsNaN(f) {
		return "NaN"
	}
	return fmt.Sprintf("%.2f", f)
}
