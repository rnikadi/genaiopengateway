package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// MCPConnection is a user's connection to an upstream MCP server
// (docs/mcp-user-credentials.md). The tokens are sealed by the caller.
type MCPConnection struct {
	Server       string     `json:"server"`
	Subject      string     `json:"subject"`
	AccessToken  string     `json:"-"`
	RefreshToken string     `json:"-"`
	Scopes       []string   `json:"scopes"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	LastUsedAt   time.Time  `json:"last_used_at"`
}

const connCols = `server, subject, access_token, refresh_token, scopes, expires_at, created_at, updated_at, last_used_at`

func scanConn(row interface{ Scan(...any) error }) (*MCPConnection, error) {
	var c MCPConnection
	var scopes string
	var expires sql.NullInt64
	var created, updated, used int64
	if err := row.Scan(&c.Server, &c.Subject, &c.AccessToken, &c.RefreshToken, &scopes, &expires, &created, &updated, &used); err != nil {
		return nil, err
	}
	c.Scopes = strings.Fields(scopes)
	c.ExpiresAt, c.CreatedAt, c.UpdatedAt, c.LastUsedAt = toTimePtr(expires), toTime(created), toTime(updated), toTime(used)
	return &c, nil
}

// PutMCPConnection creates or replaces a connection's tokens, keeping
// its creation time.
func (s *Store) PutMCPConnection(ctx context.Context, c *MCPConnection) error {
	t := now()
	_, err := s.db.ExecContext(ctx, s.q(`INSERT INTO mcp_connections (`+connCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (server, subject) DO UPDATE SET access_token = excluded.access_token, refresh_token = excluded.refresh_token,
			scopes = excluded.scopes, expires_at = excluded.expires_at, updated_at = excluded.updated_at, last_used_at = excluded.last_used_at`),
		c.Server, c.Subject, c.AccessToken, c.RefreshToken, strings.Join(c.Scopes, " "), fromTimePtr(c.ExpiresAt), t, t, t)
	return err
}

// GetMCPConnection returns a user's connection to a server.
func (s *Store) GetMCPConnection(ctx context.Context, server, subject string) (*MCPConnection, error) {
	c, err := scanConn(s.db.QueryRowContext(ctx, s.q(`SELECT `+connCols+` FROM mcp_connections WHERE server = ? AND subject = ?`), server, subject))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// DeleteMCPConnection deletes a connection.
func (s *Store) DeleteMCPConnection(ctx context.Context, server, subject string) error {
	return affected(s.db.ExecContext(ctx, s.q(`DELETE FROM mcp_connections WHERE server = ? AND subject = ?`), server, subject))
}

// TouchMCPConnection records that a connection was used.
func (s *Store) TouchMCPConnection(ctx context.Context, server, subject string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, s.q(`UPDATE mcp_connections SET last_used_at = ? WHERE server = ? AND subject = ? AND last_used_at < ?`),
		at.UnixMilli(), server, subject, at.UnixMilli())
	return err
}

// RecentMCPSubject returns the subject whose connection to a server was
// used last, or "" when nobody is connected.
func (s *Store) RecentMCPSubject(ctx context.Context, server string) (string, error) {
	var subject string
	err := s.db.QueryRowContext(ctx, s.q(`SELECT subject FROM mcp_connections WHERE server = ? ORDER BY last_used_at DESC LIMIT 1`), server).Scan(&subject)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return subject, err
}

