package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// Denial is a model request the gateway refused, with the reason it gave
// (docs/why-denied.md).
type Denial struct {
	RequestID string    `json:"request_id"`
	Time      time.Time `json:"ts"`
	OrgID     string    `json:"org_id"`
	TeamID    string    `json:"team_id"`
	KeyID     string    `json:"key_id"`
	Endpoint  string    `json:"endpoint"`
	Model     string    `json:"model"`
	Status    int       `json:"status"`
	Code      string    `json:"code"`
	Reason    string    `json:"reason"`
}

// InsertDenials writes a batch of denials. A request id seen before is
// skipped, so a retried batch does no harm.
func (s *Store) InsertDenials(ctx context.Context, ds []Denial) error {
	if len(ds) == 0 {
		return nil
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, s.q(`INSERT INTO denials (request_id, ts, org_id, team_id, key_id, endpoint, model, status, code, reason)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (request_id) DO NOTHING`))
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, d := range ds {
			if _, err := stmt.ExecContext(ctx, d.RequestID, d.Time.UnixMilli(), d.OrgID, d.TeamID, d.KeyID, d.Endpoint, d.Model, d.Status, d.Code, d.Reason); err != nil {
				return err
			}
		}
		return nil
	})
}

// DenialQuery filters denials. Zero values mean no filter.
type DenialQuery struct {
	OrgID, TeamID, KeyID, Code string
	From, To                   time.Time
}

const denialCols = `request_id, ts, org_id, team_id, key_id, endpoint, model, status, code, reason`

func scanDenial(row interface{ Scan(...any) error }) (*Denial, error) {
	var d Denial
	var ts int64
	if err := row.Scan(&d.RequestID, &ts, &d.OrgID, &d.TeamID, &d.KeyID, &d.Endpoint, &d.Model, &d.Status, &d.Code, &d.Reason); err != nil {
		return nil, err
	}
	d.Time = toTime(ts)
	return &d, nil
}

// ListDenials returns denials newest first; After is a request id.
func (s *Store) ListDenials(ctx context.Context, q DenialQuery, p Page) ([]*Denial, bool, error) {
	var where []string
	var args []any
	for _, f := range []struct{ col, v string }{{"org_id", q.OrgID}, {"team_id", q.TeamID}, {"key_id", q.KeyID}, {"code", q.Code}} {
		if f.v != "" {
			where, args = append(where, f.col+" = ?"), append(args, f.v)
		}
	}
	if !q.From.IsZero() {
		where, args = append(where, "ts >= ?"), append(args, q.From.UnixMilli())
	}
	if !q.To.IsZero() {
		where, args = append(where, "ts < ?"), append(args, q.To.UnixMilli())
	}
	if p.After != "" {
		where = append(where, "(ts < (SELECT ts FROM denials WHERE request_id = ?) OR (ts = (SELECT ts FROM denials WHERE request_id = ?) AND request_id < ?))")
		args = append(args, p.After, p.After, p.After)
	}
	query := `SELECT ` + denialCols + ` FROM denials`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, " AND ")
	}
	n := limit(p)
	rows, err := s.db.QueryContext(ctx, s.q(query+` ORDER BY ts DESC, request_id DESC LIMIT ?`), append(args, n+1)...)
	if err != nil {
		return nil, false, err
	}
	return collect(rows, n, scanDenial)
}

// GetDenial returns the denial of a request, or ErrNotFound.
func (s *Store) GetDenial(ctx context.Context, requestID string) (*Denial, error) {
	d, err := scanDenial(s.db.QueryRowContext(ctx, s.q(`SELECT `+denialCols+` FROM denials WHERE request_id = ?`), requestID))
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return d, err
}

// PruneDenials deletes denials older than a time.
func (s *Store) PruneDenials(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM denials WHERE ts < ?`), before.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
