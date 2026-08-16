package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ProposalStatus mirrors the state machine in SPEC.md storage-engineer
// kabul kriteri: PENDING→APPROVED/REJECTED/EXPIRED→SUBMITTED→FILLED/FAILED.
// Store does not enforce transitions between these — that belongs to the
// notify/approval layer — it only persists whatever status the caller sets.
type ProposalStatus string

const (
	ProposalPending   ProposalStatus = "PENDING"
	ProposalApproved  ProposalStatus = "APPROVED"
	ProposalRejected  ProposalStatus = "REJECTED"
	ProposalExpired   ProposalStatus = "EXPIRED"
	ProposalSubmitted ProposalStatus = "SUBMITTED"
	ProposalFilled    ProposalStatus = "FILLED"
	ProposalFailed    ProposalStatus = "FAILED"
)

// Proposal is the store's row shape for the proposals table: a strategy
// Signal that has been sized and is awaiting (or has received) a Telegram
// approval decision.
type Proposal struct {
	ID          string
	CreatedAt   time.Time
	AsOf        time.Time
	Symbol      string
	Side        string // "long" | "exit"
	Strategy    string
	Score       *float64
	RefPrice    float64
	StopPrice   *float64
	Qty         string // decimal string
	RiskAmount  float64
	Reason      string
	MetricsJSON string
	Status      ProposalStatus
	ExpiresAt   time.Time
	DecidedAt   time.Time // zero if not yet decided
	OrderID     string    // empty if no order submitted yet
}

// InsertProposal inserts a new proposal row. IDs are caller-generated
// (ULID/UUID per SPEC.md Bölüm 4.1).
func (s *Store) InsertProposal(ctx context.Context, p Proposal) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO proposals (id, created_at, as_of, symbol, side, strategy, score, ref_price, stop_price,
			qty, risk_amount, reason, metrics_json, status, expires_at, decided_at, order_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, p.ID, toUnixMs(p.CreatedAt), toUnixMs(p.AsOf), p.Symbol, p.Side, p.Strategy, p.Score, p.RefPrice, p.StopPrice,
		p.Qty, p.RiskAmount, p.Reason, p.MetricsJSON, string(p.Status), toUnixMs(p.ExpiresAt),
		toUnixMsPtr(p.DecidedAt), nullIfEmpty(p.OrderID))
	if err != nil {
		return fmt.Errorf("store: insert proposal %s: %w", p.ID, err)
	}
	return nil
}

// UpdateProposalStatus transitions a proposal to status, recording
// decidedAt and, once an order is placed, orderID. Pass a zero decidedAt to
// leave the column untouched (e.g. an EXPIRED sweep that predates any
// decision).
func (s *Store) UpdateProposalStatus(ctx context.Context, id string, status ProposalStatus, decidedAt time.Time, orderID string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE proposals SET status = ?, decided_at = COALESCE(?, decided_at), order_id = COALESCE(?, order_id)
		WHERE id = ?
	`, string(status), toUnixMsPtr(decidedAt), nullIfEmpty(orderID), id)
	if err != nil {
		return fmt.Errorf("store: update proposal status %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: update proposal status %s: rows affected: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("store: update proposal status %s: not found", id)
	}
	return nil
}

// GetProposal returns a single proposal by ID. ok is false if not found.
func (s *Store) GetProposal(ctx context.Context, id string) (p Proposal, ok bool, err error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, created_at, as_of, symbol, side, strategy, score, ref_price, stop_price,
			qty, risk_amount, reason, metrics_json, status, expires_at, decided_at, order_id
		FROM proposals WHERE id = ?
	`, id)
	p, err = scanProposal(row)
	if err == sql.ErrNoRows {
		return Proposal{}, false, nil
	}
	if err != nil {
		return Proposal{}, false, fmt.Errorf("store: get proposal %s: %w", id, err)
	}
	return p, true, nil
}

// ListProposalsByStatus returns proposals with the given status, most
// recently created first. Used by the approval-expiry sweep and the panel's
// pending-proposals view.
func (s *Store) ListProposalsByStatus(ctx context.Context, status ProposalStatus) ([]Proposal, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, created_at, as_of, symbol, side, strategy, score, ref_price, stop_price,
			qty, risk_amount, reason, metrics_json, status, expires_at, decided_at, order_id
		FROM proposals WHERE status = ? ORDER BY created_at DESC
	`, string(status))
	if err != nil {
		return nil, fmt.Errorf("store: list proposals by status %s: %w", status, err)
	}
	defer rows.Close()

	var out []Proposal
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list proposals by status %s: scan: %w", status, err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list proposals by status %s: %w", status, err)
	}
	return out, nil
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows, letting
// scanProposal serve GetProposal and the List* functions alike.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanProposal(row rowScanner) (Proposal, error) {
	var p Proposal
	var createdAt, asOf, expiresAt int64
	var status string
	var decidedAt sql.NullInt64
	var orderID sql.NullString
	if err := row.Scan(&p.ID, &createdAt, &asOf, &p.Symbol, &p.Side, &p.Strategy, &p.Score, &p.RefPrice, &p.StopPrice,
		&p.Qty, &p.RiskAmount, &p.Reason, &p.MetricsJSON, &status, &expiresAt, &decidedAt, &orderID); err != nil {
		return Proposal{}, err
	}
	p.CreatedAt = fromUnixMs(createdAt)
	p.AsOf = fromUnixMs(asOf)
	p.ExpiresAt = fromUnixMs(expiresAt)
	p.Status = ProposalStatus(status)
	p.DecidedAt = fromUnixMsPtr(decidedAt)
	if orderID.Valid {
		p.OrderID = orderID.String
	}
	return p, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
