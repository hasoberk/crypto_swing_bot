package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// --- orders ----------------------------------------------------------------

// Order is the store's row shape for the orders table.
type Order struct {
	ID            string
	ClientOrderID string // İ5 idempotency key
	ProposalID    string // empty if none
	Symbol        string
	Side          string
	Type          string // "market" | "limit"
	Qty           string // decimal string
	Price         string // decimal string, empty for market orders
	Status        string
	FilledQty     string // decimal string
	AvgPrice      string // decimal string, empty until filled
	Fee           string // decimal string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	RawJSON       string // empty if none
}

// InsertOrder inserts a new order row. client_order_id is UNIQUE, so a
// retried CreateOrder call after a network hiccup fails here instead of
// creating a second order for the same intent (SPEC.md İ5).
func (s *Store) InsertOrder(ctx context.Context, o Order) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO orders (id, client_order_id, proposal_id, symbol, side, type, qty, price,
			status, filled_qty, avg_price, fee, created_at, updated_at, raw_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, o.ID, o.ClientOrderID, nullIfEmpty(o.ProposalID), o.Symbol, o.Side, o.Type, o.Qty, nullIfEmpty(o.Price),
		o.Status, o.FilledQty, nullIfEmpty(o.AvgPrice), nullIfEmpty(o.Fee),
		toUnixMs(o.CreatedAt), toUnixMs(o.UpdatedAt), nullIfEmpty(o.RawJSON))
	if err != nil {
		return fmt.Errorf("store: insert order %s: %w", o.ID, err)
	}
	return nil
}

// UpdateOrderFill updates an order's fill state (status, filled_qty,
// avg_price, fee, raw exchange payload) after a poll or a fill webhook.
func (s *Store) UpdateOrderFill(ctx context.Context, id, status, filledQty, avgPrice, fee, rawJSON string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE orders SET status = ?, filled_qty = ?, avg_price = ?, fee = ?, raw_json = ?, updated_at = ?
		WHERE id = ?
	`, status, filledQty, nullIfEmpty(avgPrice), nullIfEmpty(fee), nullIfEmpty(rawJSON), toUnixMs(nowUTC()), id)
	if err != nil {
		return fmt.Errorf("store: update order fill %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: update order fill %s: rows affected: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("store: update order fill %s: not found", id)
	}
	return nil
}

// GetOrderByClientID looks up an order by its idempotency key. Callers use
// this before submitting to an exchange to detect "we already sent this"
// after a retry (SPEC.md İ5). ok is false if not found.
func (s *Store) GetOrderByClientID(ctx context.Context, clientOrderID string) (o Order, ok bool, err error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, client_order_id, proposal_id, symbol, side, type, qty, price,
			status, filled_qty, avg_price, fee, created_at, updated_at, raw_json
		FROM orders WHERE client_order_id = ?
	`, clientOrderID)
	o, err = scanOrder(row)
	if err == sql.ErrNoRows {
		return Order{}, false, nil
	}
	if err != nil {
		return Order{}, false, fmt.Errorf("store: get order by client id %s: %w", clientOrderID, err)
	}
	return o, true, nil
}

func scanOrder(row rowScanner) (Order, error) {
	var o Order
	var proposalID, price, avgPrice, fee, rawJSON sql.NullString
	var createdAt, updatedAt int64
	if err := row.Scan(&o.ID, &o.ClientOrderID, &proposalID, &o.Symbol, &o.Side, &o.Type, &o.Qty, &price,
		&o.Status, &o.FilledQty, &avgPrice, &fee, &createdAt, &updatedAt, &rawJSON); err != nil {
		return Order{}, err
	}
	o.ProposalID = proposalID.String
	o.Price = price.String
	o.AvgPrice = avgPrice.String
	o.Fee = fee.String
	o.RawJSON = rawJSON.String
	o.CreatedAt = fromUnixMs(createdAt)
	o.UpdatedAt = fromUnixMs(updatedAt)
	return o, nil
}

// --- trades ------------------------------------------------------------

// Trade is the store's row shape for the trades table: a closed (or still
// open, if ExitTime is zero) round-trip position.
type Trade struct {
	ID         string
	Symbol     string
	Strategy   string
	EntryTime  time.Time
	EntryPrice float64
	ExitTime   time.Time // zero if still open
	ExitPrice  *float64
	Qty        string // decimal string
	Fees       float64
	PnLQuote   *float64
	PnLPct     *float64
	ExitReason string // "stop"|"signal"|"manual"|"breaker", empty if still open
	Mode       string // "backtest"|"paper"|"live"
}

