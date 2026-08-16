// Package datafeed owns everything SPEC.md Bölüm 6.1 assigns to "veri
// toplama": pulling OHLCV candles from an exchange.Exchange into the
// store.Store, keeping the markets table's delisted bookkeeping current, and
// running the data-quality checks that decide what is safe to hand to
// upstream layers (universe, strategy, backtest, engine).
//
// This package is also İ2's first line of defense (SPEC.md Bölüm 4.2): a
// still-open candle must never reach the strategy layer. exchange.Exchange
// is explicitly allowed to return one (see exchange.go's FetchOHLCV doc
// comment); datafeed is where it gets filtered out, once, so every caller
// downstream can trust that anything read from the candles table is closed.
package datafeed

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"swingbot/internal/domain"
	"swingbot/internal/exchange"
)

// ErrUnknownTimeframe is returned by ParseTimeframe for a string it cannot
// parse as "<n><unit>" with unit one of m/h/d/w.
var ErrUnknownTimeframe = errors.New("datafeed: unknown timeframe")

// ParseTimeframe parses a SPEC.md-style timeframe string ("1d", "4h", "15m",
// "1w") into a time.Duration. It is intentionally strict (no fractional
// units, no bare numbers) since a misparsed timeframe would silently corrupt
// every gap/closed-candle calculation downstream.
func ParseTimeframe(tf string) (time.Duration, error) {
	if len(tf) < 2 {
		return 0, fmt.Errorf("%w: %q", ErrUnknownTimeframe, tf)
	}
	unit := tf[len(tf)-1]
	n, err := strconv.Atoi(tf[:len(tf)-1])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%w: %q", ErrUnknownTimeframe, tf)
	}
	switch unit {
	case 'm':
		return time.Duration(n) * time.Minute, nil
	case 'h':
		return time.Duration(n) * time.Hour, nil
	case 'd':
		return time.Duration(n) * 24 * time.Hour, nil
	case 'w':
		return time.Duration(n) * 7 * 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("%w: %q", ErrUnknownTimeframe, tf)
	}
}

// IsClosed implements SPEC.md Bölüm 4.2's rule verbatim: a candle is closed
// only if openTime+tf <= now. Exported so other layers (universe, strategy,
// engine) can apply the same check defensively — SPEC.md is explicit that
// datafeed is the "first line of defense", not the only possible one.
func IsClosed(openTime time.Time, tf time.Duration, now time.Time) bool {
	return !openTime.Add(tf).After(now)
}

// clampPageSize enforces SPEC.md Bölüm 6.1's "Sayfa boyutu genelde 500–1000
// mum" guidance: an unconfigured or out-of-range page size is coerced into
// [500, 1000] rather than silently sent to the exchange as-is (a page size
// of, say, 1 would need thousands of paginated requests for the 3-year
// backfill target and hammer the rate limiter for no reason; a page size in
// the tens of thousands risks exceeding what most exchange APIs accept per
// call).
func clampPageSize(n int) int {
	switch {
	case n <= 0:
		return 1000
	case n < 500:
		return 500
	case n > 1000:
		return 1000
	default:
		return n
	}
}

// FetchPages paginates exchange.Exchange.FetchOHLCV for a single
// symbol/timeframe starting at since, invoking onPage once per page in
// chronological order until the exchange has no more data at or before now()
// (the loop re-evaluates now() on every iteration so a long backfill's tail
// end still stops at whatever "now" has become, never running forever).
//
// It does not touch the store itself — callers are expected to persist each
// page inside onPage. That is deliberate: because store.UpsertCandles writes
// each call inside its own transaction, persisting per-page instead of
// buffering the whole history in memory is what makes a backfill resumable
// (SPEC.md Bölüm 6.1: "İlerlemeyi kalıcı tut; yarıda kesilirse kaldığı yerden
// devam etsin"). If FetchPages is interrupted (process killed, network dies)
// after N pages, everything up to page N is already durably committed; the
// next run's `since` is recomputed from store.MaxOpenTime, so it resumes
// exactly where it left off without a separate checkpoint table.
func FetchPages(
	ctx context.Context,
	ex exchange.Exchange,
	symbol, timeframe string,
	since time.Time,
	pageSize int,
	now func() time.Time,
	onPage func([]domain.Candle) error,
) error {
	tfDur, err := ParseTimeframe(timeframe)
	if err != nil {
		return err
	}
	// Note: FetchPages itself does not enforce SPEC.md Bölüm 6.1's
	// "500-1000 mum/sayfa" guidance — that is Feed's job (see
	// clampPageSize / WithPageSize), so this generic paginator stays usable
	// with any page size a caller (including tests) wants.
	if pageSize <= 0 {
		pageSize = 1000
	}

	cursor := since
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		nowTs := now().UTC()
		if !cursor.Before(nowTs) {
			// Cursor has caught up to "now": nothing closed left to fetch.
			return nil
		}

		candles, err := ex.FetchOHLCV(ctx, symbol, timeframe, cursor, pageSize)
		if err != nil {
			return fmt.Errorf("datafeed: FetchOHLCV %s %s since=%s: %w", symbol, timeframe, cursor.Format(time.RFC3339), err)
		}
		if len(candles) == 0 {
			return nil
		}

		// Exchanges are expected to return candles in ascending open_time
		// order already, but sort defensively — nothing downstream should
		// have to trust that assumption silently.
		sort.Slice(candles, func(i, j int) bool { return candles[i].OpenTime.Before(candles[j].OpenTime) })

		if err := onPage(candles); err != nil {
			return err
		}

		last := candles[len(candles)-1].OpenTime
		next := last.Add(tfDur)
		if !next.After(cursor) {
			// Defensive guard: a misbehaving exchange/mock that returns the
			// same or earlier open_time forever would otherwise spin this
			// loop indefinitely. Fail loudly instead (İ-adjacent: no silent
			// hang standing in for "no more data").
			return fmt.Errorf("datafeed: pagination did not advance past %s for %s %s (exchange returned open_time <= cursor)", cursor.Format(time.RFC3339), symbol, timeframe)
		}
		cursor = next

		if len(candles) < pageSize {
			// Short page: the exchange had less than a full page available,
			// meaning we have caught up to its most recent data (or its
			// full history, on a very young symbol). One more request would
			// almost always return empty; skip it.
			return nil
		}
	}
}