// CountMCPConnections returns the number of connections per server.
func (s *Store) CountMCPConnections(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT server, COUNT(*) FROM mcp_connections GROUP BY server`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var server string
		var n int
		if err := rows.Scan(&server, &n); err != nil {
			return nil, err
		}
		out[server] = n
	}
	return out, rows.Err()
}

// ListMCPConnections lists connections by server and subject, of one
// server or subject when set. After is "server/subject".
func (s *Store) ListMCPConnections(ctx context.Context, server, subject string, p Page) ([]*MCPConnection, bool, error) {
	query := `SELECT ` + connCols + ` FROM mcp_connections WHERE (? = '' OR server = ?) AND (? = '' OR subject = ?)`
	args := []any{server, server, subject, subject}
	if p.After != "" {
		as, asub, _ := strings.Cut(p.After, "/")
		query += ` AND (server > ? OR (server = ? AND subject > ?))`
		args = append(args, as, as, asub)
	}
	n := limit(p)
	rows, err := s.db.QueryContext(ctx, s.q(query+` ORDER BY server, subject LIMIT ?`), append(args, n+1)...)
	if err != nil {
		return nil, false, err
	}
	return collect(rows, n, scanConn)
}

// ConnectRequest is a pending connect flow: created when a call needs a
// connection, opened once in the user's browser, and taken on the
// upstream's callback.
type ConnectRequest struct {
	IDHash        string
	StateHash     string
	Server        string
	Subject       string
	VirtualServer string
	Verifier      string // PKCE code verifier
	ExpiresAt     time.Time
}

// CreateConnectRequest stores a new connect request.
func (s *Store) CreateConnectRequest(ctx context.Context, r *ConnectRequest) error {
	_, err := s.db.ExecContext(ctx, s.q(`INSERT INTO mcp_connect_requests (id_hash, state_hash, server, subject, virtual_server, verifier, expires_at)
		VALUES (?, '', ?, ?, ?, ?, ?)`), r.IDHash, r.Server, r.Subject, r.VirtualServer, r.Verifier, r.ExpiresAt.UnixMilli())
	return err
}

const connectReqCols = `id_hash, state_hash, server, subject, virtual_server, verifier, expires_at`

func scanConnectRequest(row interface{ Scan(...any) error }) (*ConnectRequest, error) {
	var r ConnectRequest
	var expires int64
	err := row.Scan(&r.IDHash, &r.StateHash, &r.Server, &r.Subject, &r.VirtualServer, &r.Verifier, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	r.ExpiresAt = toTime(expires)
	return &r, err
}

// GetConnectRequest returns an unexpired connect request that has not
// been opened yet.
func (s *Store) GetConnectRequest(ctx context.Context, idHash string, at time.Time) (*ConnectRequest, error) {
	return scanConnectRequest(s.db.QueryRowContext(ctx, s.q(`SELECT `+connectReqCols+` FROM mcp_connect_requests
		WHERE id_hash = ? AND state_hash = '' AND expires_at > ?`), idHash, at.UnixMilli()))
}

// OpenConnectRequest marks a connect request opened, binding it to the
// state sent upstream. It fails with ErrNotFound when the request is
// gone, expired or already opened, so each link works once.
func (s *Store) OpenConnectRequest(ctx context.Context, idHash, stateHash string, at time.Time) error {
	return affected(s.db.ExecContext(ctx, s.q(`UPDATE mcp_connect_requests SET state_hash = ?
		WHERE id_hash = ? AND state_hash = '' AND expires_at > ?`), stateHash, idHash, at.UnixMilli()))
}

// TakeConnectRequest deletes and returns the opened, unexpired connect
// request with a state, so a callback is accepted once.
func (s *Store) TakeConnectRequest(ctx context.Context, stateHash string, at time.Time) (*ConnectRequest, error) {
	var r *ConnectRequest
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		r, err = scanConnectRequest(tx.QueryRowContext(ctx, s.q(`SELECT `+connectReqCols+` FROM mcp_connect_requests
			WHERE state_hash = ? AND state_hash <> '' AND expires_at > ?`), stateHash, at.UnixMilli()))
		if err != nil {
			return err
		}
		return affected(tx.ExecContext(ctx, s.q(`DELETE FROM mcp_connect_requests WHERE id_hash = ?`), r.IDHash))
	})
	if err != nil {
		return nil, err
	}
	return r, nil
}

// PruneConnectRequests deletes expired connect requests.
func (s *Store) PruneConnectRequests(ctx context.Context, at time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM mcp_connect_requests WHERE expires_at <= ?`), at.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
