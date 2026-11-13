package store

import (
	"context"
	"database/sql"
	"time"
)

// TeamCredential is a team's own credential for a provider
// (docs/team-credentials.md). Secret is sealed by the server.
type TeamCredential struct {
	TeamID    string    `json:"team_id"`
	Provider  string    `json:"provider"`
	Secret    string    `json:"-"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by"`
}

// ListTeamCredentials returns the credentials of one team, or of every
// team when teamID is empty.
func (s *Store) ListTeamCredentials(ctx context.Context, teamID string) ([]*TeamCredential, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT team_id, provider, secret, updated_at, updated_by FROM team_credentials WHERE ? = '' OR team_id = ? ORDER BY team_id, provider`), teamID, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*TeamCredential
	for rows.Next() {
		c := &TeamCredential{}
		var at int64
		if err := rows.Scan(&c.TeamID, &c.Provider, &c.Secret, &at, &c.UpdatedBy); err != nil {
			return nil, err
		}
		c.UpdatedAt = toTime(at)
		out = append(out, c)
	}
	return out, rows.Err()
}

// PutTeamCredential sets a team's credential for a provider.
func (s *Store) PutTeamCredential(ctx context.Context, c *TeamCredential) error {
	if _, err := s.GetTeam(ctx, c.TeamID); err != nil {
		return err
	}
	c.UpdatedAt = toTime(now())
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.q(`UPDATE team_credentials SET secret = ?, updated_at = ?, updated_by = ? WHERE team_id = ? AND provider = ?`),
			c.Secret, c.UpdatedAt.UnixMilli(), c.UpdatedBy, c.TeamID, c.Provider)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return nil
		}
		_, err = tx.ExecContext(ctx, s.q(`INSERT INTO team_credentials (team_id, provider, secret, updated_at, updated_by) VALUES (?, ?, ?, ?, ?)`),
			c.TeamID, c.Provider, c.Secret, c.UpdatedAt.UnixMilli(), c.UpdatedBy)
		return err
	})
}

// DeleteTeamCredential removes a team's credential for a provider.
func (s *Store) DeleteTeamCredential(ctx context.Context, teamID, provider string) error {
	return affected(s.db.ExecContext(ctx, s.q(`DELETE FROM team_credentials WHERE team_id = ? AND provider = ?`), teamID, provider))
}
