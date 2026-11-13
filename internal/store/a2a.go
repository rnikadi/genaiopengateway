package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Kinds of A2A object whose owner is recorded.
const (
	A2ATask    = "task"
	A2AContext = "context"
)

// A2AObject is an A2A task or context id an agent returned, with the
// caller that caused it (ADR 0033).
type A2AObject struct {
	Agent     string
	Kind      string
	ID        string
	OrgID     string
	TeamID    string
	KeyID     string
	CreatedAt time.Time
}

// RecordA2AObject records an object's owner. An id already recorded
// keeps its first owner.
func (s *Store) RecordA2AObject(ctx context.Context, o *A2AObject) error {
	_, err := s.db.ExecContext(ctx, s.q(`INSERT INTO a2a_objects (agent, kind, id, org_id, team_id, key_id, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (agent, kind, id) DO NOTHING`), o.Agent, o.Kind, o.ID, o.OrgID, o.TeamID, o.KeyID, now())
	return err
}

// GetA2AObject returns an object's record.
func (s *Store) GetA2AObject(ctx context.Context, agent, kind, id string) (*A2AObject, error) {
	o := A2AObject{Agent: agent, Kind: kind, ID: id}
	var created int64
	err := s.db.QueryRowContext(ctx, s.q(`SELECT org_id, team_id, key_id, created_at FROM a2a_objects WHERE agent = ? AND kind = ? AND id = ?`), agent, kind, id).
		Scan(&o.OrgID, &o.TeamID, &o.KeyID, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	o.CreatedAt = toTime(created)
	return &o, nil
}

// A2APush is a push notification configuration set through the gateway
// (docs/a2a.md#push-notifications). ID is the hash of the secret in the
// URL the agent was given; URL is where the caller wants notifications.
// TaskID and ConfigID are empty until the agent names them.
type A2APush struct {
	ID        string
	Agent     string
	TaskID    string
	ConfigID  string
	OrgID     string
	TeamID    string
	KeyID     string
	URL       string
	Secret    string // sealed by the server; "" when the caller set none
	CreatedAt time.Time
	ExpiresAt time.Time
}

// InsertA2APush stores a configuration.
func (s *Store) InsertA2APush(ctx context.Context, p *A2APush) error {
	_, err := s.db.ExecContext(ctx, s.q(`INSERT INTO a2a_push (id, agent, task_id, config_id, org_id, team_id, key_id, url, secret, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`), p.ID, p.Agent, p.TaskID, p.ConfigID, p.OrgID, p.TeamID, p.KeyID, p.URL, p.Secret, now(), p.ExpiresAt.UnixMilli())
	return err
}

// GetA2APush returns a configuration that has not expired.
func (s *Store) GetA2APush(ctx context.Context, id string, at time.Time) (*A2APush, error) {
	p := A2APush{ID: id}
	var created, expires int64
	err := s.db.QueryRowContext(ctx, s.q(`SELECT agent, task_id, config_id, org_id, team_id, key_id, url, secret, created_at, expires_at
		FROM a2a_push WHERE id = ? AND expires_at > ?`), id, at.UnixMilli()).
		Scan(&p.Agent, &p.TaskID, &p.ConfigID, &p.OrgID, &p.TeamID, &p.KeyID, &p.URL, &p.Secret, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.CreatedAt, p.ExpiresAt = toTime(created), toTime(expires)
	return &p, nil
}

// NameA2APush fills in the task and the configuration id of a
// configuration, where they are not known yet.
func (s *Store) NameA2APush(ctx context.Context, id, taskID, configID string) error {
	_, err := s.db.ExecContext(ctx, s.q(`UPDATE a2a_push SET
		task_id = CASE WHEN task_id = '' THEN ? ELSE task_id END,
		config_id = CASE WHEN config_id = '' THEN ? ELSE config_id END
		WHERE id = ?`), taskID, configID, id)
	return err
}

// DeleteA2APush removes a task's configuration with the given id, or
// all of the task's when configID is empty.
func (s *Store) DeleteA2APush(ctx context.Context, agent, taskID, configID string) error {
	if taskID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, s.q(`DELETE FROM a2a_push WHERE agent = ? AND task_id = ? AND (? = '' OR config_id = ?)`), agent, taskID, configID, configID)
	return err
}

// PruneA2APush removes expired configurations.
func (s *Store) PruneA2APush(ctx context.Context, at time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM a2a_push WHERE expires_at <= ?`), at.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
