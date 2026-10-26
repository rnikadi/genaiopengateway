package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Grant is one operator role, gateway-wide when OrgID is empty.
type Grant struct {
	Role  string `json:"role"`
	OrgID string `json:"org_id,omitempty"`
}

// Session is a signed-in operator of the admin UI. IDHash is the SHA-256
// of the cookie value; the value itself is never stored.
type Session struct {
	IDHash  string
	Subject string
	Email   string
	Name    string
	Grants  []Grant
	// Claims are what role mappings match, for SSO sessions only.
	Claims    *SessionClaims
	CreatedAt time.Time
	ExpiresAt time.Time
}

// SessionClaims are the identity provider's view of a user that role
// mappings match: the verified email and the groups.
type SessionClaims struct {
	Email  string   `json:"email,omitempty"`
	Groups []string `json:"groups,omitempty"`
}

// CreateSession stores a new session.
func (s *Store) CreateSession(ctx context.Context, ss *Session) error {
	roles, err := json.Marshal(ss.Grants)
	if err != nil {
		return err
	}
	var claims sql.NullString
	if ss.Claims != nil {
		b, err := json.Marshal(ss.Claims)
		if err != nil {
			return err
		}
		claims = sql.NullString{String: string(b), Valid: true}
	}
	_, err = s.db.ExecContext(ctx, s.q(`INSERT INTO admin_sessions (id_hash, subject, email, name, roles, claims, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		ss.IDHash, ss.Subject, ss.Email, ss.Name, string(roles), claims, ss.CreatedAt.UnixMilli(), ss.ExpiresAt.UnixMilli())
	return err
}

// GetSession returns an unexpired session, or ErrNotFound.
func (s *Store) GetSession(ctx context.Context, idHash string) (*Session, error) {
	var ss Session
	var roles string
	var claims sql.NullString
	var created, expires int64
	err := s.db.QueryRowContext(ctx, s.q(`SELECT id_hash, subject, email, name, roles, claims, created_at, expires_at FROM admin_sessions WHERE id_hash = ? AND expires_at > ?`),
		idHash, now()).Scan(&ss.IDHash, &ss.Subject, &ss.Email, &ss.Name, &roles, &claims, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(roles), &ss.Grants); err != nil {
		return nil, err
	}
	if claims.Valid {
		ss.Claims = &SessionClaims{}
		if err := json.Unmarshal([]byte(claims.String), ss.Claims); err != nil {
			return nil, err
		}
	}
	ss.CreatedAt, ss.ExpiresAt = toTime(created), toTime(expires)
	return &ss, nil
}

// DeleteSession signs a session out. Deleting a missing one is not an error.
func (s *Store) DeleteSession(ctx context.Context, idHash string) error {
	_, err := s.db.ExecContext(ctx, s.q(`DELETE FROM admin_sessions WHERE id_hash = ?`), idHash)
	return err
}

// DeleteOtherSessions signs out every session but one, or with subject
// set, every session of that user but one, and reports how many.
func (s *Store) DeleteOtherSessions(ctx context.Context, keepHash, subject string) (int64, error) {
	q, args := `DELETE FROM admin_sessions WHERE id_hash <> ?`, []any{keepHash}
	if subject != "" {
		q += ` AND subject = ?`
		args = append(args, subject)
	}
	res, err := s.db.ExecContext(ctx, s.q(q), args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PruneSessions deletes sessions that expired before a time.
func (s *Store) PruneSessions(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM admin_sessions WHERE expires_at < ?`), before.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
