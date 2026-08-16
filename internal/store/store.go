// Package store is the SQLite persistence layer (SPEC.md Bölüm 4). It is the
// only package in the codebase that speaks SQL: everything else (datafeed,
// backtest, engine, web, ...) calls typed Go functions here and never sees a
// query string.
//
// modernc.org/sqlite is a pure-Go SQLite driver (no cgo), matching SPEC.md
// Bölüm 2's "Neden Parquet değil SQLite" rationale: single file, queryable,
// transactional, and Go-native so cross-compiling the swingbot binary stays
// a one-command affair.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Store wraps a *sql.DB opened against a swingbot SQLite file and provides
// typed CRUD across all tables in SPEC.md Bölüm 4.1.
type Store struct {
	db *sql.DB
}

// Open opens (creating if necessary) the SQLite database at path and runs
// all pending migrations. path may be ":memory:" for tests.
//
// A single connection is enforced (SetMaxOpenConns(1)): SQLite serializes
// writers regardless, and modernc.org/sqlite's driver-level locking is
// friendlier to reason about with one connection than with pool contention
// plus "database is locked" retries.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := path
	if path != ":memory:" {
		// _pragma params are applied by the driver on every new connection.
		dsn = fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: ping %s: %w", path, err)
	}

	if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: set WAL mode: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	return s, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// DB exposes the underlying *sql.DB for callers (e.g. panel health checks)
// that genuinely need it. Prefer the typed methods on Store instead.
func (s *Store) DB() *sql.DB {
	return s.db
}

// withTx runs fn inside a transaction, committing on success and rolling
// back on any error (including a panic, which is re-raised after rollback).
// Every multi-statement write in this package goes through here so a
// backfill page or a proposal decision can never be written half-done.
func (s *Store) withTx(ctx context.Context, fn func(tx *sql.Tx) error) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			tx.Rollback()
			panic(p)
		}
		if err != nil {
			tx.Rollback()
			return
		}
		err = tx.Commit()
	}()
	return fn(tx)
}

// --- time conversion -------------------------------------------------------
//
// SPEC.md Bölüm 4.2: all times are stored as UTC unix-milliseconds. This is
// the single place that conversion happens; callers pass/receive time.Time
// and never see an int64 timestamp.

func nowUTC() time.Time {
	return time.Now().UTC()
}

