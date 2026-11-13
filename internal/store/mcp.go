package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// MCPToolPin is the approval state of one upstream MCP tool.
type MCPToolPin struct {
	Server string `json:"server"`
	Tool   string `json:"tool"`
	// Hash is of the definition last seen; ApprovedHash of the one
	// approved, empty if none was.
	Hash         string          `json:"hash"`
	ApprovedHash string          `json:"approved_hash"`
	Definition   json.RawMessage `json:"definition"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

// Pin statuses.
const (
	PinApproved = "approved"
	PinPending  = "pending"
	PinChanged  = "changed"
)

// Status says whether the definition last seen is approved.
func (p *MCPToolPin) Status() string {
	switch p.ApprovedHash {
	case p.Hash:
		return PinApproved
	case "":
		return PinPending
	}
	return PinChanged
}

// ListPins returns the pins of one server, or of all servers when server
// is empty, ordered by server and tool.
func (s *Store) ListPins(ctx context.Context, server string) ([]*MCPToolPin, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT server, tool, hash, approved_hash, definition, updated_at FROM mcp_tool_pins
		WHERE (? = '' OR server = ?) ORDER BY server, tool`), server, server)
	if err != nil {
		return nil, err
	}
	pins, _, err := collect(rows, -1, func(row interface{ Scan(...any) error }) (*MCPToolPin, error) {
		var p MCPToolPin
		var def string
		var ts int64
		if err := row.Scan(&p.Server, &p.Tool, &p.Hash, &p.ApprovedHash, &def, &ts); err != nil {
			return nil, err
		}
		p.Definition, p.UpdatedAt = json.RawMessage(def), toTime(ts)
		return &p, nil
	})
	return pins, err
}

// InsertPin records a tool seen for the first time. A pin another
// replica inserted first wins.
func (s *Store) InsertPin(ctx context.Context, p *MCPToolPin) error {
	_, err := s.db.ExecContext(ctx, s.q(`INSERT INTO mcp_tool_pins (server, tool, hash, approved_hash, definition, updated_at)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (server, tool) DO NOTHING`),
		p.Server, p.Tool, p.Hash, p.ApprovedHash, string(p.Definition), now())
	return err
}

// UpdatePinHash records a changed definition, leaving the approval as is.
func (s *Store) UpdatePinHash(ctx context.Context, p *MCPToolPin) error {
	_, err := s.db.ExecContext(ctx, s.q(`UPDATE mcp_tool_pins SET hash = ?, definition = ?, updated_at = ? WHERE server = ? AND tool = ?`),
		p.Hash, string(p.Definition), now(), p.Server, p.Tool)
	return err
}

// GetPin returns one pin.
func (s *Store) GetPin(ctx context.Context, server, tool string) (*MCPToolPin, error) {
	pins, err := s.ListPins(ctx, server)
	if err != nil {
		return nil, err
	}
	for _, p := range pins {
		if p.Tool == tool {
			return p, nil
		}
	}
	return nil, ErrNotFound
}

// ApprovePin approves the definition with the given hash. It fails with
// ErrConflict when that is no longer the definition last seen, so an
// admin approves only what they reviewed.
func (s *Store) ApprovePin(ctx context.Context, server, tool, hash string) (*MCPToolPin, error) {
	res, err := s.db.ExecContext(ctx, s.q(`UPDATE mcp_tool_pins SET approved_hash = hash, updated_at = ? WHERE server = ? AND tool = ? AND hash = ?`),
		now(), server, tool, hash)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	p, err := s.GetPin(ctx, server, tool)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, fmt.Errorf("%w: the definition of %s/%s changed since it was shown; its hash is now %s", ErrConflict, server, tool, p.Hash)
	}
	return p, nil
}

// MCPCall is the audit record of one MCP call.
type MCPCall struct {
	ID            string    `json:"id"`
	Time          time.Time `json:"ts"`
	OrgID         string    `json:"org_id"`
	TeamID        string    `json:"team_id"`
	KeyID         string    `json:"key_id"`
	Subject       string    `json:"subject,omitempty"`
	VirtualServer string    `json:"virtual_server"`
	Server        string    `json:"server"`
	Method        string    `json:"method"`
	Name          string    `json:"name"`
	Status        string    `json:"status"`
	ErrorCode     int64     `json:"error_code,omitempty"`
	LatencyMS     int64     `json:"latency_ms"`
	RequestID     string    `json:"request_id"`
	// HasBody is set by ListMCPCalls when the call's body is logged.
	HasBody bool `json:"has_body"`
}

// InsertMCPCalls writes a batch of call records.
func (s *Store) InsertMCPCalls(ctx context.Context, calls []MCPCall) error {
	if len(calls) == 0 {
		return nil
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, s.q(`INSERT INTO mcp_calls (id, ts, org_id, team_id, key_id, subject, virtual_server, server, method, name, status, error_code, latency_ms, request_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`))
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, c := range calls {
			if _, err := stmt.ExecContext(ctx, c.ID, c.Time.UnixMilli(), c.OrgID, c.TeamID, c.KeyID, c.Subject, c.VirtualServer,
				c.Server, c.Method, c.Name, c.Status, c.ErrorCode, c.LatencyMS, c.RequestID); err != nil {
				return err
			}
		}
		return nil
	})
}

// MCPCallQuery filters call records. Zero values mean no filter.
type MCPCallQuery struct {
	OrgID, TeamID, KeyID, Server string
	From, To                     time.Time
}

// ListMCPCalls returns call records newest first; After is a call id.
func (s *Store) ListMCPCalls(ctx context.Context, cq MCPCallQuery, p Page) ([]*MCPCall, bool, error) {
	var where []string
	var args []any
	for col, v := range map[string]string{"org_id": cq.OrgID, "team_id": cq.TeamID, "key_id": cq.KeyID, "server": cq.Server} {
		if v != "" {
			where = append(where, col+" = ?")
			args = append(args, v)
		}
	}
	if !cq.From.IsZero() {
		where, args = append(where, "ts >= ?"), append(args, cq.From.UnixMilli())
	}
	if !cq.To.IsZero() {
		where, args = append(where, "ts < ?"), append(args, cq.To.UnixMilli())
	}
	if p.After != "" {
		where, args = append(where, "id < ?"), append(args, p.After)
	}
	query := `SELECT id, ts, org_id, team_id, key_id, subject, virtual_server, server, method, name, status, error_code, latency_ms, request_id,
		EXISTS (SELECT 1 FROM request_bodies b WHERE b.request_id = mcp_calls.request_id AND b.org_id = mcp_calls.org_id AND b.kind = 'mcp') FROM mcp_calls`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, " AND ")
	}
	n := limit(p)
	rows, err := s.db.QueryContext(ctx, s.q(query+` ORDER BY id DESC LIMIT ?`), append(args, n+1)...)
	if err != nil {
		return nil, false, err
	}
	return collect(rows, n, func(row interface{ Scan(...any) error }) (*MCPCall, error) {
		var c MCPCall
		var ts int64
		if err := row.Scan(&c.ID, &ts, &c.OrgID, &c.TeamID, &c.KeyID, &c.Subject, &c.VirtualServer, &c.Server, &c.Method,
			&c.Name, &c.Status, &c.ErrorCode, &c.LatencyMS, &c.RequestID, &c.HasBody); err != nil {
			return nil, err
		}
		c.Time = toTime(ts)
		return &c, nil
	})
}

// PruneMCPCalls deletes call records older than a time.
func (s *Store) PruneMCPCalls(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM mcp_calls WHERE ts < ?`), before.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
