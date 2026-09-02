// Data segmentation and the "look exactly once" discipline around the
// locked segment (SPEC.md Bölüm 11.1).
package backtest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"swingbot/internal/domain"
)

// DefaultLockedStart is SPEC.md Bölüm 11.1's literal split point:
// Development is everything before this date, Locked is everything at or
// after it ("Kilitli: 2025-07-01 → şimdi").
var DefaultLockedStart = time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC)

// SplitDevLocked partitions candles into the Development segment
// (OpenTime < lockedStart) and the Locked segment (OpenTime >= lockedStart)
// per SPEC.md Bölüm 11.1. Every parameter decision — strategy choice,
// walk-forward window sizes, ParamGrid, Thresholds — must be finalized
// using ONLY dev; locked exists to be looked at exactly once, after every
// other decision is already frozen (see ViewLockedSegment).
func SplitDevLocked(candles map[string][]domain.Candle, lockedStart time.Time) (dev, locked map[string][]domain.Candle) {
	dev = make(map[string][]domain.Candle, len(candles))
	locked = make(map[string][]domain.Candle, len(candles))
	for sym, series := range candles {
		var devSeries, lockedSeries []domain.Candle
		for _, c := range series {
			if c.OpenTime.Before(lockedStart) {
				devSeries = append(devSeries, c)
			} else {
				lockedSeries = append(lockedSeries, c)
			}
		}
		if len(devSeries) > 0 {
			dev[sym] = devSeries
		}
		if len(lockedSeries) > 0 {
			locked[sym] = lockedSeries
		}
	}
	return dev, locked
}

// LockedSegmentRecord is what gets persisted the first (and, by
// discipline, only) time the locked segment is viewed.
type LockedSegmentRecord struct {
	ViewedAt   time.Time
	Thresholds Thresholds
	GitSHA     string
}

// LockedSegmentStore persists a LockedSegmentRecord. This package performs
// no I/O itself (SPEC.md Bölüm 3 layering: internal/backtest does not
// depend on internal/store); the caller (cmd/swingbot) supplies a small
// adapter over store.GetState/SetState, the same pattern already used for
// the circuit breaker's persisted State (see cmd/swingbot's
// breakerStateKey).
type LockedSegmentStore interface {
	// Load returns the previously recorded LockedSegmentRecord, and
	// whether one exists at all (ok=false on the very first view).
	Load(ctx context.Context) (rec LockedSegmentRecord, ok bool, err error)
	Save(ctx context.Context, rec LockedSegmentRecord) error
}

// ErrLockedSegmentAlreadyViewed is returned by ViewLockedSegment when a
// LockedSegmentRecord already exists. SPEC.md Bölüm 11.1: "Kilitli bölüme
// yalnızca bir kez bakılır ve o bakıştan sonra strateji üzerinde
// değişiklik yapılmaz." This is a code-level speed bump, not a technical
// impossibility to bypass — the actual enforcement is a discipline the
// operator commits to (SPEC.md Bölüm 14: "Kendi başarısını abartma...
// kararlarını önceden yazdığın eşiklere göre ver"). force=true in
// ViewLockedSegment exists ONLY to let the tool acknowledge a look that
// already happened, never to make repeated looking convenient.
var ErrLockedSegmentAlreadyViewed = errors.New(
	"backtest: kilitli bölüm (SPEC.md Bölüm 11.1) daha önce görüntülendi — " +
		"strateji artık değiştirilemez; force=true yalnızca bunu KABUL ETMEK içindir, yeniden bakmak için değil",
)

// ErrThresholdsNotRecorded is returned by ViewLockedSegment when rec.
// Thresholds is the zero value — SPEC.md Bölüm 11.4's thresholds must be
// written down BEFORE the locked segment is ever looked at, never derived
// or adjusted afterward.
var ErrThresholdsNotRecorded = errors.New(
	"backtest: Thresholds (SPEC.md Bölüm 11.4) kilitli bölüm görüntülenmeden ÖNCE kaydedilmeli — " +
		"boş Thresholds ile ViewLockedSegment çağrılamaz",
)

// ViewLockedSegment enforces the one-time-look rule.
//
// On the first call (no prior record in st), it validates that rec.
// Thresholds is not the zero value, records rec (defaulting ViewedAt to
// time.Now().UTC() if zero) and returns it with alreadyViewed=false.
//
// On any later call it returns the ORIGINAL record with alreadyViewed=true:
//   - if force is false, err is ErrLockedSegmentAlreadyViewed and the
//     caller MUST NOT proceed to look at the locked segment.
//   - if force is true, err is nil, but the caller MUST surface
//     alreadyViewed=true as a loud, impossible-to-miss warning (see
//     cmd/swingbot's walkforward CLI wiring) — this is meant for
//     acknowledging a discipline violation, not smoothing over one.
func ViewLockedSegment(ctx context.Context, st LockedSegmentStore, rec LockedSegmentRecord, force bool) (recorded LockedSegmentRecord, alreadyViewed bool, err error) {
	if rec.Thresholds == (Thresholds{}) {
		return LockedSegmentRecord{}, false, ErrThresholdsNotRecorded
	}

	existing, ok, err := st.Load(ctx)
	if err != nil {
		return LockedSegmentRecord{}, false, fmt.Errorf("backtest: locked segment durumu okunamadı: %w", err)
	}
	if ok {
		if !force {
			return existing, true, ErrLockedSegmentAlreadyViewed
		}
		return existing, true, nil
	}

	if rec.ViewedAt.IsZero() {
		rec.ViewedAt = time.Now().UTC()
	}
	if err := st.Save(ctx, rec); err != nil {
		return LockedSegmentRecord{}, false, fmt.Errorf("backtest: locked segment durumu kaydedilemedi: %w", err)
	}
	return rec, false, nil
}