// InsertTrade records the opening of a round-trip trade (exit fields left
// unset until CloseTrade is called).
func (s *Store) InsertTrade(ctx context.Context, t Trade) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO trades (id, symbol, strategy, entry_time, entry_price, exit_time, exit_price,
			qty, fees, pnl_quote, pnl_pct, exit_reason, mode)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, t.ID, t.Symbol, t.Strategy, toUnixMs(t.EntryTime), t.EntryPrice, toUnixMsPtr(t.ExitTime), t.ExitPrice,
		t.Qty, t.Fees, t.PnLQuote, t.PnLPct, nullIfEmpty(t.ExitReason), t.Mode)
	if err != nil {
		return fmt.Errorf("store: insert trade %s: %w", t.ID, err)
	}
	return nil
}

// CloseTrade fills in a trade's exit fields.
func (s *Store) CloseTrade(ctx context.Context, id string, exitTime time.Time, exitPrice, pnlQuote, pnlPct, fees float64, exitReason string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE trades SET exit_time = ?, exit_price = ?, pnl_quote = ?, pnl_pct = ?, fees = ?, exit_reason = ?
		WHERE id = ?
	`, toUnixMs(exitTime), exitPrice, pnlQuote, pnlPct, fees, exitReason, id)
	if err != nil {
		return fmt.Errorf("store: close trade %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: close trade %s: rows affected: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("store: close trade %s: not found", id)
	}
	return nil
}

// ListTrades returns trades for mode (and, if symbol is non-empty, filtered
// to that symbol too), ordered by entry_time ascending. Used by backtest
// metrics and the panel's trades page.
func (s *Store) ListTrades(ctx context.Context, mode, symbol string) ([]Trade, error) {
	query := `
		SELECT id, symbol, strategy, entry_time, entry_price, exit_time, exit_price,
			qty, fees, pnl_quote, pnl_pct, exit_reason, mode
		FROM trades WHERE mode = ?
	`
	args := []any{mode}
	if symbol != "" {
		query += ` AND symbol = ?`
		args = append(args, symbol)
	}
	query += ` ORDER BY entry_time ASC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list trades: %w", err)
	}
	defer rows.Close()

	var out []Trade
	for rows.Next() {
		var t Trade
		var entryTime int64
		var exitTime sql.NullInt64
		var exitReason sql.NullString
		if err := rows.Scan(&t.ID, &t.Symbol, &t.Strategy, &entryTime, &t.EntryPrice, &exitTime, &t.ExitPrice,
			&t.Qty, &t.Fees, &t.PnLQuote, &t.PnLPct, &exitReason, &t.Mode); err != nil {
			return nil, fmt.Errorf("store: list trades: scan: %w", err)
		}
		t.EntryTime = fromUnixMs(entryTime)
		t.ExitTime = fromUnixMsPtr(exitTime)
		t.ExitReason = exitReason.String
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list trades: %w", err)
	}
	return out, nil
}

// --- equity_snapshots ----------------------------------------------------

// EquitySnapshot is the store's row shape for the equity_snapshots table: a
// daily portfolio-value point used by the panel's equity curve and by
// backtest metrics (drawdown, Sharpe, ...).
type EquitySnapshot struct {
	Mode       string
	TS         time.Time
	Equity     float64
	Cash       float64
	Exposure   float64 // 0..1
	BenchBTC   *float64
	BenchTop10 *float64
}

// InsertEquitySnapshot upserts a snapshot (PRIMARY KEY (mode, ts)): writing
// the same day twice replaces the row instead of duplicating it.
func (s *Store) InsertEquitySnapshot(ctx context.Context, e EquitySnapshot) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO equity_snapshots (mode, ts, equity, cash, exposure, bench_btc, bench_top10)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(mode, ts) DO UPDATE SET
			equity = excluded.equity,
			cash = excluded.cash,
			exposure = excluded.exposure,
			bench_btc = excluded.bench_btc,
			bench_top10 = excluded.bench_top10
	`, e.Mode, toUnixMs(e.TS), e.Equity, e.Cash, e.Exposure, e.BenchBTC, e.BenchTop10)
	if err != nil {
		return fmt.Errorf("store: insert equity snapshot %s %s: %w", e.Mode, e.TS, err)
	}
	return nil
}

// ListEquitySnapshots returns snapshots for mode ordered by ts ascending.
func (s *Store) ListEquitySnapshots(ctx context.Context, mode string) ([]EquitySnapshot, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT mode, ts, equity, cash, exposure, bench_btc, bench_top10
		FROM equity_snapshots WHERE mode = ? ORDER BY ts ASC
	`, mode)
	if err != nil {
		return nil, fmt.Errorf("store: list equity snapshots %s: %w", mode, err)
	}
	defer rows.Close()

	var out []EquitySnapshot
	for rows.Next() {
		var e EquitySnapshot
		var ts int64
		if err := rows.Scan(&e.Mode, &ts, &e.Equity, &e.Cash, &e.Exposure, &e.BenchBTC, &e.BenchTop10); err != nil {
			return nil, fmt.Errorf("store: list equity snapshots %s: scan: %w", mode, err)
		}
		e.TS = fromUnixMs(ts)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list equity snapshots %s: %w", mode, err)
	}
	return out, nil
}
