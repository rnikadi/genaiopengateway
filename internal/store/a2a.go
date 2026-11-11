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
