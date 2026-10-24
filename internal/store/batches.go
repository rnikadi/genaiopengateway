package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// BatchFile is a batch input or output file at an upstream, recorded
// so only its owner reaches it (docs/batch.md). Object is the file
// object the upstream returned.
type BatchFile struct {
	ID            string
	OrgID         string
	TeamID        string
	KeyID         string
	Model         string
	Provider      string
	UpstreamModel string
	Purpose       string
	Object        json.RawMessage
	CreatedAt     time.Time
}

const fileCols = `id, org_id, team_id, key_id, model, provider, upstream_model, purpose, object, created_at`

func scanBatchFile(row interface{ Scan(...any) error }) (*BatchFile, error) {
	var f BatchFile
	var object string
	var created int64
	if err := row.Scan(&f.ID, &f.OrgID, &f.TeamID, &f.KeyID, &f.Model, &f.Provider, &f.UpstreamModel, &f.Purpose, &object, &created); err != nil {
		return nil, err
	}
	f.Object, f.CreatedAt = json.RawMessage(object), toTime(created)
	return &f, nil
}

// CreateBatchFile records a file. A file id already recorded keeps its
// first owner.
func (s *Store) CreateBatchFile(ctx context.Context, f *BatchFile) error {
	_, err := s.db.ExecContext(ctx, s.q(`INSERT INTO batch_files (`+fileCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`),
		f.ID, f.OrgID, f.TeamID, f.KeyID, f.Model, f.Provider, f.UpstreamModel, f.Purpose, string(f.Object), now())
	return err
}

// GetBatchFile returns a file by id.
func (s *Store) GetBatchFile(ctx context.Context, id string) (*BatchFile, error) {
	f, err := scanBatchFile(s.db.QueryRowContext(ctx, s.q(`SELECT `+fileCols+` FROM batch_files WHERE id = ?`), id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return f, err
}

// DeleteBatchFile removes a file's record.
func (s *Store) DeleteBatchFile(ctx context.Context, id string) error {
	return affected(s.db.ExecContext(ctx, s.q(`DELETE FROM batch_files WHERE id = ?`), id))
}

// ListBatchFiles lists a team's files, newest first, or one key's when
// keyID is set.
func (s *Store) ListBatchFiles(ctx context.Context, orgID, teamID, keyID string, p Page) ([]*BatchFile, bool, error) {
	query := `SELECT ` + fileCols + ` FROM batch_files WHERE org_id = ? AND team_id = ? AND (? = '' OR key_id = ?)`
	args := []any{orgID, teamID, keyID, keyID}
	query, args, ok, err := s.after(ctx, "batch_files", query, args, p.After)
	if err != nil || !ok {
		return []*BatchFile{}, false, err
	}
	n := limit(p)
	rows, err := s.db.QueryContext(ctx, s.q(query+` ORDER BY created_at DESC, id DESC LIMIT ?`), append(args, n+1)...)
	if err != nil {
		return nil, false, err
	}
	return collect(rows, n, scanBatchFile)
}

// Batch is a batch created through the gateway. Object is the last batch
// object the upstream returned.
type Batch struct {
	ID            string          `json:"id"`
	OrgID         string          `json:"org_id"`
	TeamID        string          `json:"team_id"`
	KeyID         string          `json:"key_id"`
	Model         string          `json:"model"`
	Provider      string          `json:"provider"`
	UpstreamModel string          `json:"upstream_model"`
	InputFileID   string          `json:"input_file_id"`
	Status        string          `json:"status"`
	Object        json.RawMessage `json:"object"`
	CostUSD       float64         `json:"cost_usd"`
	ChargedAt     *time.Time      `json:"charged_at"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

// BatchDone reports whether a batch status is terminal.
func BatchDone(status string) bool {
	switch status {
	case "completed", "failed", "expired", "cancelled":
		return true
	}
	return false
}

const batchCols = `id, org_id, team_id, key_id, model, provider, upstream_model, input_file_id, status, object, cost_usd, charged_at, created_at, updated_at`

func scanBatch(row interface{ Scan(...any) error }) (*Batch, error) {
	var b Batch
	var object string
	var charged, created, updated int64
	if err := row.Scan(&b.ID, &b.OrgID, &b.TeamID, &b.KeyID, &b.Model, &b.Provider, &b.UpstreamModel, &b.InputFileID,
		&b.Status, &object, &b.CostUSD, &charged, &created, &updated); err != nil {
		return nil, err
	}
	b.Object, b.CreatedAt, b.UpdatedAt = json.RawMessage(object), toTime(created), toTime(updated)
	if charged != 0 {
		t := toTime(charged)
		b.ChargedAt = &t
	}
	return &b, nil
}

// CreateBatch records a new batch. A batch id already recorded keeps its
// first owner.
func (s *Store) CreateBatch(ctx context.Context, b *Batch) error {
	t := now()
	_, err := s.db.ExecContext(ctx, s.q(`INSERT INTO batches (id, org_id, team_id, key_id, model, provider, upstream_model, input_file_id,
		status, object, polled_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`),
		b.ID, b.OrgID, b.TeamID, b.KeyID, b.Model, b.Provider, b.UpstreamModel, b.InputFileID, b.Status, string(b.Object), t, t, t)
	return err
}

// GetBatch returns a batch by id.
func (s *Store) GetBatch(ctx context.Context, id string) (*Batch, error) {
	b, err := scanBatch(s.db.QueryRowContext(ctx, s.q(`SELECT `+batchCols+` FROM batches WHERE id = ?`), id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}

// UpdateBatch stores the latest batch object and its status.
func (s *Store) UpdateBatch(ctx context.Context, id, status string, object json.RawMessage) error {
	t := now()
	return affected(s.db.ExecContext(ctx, s.q(`UPDATE batches SET status = ?, object = ?, updated_at = ?, polled_at = ? WHERE id = ?`),
		status, string(object), t, t, id))
}

// ClaimBatchCharge marks a batch charged at cost and reports whether
// this call did, so a batch is charged once however many replicas see
// it finish.
func (s *Store) ClaimBatchCharge(ctx context.Context, id string, cost float64) (bool, error) {
	res, err := s.db.ExecContext(ctx, s.q(`UPDATE batches SET charged_at = ?, cost_usd = ? WHERE id = ? AND charged_at = 0`), now(), cost, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ClaimBatchesToPoll returns up to n unfinished batches last refreshed
// before cutoff, oldest first, and marks them refreshed now, so another
// replica does not poll them too.
func (s *Store) ClaimBatchesToPoll(ctx context.Context, cutoff time.Time, n int) ([]*Batch, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT id, polled_at FROM batches
		WHERE status NOT IN ('completed', 'failed', 'expired', 'cancelled') AND polled_at < ? ORDER BY polled_at LIMIT ?`),
		cutoff.UnixMilli(), n)
	if err != nil {
		return nil, err
	}
	type due struct {
		id     string
		polled int64
	}
	var all []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.polled); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []*Batch
	for _, d := range all {
		res, err := s.db.ExecContext(ctx, s.q(`UPDATE batches SET polled_at = ? WHERE id = ? AND polled_at = ?`), now(), d.id, d.polled)
		if err != nil {
			return out, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			continue // another replica claimed it
		}
		b, err := s.GetBatch(ctx, d.id)
		if err != nil {
			return out, err
		}
		out = append(out, b)
	}
	return out, nil
}

// BatchQuery selects batches. Every field is optional.
type BatchQuery struct {
	OrgID, TeamID, KeyID, Status string
}

// ListBatches lists batches, newest first.
func (s *Store) ListBatches(ctx context.Context, bq BatchQuery, p Page) ([]*Batch, bool, error) {
	query := `SELECT ` + batchCols + ` FROM batches WHERE (? = '' OR org_id = ?) AND (? = '' OR team_id = ?) AND (? = '' OR key_id = ?) AND (? = '' OR status = ?)`
	args := []any{bq.OrgID, bq.OrgID, bq.TeamID, bq.TeamID, bq.KeyID, bq.KeyID, bq.Status, bq.Status}
	query, args, ok, err := s.after(ctx, "batches", query, args, p.After)
	if err != nil || !ok {
		return []*Batch{}, false, err
	}
	n := limit(p)
	rows, err := s.db.QueryContext(ctx, s.q(query+` ORDER BY created_at DESC, id DESC LIMIT ?`), append(args, n+1)...)
	if err != nil {
		return nil, false, err
	}
	return collect(rows, n, scanBatch)
}

// after narrows a newest-first query on table to rows after the row with
// id after. It reports false when that row does not exist.
func (s *Store) after(ctx context.Context, table, query string, args []any, after string) (string, []any, bool, error) {
	if after == "" {
		return query, args, true, nil
	}
	var created int64
	err := s.db.QueryRowContext(ctx, s.q(`SELECT created_at FROM `+table+` WHERE id = ?`), after).Scan(&created)
	if errors.Is(err, sql.ErrNoRows) {
		return query, args, false, nil
	}
	if err != nil {
		return query, args, false, err
	}
	return query + ` AND (created_at < ? OR (created_at = ? AND id < ?))`, append(args, created, created, after), true, nil
}
