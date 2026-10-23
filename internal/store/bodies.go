package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/id"
)

// RequestBody is the logged content of one request (docs/body-logging.md).
// Request and Response are redacted, then sealed by the caller; the store
// never sees plaintext.
type RequestBody struct {
	ID         string
	RequestID  string
	Time       time.Time
	OrgID      string
	TeamID     string
	KeyID      string
	Kind       string // "model" or "mcp"
	Endpoint   string
	Request    string
	Response   string
	Truncated  bool
	Redactions json.RawMessage
}

// InsertRequestBodies writes a batch of body records.
func (s *Store) InsertRequestBodies(ctx context.Context, bodies []*RequestBody) error {
	if len(bodies) == 0 {
		return nil
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, s.q(`INSERT INTO request_bodies (id, request_id, ts, org_id, team_id, key_id, kind, endpoint, request, response, truncated, redactions)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`))
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, b := range bodies {
			if b.ID == "" {
				b.ID = id.New("body")
			}
			if b.Redactions == nil {
				b.Redactions = json.RawMessage("{}")
			}
			truncated := 0
			if b.Truncated {
				truncated = 1
			}
			if _, err := stmt.ExecContext(ctx, b.ID, b.RequestID, b.Time.UnixMilli(), b.OrgID, b.TeamID, b.KeyID, b.Kind, b.Endpoint,
				b.Request, b.Response, truncated, string(b.Redactions)); err != nil {
				return err
			}
		}
		return nil
	})
}

// RequestBodies returns the body records of a request id, newest first.
// Clients may choose request ids, so one id can name requests of several
// organizations; callers pick the record they may read.
func (s *Store) RequestBodies(ctx context.Context, requestID string) ([]*RequestBody, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT id, request_id, ts, org_id, team_id, key_id, kind, endpoint, request, response, truncated, redactions
		FROM request_bodies WHERE request_id = ? ORDER BY ts DESC LIMIT 20`), requestID)
	if err != nil {
		return nil, err
	}
	out, _, err := collect(rows, -1, func(row interface{ Scan(...any) error }) (*RequestBody, error) {
		var b RequestBody
		var ts int64
		var truncated int
		var redactions string
		if err := row.Scan(&b.ID, &b.RequestID, &ts, &b.OrgID, &b.TeamID, &b.KeyID, &b.Kind, &b.Endpoint, &b.Request, &b.Response, &truncated, &redactions); err != nil {
			return nil, err
		}
		b.Time, b.Truncated, b.Redactions = toTime(ts), truncated != 0, json.RawMessage(redactions)
		return &b, nil
	})
	return out, err
}

// DeleteRequestBodies deletes the body records of an organization older
// than a time, for erasure requests.
func (s *Store) DeleteRequestBodies(ctx context.Context, orgID string, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM request_bodies WHERE org_id = ? AND ts < ?`), orgID, before.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// pruneBatch bounds each delete, so pruning never holds long locks.
const pruneBatch = 1000

// PruneRequestBodies deletes body records older than a time, in batches.
func (s *Store) PruneRequestBodies(ctx context.Context, before time.Time) (int64, error) {
	var total int64
	for {
		res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM request_bodies WHERE id IN
			(SELECT id FROM request_bodies WHERE ts < ? LIMIT ?)`), before.UnixMilli(), pruneBatch)
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		total += n
		if err != nil || n < pruneBatch {
			return total, err
		}
	}
}

// RequestQuery filters the request list. Zero values mean no filter.
type RequestQuery struct {
	OrgID, TeamID, KeyID string
	From, To             time.Time
}

// RequestRow is one model request from the usage records.
type RequestRow struct {
	RequestID     string    `json:"request_id"`
	Time          time.Time `json:"ts"`
	OrgID         string    `json:"org_id"`
	TeamID        string    `json:"team_id"`
	KeyID         string    `json:"key_id"`
	Endpoint      string    `json:"endpoint"`
	Model         string    `json:"model"`
	Provider      string    `json:"provider"`
	UpstreamModel string    `json:"upstream_model"`
	Status        int       `json:"status"`
	LatencyMS     int64     `json:"latency_ms"`
	InputTokens   int64     `json:"input_tokens"`
	OutputTokens  int64     `json:"output_tokens"`
	CostUSD       float64   `json:"cost_usd"`
	HasBody       bool      `json:"has_body"`
}

// ListRequests returns model requests newest first; After is a request id.
func (s *Store) ListRequests(ctx context.Context, rq RequestQuery, p Page) ([]*RequestRow, bool, error) {
	var where []string
	var args []any
	for col, v := range map[string]string{"u.org_id": rq.OrgID, "u.team_id": rq.TeamID, "u.key_id": rq.KeyID} {
		if v != "" {
			where, args = append(where, col+" = ?"), append(args, v)
		}
	}
	if !rq.From.IsZero() {
		where, args = append(where, "u.ts >= ?"), append(args, rq.From.UnixMilli())
	}
	if !rq.To.IsZero() {
		where, args = append(where, "u.ts < ?"), append(args, rq.To.UnixMilli())
	}
	if p.After != "" {
		// After is a request id: continue below its (ts, request_id).
		where = append(where, "(u.ts < (SELECT MAX(ts) FROM usage WHERE request_id = ?) OR (u.ts = (SELECT MAX(ts) FROM usage WHERE request_id = ?) AND u.request_id < ?))")
		args = append(args, p.After, p.After, p.After)
	}
	query := `SELECT u.request_id, u.ts, u.org_id, u.team_id, u.key_id, u.endpoint, u.model, u.provider, u.upstream_model,
		u.status, u.latency_ms, u.input_tokens, u.output_tokens, u.cost_usd,
		EXISTS (SELECT 1 FROM request_bodies b WHERE b.request_id = u.request_id AND b.org_id = u.org_id)
		FROM usage u`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, " AND ")
	}
	n := limit(p)
	rows, err := s.db.QueryContext(ctx, s.q(query+` ORDER BY u.ts DESC, u.request_id DESC LIMIT ?`), append(args, n+1)...)
	if err != nil {
		return nil, false, err
	}
	return collect(rows, n, func(row interface{ Scan(...any) error }) (*RequestRow, error) {
		var r RequestRow
		var ts int64
		if err := row.Scan(&r.RequestID, &ts, &r.OrgID, &r.TeamID, &r.KeyID, &r.Endpoint, &r.Model, &r.Provider, &r.UpstreamModel,
			&r.Status, &r.LatencyMS, &r.InputTokens, &r.OutputTokens, &r.CostUSD, &r.HasBody); err != nil {
			return nil, err
		}
		r.Time = toTime(ts)
		return &r, nil
	})
}
