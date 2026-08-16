package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"swingbot/internal/domain"
)

// UpsertCandles bulk-inserts candles for (symbol, timeframe), overwriting
// any existing row with the same (symbol, timeframe, open_time) — the
// primary key. This is what makes re-running a backfill over an already
// fetched range idempotent (SPEC.md storage-engineer kabul kriteri): the
// row count does not grow, only the OHLCV values are refreshed in place.
//
// All rows are written in a single transaction so a fetch page that dies
// halfway through never leaves a partially-written page in the DB.
func (s *Store) UpsertCandles(ctx context.Context, symbol, timeframe string, candles []domain.Candle) error {
	if len(candles) == 0 {
		return nil
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO candles (symbol, timeframe, open_time, open, high, low, close, volume, quote_volume)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(symbol, timeframe, open_time) DO UPDATE SET
				open = excluded.open,
				high = excluded.high,
				low = excluded.low,
				close = excluded.close,
				volume = excluded.volume,
				quote_volume = excluded.quote_volume
		`)
		if err != nil {
			return fmt.Errorf("prepare upsert candles: %w", err)
		}
		defer stmt.Close()

		for _, c := range candles {
			if _, err := stmt.ExecContext(ctx, symbol, timeframe, toUnixMs(c.OpenTime),
				c.Open, c.High, c.Low, c.Close, c.Volume, c.QuoteVolume); err != nil {
				return fmt.Errorf("upsert candle %s %s %s: %w", symbol, timeframe, c.OpenTime, err)
			}
		}
		return nil
	})
}

// MaxOpenTime returns the most recent open_time stored for (symbol,
// timeframe). ok is false if no candles exist yet. datafeed's incremental
// updater uses this to resume a backfill from the last known bar instead of
// refetching the whole history.
func (s *Store) MaxOpenTime(ctx context.Context, symbol, timeframe string) (openTime time.Time, ok bool, err error) {
	var max sql.NullInt64
	err = s.db.QueryRowContext(ctx, `
		SELECT MAX(open_time) FROM candles WHERE symbol = ? AND timeframe = ?
	`, symbol, timeframe).Scan(&max)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: max open_time %s %s: %w", symbol, timeframe, err)
	}
	if !max.Valid {
		return time.Time{}, false, nil
	}
	return fromUnixMs(max.Int64), true, nil
}

// GetCandles returns candles for (symbol, timeframe) with open_time in
// [from, to], ordered ascending by open_time. A zero to means "no upper
// bound".
func (s *Store) GetCandles(ctx context.Context, symbol, timeframe string, from, to time.Time) ([]domain.Candle, error) {
	query := `
		SELECT open_time, open, high, low, close, volume, quote_volume
		FROM candles WHERE symbol = ? AND timeframe = ? AND open_time >= ?
	`
	args := []any{symbol, timeframe, toUnixMs(from)}
	if !to.IsZero() {
		query += ` AND open_time <= ?`
		args = append(args, toUnixMs(to))
	}
	query += ` ORDER BY open_time ASC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: get candles %s %s: %w", symbol, timeframe, err)
	}
	defer rows.Close()

	var out []domain.Candle
	for rows.Next() {
		var c domain.Candle
		var openTime int64
		if err := rows.Scan(&openTime, &c.Open, &c.High, &c.Low, &c.Close, &c.Volume, &c.QuoteVolume); err != nil {
			return nil, fmt.Errorf("store: get candles %s %s: scan: %w", symbol, timeframe, err)
		}
		c.OpenTime = fromUnixMs(openTime)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: get candles %s %s: %w", symbol, timeframe, err)
	}
	return out, nil
}

// CountCandles returns the number of stored candles for (symbol,
// timeframe). Used by idempotency tests and data-quality checks.
func (s *Store) CountCandles(ctx context.Context, symbol, timeframe string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(1) FROM candles WHERE symbol = ? AND timeframe = ?
	`, symbol, timeframe).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count candles %s %s: %w", symbol, timeframe, err)
	}
	return n, nil
}
