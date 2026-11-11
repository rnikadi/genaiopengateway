package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// CedarPolicies is one version of an organization's Cedar policies
// (docs/cedar.md).
type CedarPolicies struct {
	OrgID     string    `json:"org_id"`
	Version   int64     `json:"version"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
}

// SaveCedarPolicies stores a new version of an organization's policies;
// empty text removes them. The version is one more than the last.
func (s *Store) SaveCedarPolicies(ctx context.Context, orgID, text, by string) (*CedarPolicies, error) {
	if _, err := s.GetOrg(ctx, orgID); err != nil {
		return nil, err
	}
	p := &CedarPolicies{OrgID: orgID, Text: text, CreatedBy: by, CreatedAt: toTime(now())}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, s.q(`SELECT COALESCE(MAX(version), 0) + 1 FROM cedar_policies WHERE org_id = ?`), orgID).Scan(&p.Version); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, s.q(`INSERT INTO cedar_policies (org_id, version, text, created_at, created_by) VALUES (?, ?, ?, ?, ?)`),
			p.OrgID, p.Version, p.Text, p.CreatedAt.UnixMilli(), p.CreatedBy)
		if isUnique(err) {
			return ErrConflict
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

// GetCedarPolicies returns an organization's policies in force, or a
// given version when version is above zero. ErrNotFound means none.
func (s *Store) GetCedarPolicies(ctx context.Context, orgID string, version int64) (*CedarPolicies, error) {
	q := `SELECT org_id, version, text, created_at, created_by FROM cedar_policies WHERE org_id = ? ORDER BY version DESC LIMIT 1`
	args := []any{orgID}
	if version > 0 {
		q = `SELECT org_id, version, text, created_at, created_by FROM cedar_policies WHERE org_id = ? AND version = ?`
		args = append(args, version)
	}
	p, err := scanCedar(s.db.QueryRowContext(ctx, s.q(q), args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// ListCedarVersions returns an organization's versions, newest first,
// without their text.
func (s *Store) ListCedarVersions(ctx context.Context, orgID string, limit int) ([]*CedarPolicies, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT org_id, version, '', created_at, created_by FROM cedar_policies WHERE org_id = ? ORDER BY version DESC LIMIT ?`), orgID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CedarPolicies
	for rows.Next() {
		p, err := scanCedar(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// LoadCedarPolicies returns the policies in force of every organization
// that has any.
func (s *Store) LoadCedarPolicies(ctx context.Context) (map[string]*CedarPolicies, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.org_id, c.version, c.text, c.created_at, c.created_by FROM cedar_policies c
		JOIN (SELECT org_id, MAX(version) AS version FROM cedar_policies GROUP BY org_id) m ON m.org_id = c.org_id AND m.version = c.version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*CedarPolicies{}
	for rows.Next() {
		p, err := scanCedar(rows)
		if err != nil {
			return nil, err
		}
		if p.Text != "" {
			out[p.OrgID] = p
		}
	}
	return out, rows.Err()
}

func scanCedar(row interface{ Scan(...any) error }) (*CedarPolicies, error) {
	var p CedarPolicies
	var at int64
	if err := row.Scan(&p.OrgID, &p.Version, &p.Text, &at, &p.CreatedBy); err != nil {
		return nil, err
	}
	p.CreatedAt = toTime(at)
	return &p, nil
}
