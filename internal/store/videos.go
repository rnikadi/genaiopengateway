package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// VideoJob is a video job created through the gateway. Object is the
// last job object the upstream returned.
type VideoJob struct {
	ID            string          `json:"id"`
	OrgID         string          `json:"org_id"`
	TeamID        string          `json:"team_id"`
	KeyID         string          `json:"key_id"`
	Model         string          `json:"model"`
	Provider      string          `json:"provider"`
	UpstreamModel string          `json:"upstream_model"`
	Object        json.RawMessage `json:"object"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

const videoCols = `id, org_id, team_id, key_id, model, provider, upstream_model, object, created_at, updated_at`

func scanVideoJob(row interface{ Scan(...any) error }) (*VideoJob, error) {
	var j VideoJob
	var object string
	var created, updated int64
	if err := row.Scan(&j.ID, &j.OrgID, &j.TeamID, &j.KeyID, &j.Model, &j.Provider, &j.UpstreamModel, &object, &created, &updated); err != nil {
		return nil, err
	}
	j.Object, j.CreatedAt, j.UpdatedAt = json.RawMessage(object), toTime(created), toTime(updated)
	return &j, nil
}

// CreateVideoJob records a new job. A job id already recorded keeps its
// first owner.
func (s *Store) CreateVideoJob(ctx context.Context, j *VideoJob) error {
	t := now()
	_, err := s.db.ExecContext(ctx, s.q(`INSERT INTO video_jobs (`+videoCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`),
		j.ID, j.OrgID, j.TeamID, j.KeyID, j.Model, j.Provider, j.UpstreamModel, string(j.Object), t, t)
	return err
}

// GetVideoJob returns a job by id.
func (s *Store) GetVideoJob(ctx context.Context, id string) (*VideoJob, error) {
	j, err := scanVideoJob(s.db.QueryRowContext(ctx, s.q(`SELECT `+videoCols+` FROM video_jobs WHERE id = ?`), id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

// UpdateVideoJob stores the latest job object.
func (s *Store) UpdateVideoJob(ctx context.Context, id string, object json.RawMessage) error {
	return affected(s.db.ExecContext(ctx, s.q(`UPDATE video_jobs SET object = ?, updated_at = ? WHERE id = ?`), string(object), now(), id))
}

// DeleteVideoJob removes a job's record.
func (s *Store) DeleteVideoJob(ctx context.Context, id string) error {
	return affected(s.db.ExecContext(ctx, s.q(`DELETE FROM video_jobs WHERE id = ?`), id))
}

// ListVideoJobs lists jobs, newest first: one organization's when orgID
// is set, one team's when teamID is set, and one key's when keyID is set. After is the id
// of the last job of the previous page.
func (s *Store) ListVideoJobs(ctx context.Context, orgID, teamID, keyID string, p Page) ([]*VideoJob, bool, error) {
	query := `SELECT ` + videoCols + ` FROM video_jobs WHERE (? = '' OR org_id = ?) AND (? = '' OR team_id = ?) AND (? = '' OR key_id = ?)`
	args := []any{orgID, orgID, teamID, teamID, keyID, keyID}
	if p.After != "" {
		var created int64
		err := s.db.QueryRowContext(ctx, s.q(`SELECT created_at FROM video_jobs WHERE id = ?`), p.After).Scan(&created)
		if errors.Is(err, sql.ErrNoRows) {
			return []*VideoJob{}, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		query += ` AND (created_at < ? OR (created_at = ? AND id < ?))`
		args = append(args, created, created, p.After)
	}
	n := limit(p)
	rows, err := s.db.QueryContext(ctx, s.q(query+` ORDER BY created_at DESC, id DESC LIMIT ?`), append(args, n+1)...)
	if err != nil {
		return nil, false, err
	}
	return collect(rows, n, scanVideoJob)
}
