package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/id"
)

// Statuses of an MCP access request.
const (
	RequestPending  = "pending"
	RequestApproved = "approved"
	RequestDenied   = "denied"
)

// MCPAccessRequest asks for a team's access to an MCP virtual server
// (docs/mcp-catalog.md).
type MCPAccessRequest struct {
	ID          string     `json:"id"`
	OrgID       string     `json:"org_id"`
	TeamID      string     `json:"team_id"`
	Server      string     `json:"server"`
	Reason      string     `json:"reason"`
	RequestedBy string     `json:"requested_by"`
	CreatedAt   time.Time  `json:"created_at"`
	Status      string     `json:"status"`
	DecidedBy   string     `json:"decided_by,omitempty"`
	DecidedAt   *time.Time `json:"decided_at,omitempty"`
	Note        string     `json:"note,omitempty"`
}

const mcpRequestCols = `id, org_id, team_id, server, reason, requested_by, created_at, status, decided_by, decided_at, note`

// CreateMCPAccessRequest stores a pending request. A team may have one
// pending request per server: another is ErrConflict.
func (s *Store) CreateMCPAccessRequest(ctx context.Context, r *MCPAccessRequest) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM mcp_access_requests WHERE team_id = ? AND server = ? AND status = ?`),
			r.TeamID, r.Server, RequestPending).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrConflict
		}
		r.ID = id.New("mcpreq")
		r.CreatedAt = toTime(now())
		r.Status = RequestPending
		_, err := tx.ExecContext(ctx, s.q(`INSERT INTO mcp_access_requests (`+mcpRequestCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', 0, '')`),
			r.ID, r.OrgID, r.TeamID, r.Server, r.Reason, r.RequestedBy, r.CreatedAt.UnixMilli(), r.Status)
		return err
	})
}

// GetMCPAccessRequest returns one request.
func (s *Store) GetMCPAccessRequest(ctx context.Context, requestID string) (*MCPAccessRequest, error) {
	r, err := scanMCPRequest(s.db.QueryRowContext(ctx, s.q(`SELECT `+mcpRequestCols+` FROM mcp_access_requests WHERE id = ?`), requestID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// MCPRequestFilter selects requests; empty fields match all.
type MCPRequestFilter struct {
	OrgIDs  []string
	TeamIDs []string
	Status  string
	Limit   int
}

// ListMCPAccessRequests returns requests, newest first. With OrgIDs or
// TeamIDs set, a request matches when it is in any of the organizations
// or any of the teams.
func (s *Store) ListMCPAccessRequests(ctx context.Context, f MCPRequestFilter) ([]*MCPAccessRequest, error) {
	q := `SELECT ` + mcpRequestCols + ` FROM mcp_access_requests WHERE 1 = 1`
	var args []any
	if f.OrgIDs != nil || f.TeamIDs != nil {
		q += ` AND (1 = 0`
		for _, o := range f.OrgIDs {
			q += ` OR org_id = ?`
			args = append(args, o)
		}
		for _, t := range f.TeamIDs {
			q += ` OR team_id = ?`
			args = append(args, t)
		}
		q += `)`
	}
	if f.Status != "" {
		q += ` AND status = ?`
		args = append(args, f.Status)
	}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 500
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, f.Limit)
	rows, err := s.db.QueryContext(ctx, s.q(q), args...)
	if err != nil {
		return nil, err
	}
	out, _, err := collect(rows, -1, scanMCPRequest)
	return out, err
}

// DecideMCPAccessRequest approves or denies a pending request. One that
// is no longer pending is ErrConflict.
func (s *Store) DecideMCPAccessRequest(ctx context.Context, requestID, status, by, note string) (*MCPAccessRequest, error) {
	at := now()
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.q(`UPDATE mcp_access_requests SET status = ?, decided_by = ?, decided_at = ?, note = ? WHERE id = ? AND status = ?`),
			status, by, at, note, requestID, RequestPending)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			var exists int
			if err := tx.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM mcp_access_requests WHERE id = ?`), requestID).Scan(&exists); err != nil {
				return err
			}
			if exists == 0 {
				return ErrNotFound
			}
			return ErrConflict
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetMCPAccessRequest(ctx, requestID)
}

func scanMCPRequest(row interface{ Scan(...any) error }) (*MCPAccessRequest, error) {
	var r MCPAccessRequest
	var created, decided int64
	if err := row.Scan(&r.ID, &r.OrgID, &r.TeamID, &r.Server, &r.Reason, &r.RequestedBy, &created, &r.Status, &r.DecidedBy, &decided, &r.Note); err != nil {
		return nil, err
	}
	r.CreatedAt = toTime(created)
	if decided > 0 {
		t := toTime(decided)
		r.DecidedAt = &t
	}
	return &r, nil
}