func toUnixMs(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromUnixMs(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// toUnixMsPtr/fromUnixMsPtr handle NULL-able timestamp columns (e.g.
// delisted_at, decided_at) where zero and "not set" must be distinguishable.

func toUnixMsPtr(t time.Time) *int64 {
	if t.IsZero() {
		return nil
	}
	ms := t.UnixMilli()
	return &ms
}

func fromUnixMsPtr(ms sql.NullInt64) time.Time {
	if !ms.Valid {
		return time.Time{}
	}
	return time.UnixMilli(ms.Int64).UTC()
}

// --- system_state ------------------------------------------------------

// SetState upserts a single key/value pair in system_state (e.g. circuit
// breaker status). value is caller-defined (often JSON); this layer does
// not interpret it.
func (s *Store) SetState(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO system_state (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
	`, key, value, toUnixMs(time.Now().UTC()))
	if err != nil {
		return fmt.Errorf("store: set state %s: %w", key, err)
	}
	return nil
}

// GetState reads a system_state value. ok is false if key is not set.
func (s *Store) GetState(ctx context.Context, key string) (value string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT value FROM system_state WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: get state %s: %w", key, err)
	}
	return value, true, nil
}

// --- markets -------------------------------------------------------------

// Market is the store's row shape for the markets table. It carries the
// same fields as domain.Market but keeps decimal/time values as strings and
// SQL-friendly nullable timestamps at the storage boundary; callers convert
// to/from domain.Market themselves (store does not import decimal to stay a
// minimal, swappable layer, matching İ1's package-boundary discipline).
type Market struct {
	Symbol      string
	Base        string
	Quote       string
	Active      bool
	TickSize    string
	StepSize    string
	MinNotional string
	ListedAt    time.Time // zero if unknown
	DelistedAt  time.Time // zero if active
	UpdatedAt   time.Time
}

// UpsertMarket inserts or updates a market row. Symbols are never deleted
// (SPEC.md Bölüm 6.1: delisting sets active=0 + delisted_at, it does not
// DELETE — deleting would introduce survivorship bias into historical
// universe reconstruction).
func (s *Store) UpsertMarket(ctx context.Context, m Market) error {
	active := 0
	if m.Active {
		active = 1
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO markets (symbol, base, quote, active, tick_size, step_size, min_notional, listed_at, delisted_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(symbol) DO UPDATE SET
			base = excluded.base,
			quote = excluded.quote,
			active = excluded.active,
			tick_size = excluded.tick_size,
			step_size = excluded.step_size,
			min_notional = excluded.min_notional,
			listed_at = excluded.listed_at,
			delisted_at = excluded.delisted_at,
			updated_at = excluded.updated_at
	`, m.Symbol, m.Base, m.Quote, active, m.TickSize, m.StepSize, m.MinNotional,
		toUnixMsPtr(m.ListedAt), toUnixMsPtr(m.DelistedAt), toUnixMs(m.UpdatedAt))
	if err != nil {
		return fmt.Errorf("store: upsert market %s: %w", m.Symbol, err)
	}
	return nil
}

// MarkDelisted sets active=0 and delisted_at=at for symbol, without
// deleting the row (see UpsertMarket doc comment).
func (s *Store) MarkDelisted(ctx context.Context, symbol string, at time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE markets SET active = 0, delisted_at = ?, updated_at = ? WHERE symbol = ?
	`, toUnixMs(at), toUnixMs(time.Now().UTC()), symbol)
	if err != nil {
		return fmt.Errorf("store: mark delisted %s: %w", symbol, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: mark delisted %s: rows affected: %w", symbol, err)
	}
	if n == 0 {
		return fmt.Errorf("store: mark delisted %s: not found", symbol)
	}
	return nil
}

// ListMarkets returns all markets, optionally filtered to active-only.
func (s *Store) ListMarkets(ctx context.Context, activeOnly bool) ([]Market, error) {
	query := `SELECT symbol, base, quote, active, tick_size, step_size, min_notional, listed_at, delisted_at, updated_at FROM markets`
	if activeOnly {
		query += ` WHERE active = 1`
	}
	query += ` ORDER BY symbol`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("store: list markets: %w", err)
	}
	defer rows.Close()

	var out []Market
	for rows.Next() {
		var m Market
		var active int
		var listedAt, delistedAt sql.NullInt64
		var updatedAt int64
		if err := rows.Scan(&m.Symbol, &m.Base, &m.Quote, &active, &m.TickSize, &m.StepSize, &m.MinNotional,
			&listedAt, &delistedAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("store: list markets: scan: %w", err)
		}
		m.Active = active != 0
		m.ListedAt = fromUnixMsPtr(listedAt)
		m.DelistedAt = fromUnixMsPtr(delistedAt)
		m.UpdatedAt = fromUnixMs(updatedAt)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list markets: %w", err)
	}
	return out, nil
}

// GetMarket returns a single market by symbol. ok is false if not found.
func (s *Store) GetMarket(ctx context.Context, symbol string) (m Market, ok bool, err error) {
	var active int
	var listedAt, delistedAt sql.NullInt64
	var updatedAt int64
	row := s.db.QueryRowContext(ctx, `
		SELECT symbol, base, quote, active, tick_size, step_size, min_notional, listed_at, delisted_at, updated_at
		FROM markets WHERE symbol = ?
	`, symbol)
	err = row.Scan(&m.Symbol, &m.Base, &m.Quote, &active, &m.TickSize, &m.StepSize, &m.MinNotional,
		&listedAt, &delistedAt, &updatedAt)
	if err == sql.ErrNoRows {
		return Market{}, false, nil
	}
	if err != nil {
		return Market{}, false, fmt.Errorf("store: get market %s: %w", symbol, err)
	}
	m.Active = active != 0
	m.ListedAt = fromUnixMsPtr(listedAt)
	m.DelistedAt = fromUnixMsPtr(delistedAt)
	m.UpdatedAt = fromUnixMs(updatedAt)
	return m, true, nil
}
