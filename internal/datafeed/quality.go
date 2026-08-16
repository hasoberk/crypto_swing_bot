package datafeed

import (
	"fmt"
	"math"
	"sort"
	"time"

	"swingbot/internal/domain"
)

// Quality issue kinds — one per row of SPEC.md Bölüm 6.1's kalite kontrolleri
// table.
const (
	IssueMissingCandle = "missing_candle"
	IssueZeroVolume    = "zero_volume"
	IssueInvalidOHLC   = "invalid_ohlc"
	IssueOutlierJump   = "outlier_jump"
	IssueFutureCandle  = "future_candle"
)

// Severity levels. Critical issues mean the candle is rejected outright
// (never stored / never returned in ValidateCandles' clean slice); warn
// issues are informational — the candle is kept but flagged so universe/
// strategy layers can choose to exclude it from the tradable set.
const (
	SeverityCritical = "critical"
	SeverityWarn     = "warn"
)

// outlierJumpThreshold is SPEC.md Bölüm 6.1's "aykırı sıçrama" threshold:
// abs(close/prev_close - 1) > 0.5.
const outlierJumpThreshold = 0.5

// QualityIssue records one finding from ValidateCandles against a single
// candle (or, for IssueMissingCandle, the gap immediately preceding it).
type QualityIssue struct {
	Symbol    string
	Timeframe string
	OpenTime  time.Time
	Kind      string
	Severity  string
	Message   string
}

// ValidateCandles applies SPEC.md Bölüm 6.1's five quality checks, in the
// table's order, to candles (which need not be pre-sorted). It returns:
//
//   - clean: the candles safe to persist/use, in ascending open_time order,
//     with every candle that failed a critical check (invalid_ohlc,
//     future_candle) removed. Candles that only triggered a warn-level issue
//     (zero_volume, outlier_jump) are still included — flagging them for
//     exclusion from the universe/strategy is a downstream concern, not a
//     reason to lose the underlying market data.
//   - issues: every finding, critical and warn, for the caller to log/
//     persist/surface in a `data verify` report.
//
// now is passed in explicitly (rather than reading time.Now internally) so
// callers — and this package's own tests — can pin exactly what "not yet
// closed" means without depending on wall-clock time. SPEC.md Bölüm 4.2 /
// İ2: a candle with open_time + tf > now is a future/still-open candle and
// is rejected unconditionally, no exceptions.
func ValidateCandles(symbol, timeframe string, tf time.Duration, candles []domain.Candle, now time.Time) (clean []domain.Candle, issues []QualityIssue) {
	sorted := make([]domain.Candle, len(candles))
	copy(sorted, candles)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].OpenTime.Before(sorted[j].OpenTime) })

	clean = make([]domain.Candle, 0, len(sorted))

	var (
		havePrev  bool
		prevOpen  time.Time
		prevClose float64
	)

	for _, c := range sorted {
		// Gelecek mum (İ2) — reddet. Checked first: a candle that hasn't
		// closed yet has no business being evaluated for OHLC validity or
		// used as a reference point for gap/outlier detection either.
		if !IsClosed(c.OpenTime, tf, now) {
			issues = append(issues, QualityIssue{
				Symbol: symbol, Timeframe: timeframe, OpenTime: c.OpenTime,
				Kind: IssueFutureCandle, Severity: SeverityCritical,
				Message: fmt.Sprintf("open_time(%s)+%s > now(%s): mum henüz kapanmadı, reddedildi (İ2)",
					c.OpenTime.Format(time.RFC3339), timeframe, now.Format(time.RFC3339)),
			})
			continue
		}

		// Geçersiz OHLC — hata, reddet.
		if c.High < c.Low || c.Close < c.Low || c.Close > c.High {
			issues = append(issues, QualityIssue{
				Symbol: symbol, Timeframe: timeframe, OpenTime: c.OpenTime,
				Kind: IssueInvalidOHLC, Severity: SeverityCritical,
				Message: fmt.Sprintf("geçersiz OHLC: open=%v high=%v low=%v close=%v (high<low veya close [low,high] dışında)",
					c.Open, c.High, c.Low, c.Close),
			})
			continue
		}

		// Eksik mum — ardışık open_time farkı > timeframe. Compared against
		// the previous *accepted* candle so a single rejected bar upstream
		// doesn't itself get reported as a gap.
		if havePrev {
			gap := c.OpenTime.Sub(prevOpen)
			if gap > tf {
				issues = append(issues, QualityIssue{
					Symbol: symbol, Timeframe: timeframe, OpenTime: c.OpenTime,
					Kind: IssueMissingCandle, Severity: SeverityWarn,
					Message: fmt.Sprintf("eksik mum: %s ile %s arasında %s var (beklenen %s)",
						prevOpen.Format(time.RFC3339), c.OpenTime.Format(time.RFC3339), gap, timeframe),
				})
			}
		}

		// Sıfır hacim — şüpheli işaretle, evrenden çıkarılmak üzere bayrakla
		// (veri saklanmaya devam eder; dışlama kararı universe katmanının
		// işidir).
		if c.Volume == 0 {
			issues = append(issues, QualityIssue{
				Symbol: symbol, Timeframe: timeframe, OpenTime: c.OpenTime,
				Kind: IssueZeroVolume, Severity: SeverityWarn,
				Message: "hacim sıfır: şüpheli, evren skorlamasından dışlanmalı",
			})
		}

		// Aykırı sıçrama — uyar, reddetme (gerçek olabilir).
		if havePrev && prevClose != 0 {
			change := math.Abs(c.Close/prevClose - 1)
			if change > outlierJumpThreshold {
				issues = append(issues, QualityIssue{
					Symbol: symbol, Timeframe: timeframe, OpenTime: c.OpenTime,
					Kind: IssueOutlierJump, Severity: SeverityWarn,
					Message: fmt.Sprintf("aykırı sıçrama: close %v -> %v (%.1f%%), insan incelemeli",
						prevClose, c.Close, change*100),
				})
			}
		}

		clean = append(clean, c)
		havePrev = true
		prevOpen = c.OpenTime
		prevClose = c.Close
	}

	return clean, issues
}

// HasCritical reports whether issues contains at least one
// SeverityCritical finding.
func HasCritical(issues []QualityIssue) bool {
	for _, i := range issues {
		if i.Severity == SeverityCritical {
			return true
		}
	}
	return false
}
