package store

import (
	"context"
	"database/sql"
	"fmt"
)

// migrations is an ordered list of forward-only schema steps. Each entry
// runs at most once (tracked in schema_migrations); new schema changes are
// appended here, never edited in place, so a database that has already
// applied migration N is never replayed against a changed N.
var migrations = []struct {
	name string
	sql  string
}{
	{
		name: "0001_initial_schema",
		sql: `
-- Semboller ve borsa filtreleri
CREATE TABLE IF NOT EXISTS markets (
    symbol        TEXT PRIMARY KEY,
    base          TEXT NOT NULL,
    quote         TEXT NOT NULL,
    active        INTEGER NOT NULL,
    tick_size     TEXT NOT NULL,
    step_size     TEXT NOT NULL,
    min_notional  TEXT NOT NULL,
    listed_at     INTEGER,
    delisted_at   INTEGER,
    updated_at    INTEGER NOT NULL
);

-- OHLCV
CREATE TABLE IF NOT EXISTS candles (
    symbol       TEXT    NOT NULL,
    timeframe    TEXT    NOT NULL,
    open_time    INTEGER NOT NULL,
    open         REAL    NOT NULL,
    high         REAL    NOT NULL,
    low          REAL    NOT NULL,
    close        REAL    NOT NULL,
    volume       REAL    NOT NULL,
    quote_volume REAL    NOT NULL,
    PRIMARY KEY (symbol, timeframe, open_time)
) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS idx_candles_time ON candles(timeframe, open_time);

-- Öneriler ve onay durumu
CREATE TABLE IF NOT EXISTS proposals (
    id            TEXT PRIMARY KEY,
    created_at    INTEGER NOT NULL,
    as_of         INTEGER NOT NULL,
    symbol        TEXT NOT NULL,
    side          TEXT NOT NULL,
    strategy      TEXT NOT NULL,
    score         REAL,
    ref_price     REAL NOT NULL,
    stop_price    REAL,
    qty           TEXT NOT NULL,
    risk_amount   REAL NOT NULL,
    reason        TEXT NOT NULL,
    metrics_json  TEXT NOT NULL,
    status        TEXT NOT NULL,
    expires_at    INTEGER NOT NULL,
    decided_at    INTEGER,
    order_id      TEXT
);

CREATE INDEX IF NOT EXISTS idx_proposals_status ON proposals(status, created_at);

-- Emirler
CREATE TABLE IF NOT EXISTS orders (
    id              TEXT PRIMARY KEY,
    client_order_id TEXT UNIQUE NOT NULL,
    proposal_id     TEXT,
    symbol          TEXT NOT NULL,
    side            TEXT NOT NULL,
    type            TEXT NOT NULL,
    qty             TEXT NOT NULL,
    price           TEXT,
    status          TEXT NOT NULL,
    filled_qty      TEXT NOT NULL,
    avg_price       TEXT,
    fee             TEXT,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    raw_json        TEXT
);

-- Kapanmış işlemler (round-trip)
CREATE TABLE IF NOT EXISTS trades (
    id           TEXT PRIMARY KEY,
    symbol       TEXT NOT NULL,
    strategy     TEXT NOT NULL,
    entry_time   INTEGER NOT NULL,
    entry_price  REAL NOT NULL,
    exit_time    INTEGER,
    exit_price   REAL,
    qty          TEXT NOT NULL,
    fees         REAL NOT NULL,
    pnl_quote    REAL,
    pnl_pct      REAL,
    exit_reason  TEXT,
    mode         TEXT NOT NULL
);

-- Günlük portföy anlık görüntüsü (panel ve metrikler için)
CREATE TABLE IF NOT EXISTS equity_snapshots (
    mode        TEXT    NOT NULL,
    ts          INTEGER NOT NULL,
    equity      REAL    NOT NULL,
    cash        REAL    NOT NULL,
    exposure    REAL    NOT NULL,
    bench_btc   REAL,
    bench_top10 REAL,
    PRIMARY KEY (mode, ts)
) WITHOUT ROWID;

-- Backtest koşuları
CREATE TABLE IF NOT EXISTS runs (
    id           TEXT PRIMARY KEY,
    created_at   INTEGER NOT NULL,
    strategy     TEXT NOT NULL,
    params_json  TEXT NOT NULL,
    start_ts     INTEGER NOT NULL,
    end_ts       INTEGER NOT NULL,
    costs_json   TEXT NOT NULL,
    metrics_json TEXT NOT NULL,
    report_path  TEXT,
    git_sha      TEXT
);

-- Sistem durumu (devre kesici vb.)
CREATE TABLE IF NOT EXISTS system_state (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);
`,
	},
}

// migrate creates the schema_migrations tracking table (if absent) and
// applies every migration in `migrations` not yet recorded there, each in
// its own transaction so a crash mid-migration leaves the DB at a known
// consistent version rather than half-applied.
func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name       TEXT PRIMARY KEY,
			applied_at INTEGER NOT NULL
		)
	`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	for _, m := range migrations {
		var applied int
		err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM schema_migrations WHERE name = ?`, m.name).Scan(&applied)
		if err != nil {
			return fmt.Errorf("check migration %s: %w", m.name, err)
		}
		if applied > 0 {
			continue
		}

		err = s.withTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, m.sql); err != nil {
				return fmt.Errorf("apply migration %s: %w", m.name, err)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`,
				m.name, toUnixMs(nowUTC())); err != nil {
				return fmt.Errorf("record migration %s: %w", m.name, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}
