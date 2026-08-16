package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Run is the store's row shape for the runs table: one backtest execution,
// its parameters, cost assumptions, and resulting metrics — kept for
// reproducibility (git_sha pins the code version that produced it).
type Run struct {
	ID          string
	CreatedAt   time.Time
	Strategy    string
	ParamsJSON  string
	StartTS     time.Time
	EndTS       time.Time
	CostsJSON   string
	MetricsJSON string
	ReportPath  string // empty if no HTML report was generated
	GitSHA      string // empty if unknown
}

// InsertRun records a completed backtest run.
func (s *Store) InsertRun(ctx context.Context, r Run) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (id, created_at, strategy, params_json, start_ts, end_ts, costs_json, metrics_json, report_path, git_sha)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, r.ID, toUnixMs(r.CreatedAt), r.Strategy, r.ParamsJSON, toUnixMs(r.StartTS), toUnixMs(r.EndTS),
		r.CostsJSON, r.MetricsJSON, nullIfEmpty(r.ReportPath), nullIfEmpty(r.GitSHA))
	if err != nil {
		return fmt.Errorf("store: insert run %s: %w", r.ID, err)
	}
	return nil
}

// GetRun returns a single run by ID. ok is false if not found.
func (s *Store) GetRun(ctx context.Context, id string) (r Run, ok bool, err error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, created_at, strategy, params_json, start_ts, end_ts, costs_json, metrics_json, report_path, git_sha
		FROM runs WHERE id = ?
	`, id)
	r, err = scanRun(row)
	if err == sql.ErrNoRows {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, fmt.Errorf("store: get run %s: %w", id, err)
	}
	return r, true, nil
}

// ListRuns returns all runs, most recently created first. Used by the
// panel's runs page.
func (s *Store) ListRuns(ctx context.Context) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, created_at, strategy, params_json, start_ts, end_ts, costs_json, metrics_json, report_path, git_sha
		FROM runs ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("store: list runs: %w", err)
	}
	defer rows.Close()

	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list runs: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list runs: %w", err)
	}
	return out, nil
}

func scanRun(row rowScanner) (Run, error) {
	var r Run
	var createdAt, startTS, endTS int64
	var reportPath, gitSHA sql.NullString
	if err := row.Scan(&r.ID, &createdAt, &r.Strategy, &r.ParamsJSON, &startTS, &endTS,
		&r.CostsJSON, &r.MetricsJSON, &reportPath, &gitSHA); err != nil {
		return Run{}, err
	}
	r.CreatedAt = fromUnixMs(createdAt)
	r.StartTS = fromUnixMs(startTS)
	r.EndTS = fromUnixMs(endTS)
	r.ReportPath = reportPath.String
	r.GitSHA = gitSHA.String
	return r, nil
}
