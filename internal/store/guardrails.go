package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// GuardrailEvent records that a guardrail acted on a request
// (docs/guardrails.md). It never holds the text checked.
type GuardrailEvent struct {
	ID        string          `json:"id"`
	RequestID string          `json:"request_id"`
	Time      time.Time       `json:"ts"`
	OrgID     string          `json:"org_id"`
	TeamID    string          `json:"team_id"`
	KeyID     string          `json:"key_id"`
	Guardrail string          `json:"guardrail"`
	Stage     string          `json:"stage"`
	Action    string          `json:"action"`
	Failed    bool            `json:"failed"`
	Reason    string          `json:"reason,omitempty"`
	Counts    json.RawMessage `json:"counts,omitempty"`
	LatencyMS int64           `json:"latency_ms"`
}

// InsertGuardrailEvents writes a batch of events.
func (s *Store) InsertGuardrailEvents(ctx context.Context, events []GuardrailEvent) error {
	if len(events) == 0 {
		return nil
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, s.q(`INSERT INTO guardrail_events (id, request_id, ts, org_id, team_id, key_id, guardrail, stage, action, failed, reason, counts, latency_ms)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`))
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, e := range events {
			counts := string(e.Counts)
			if counts == "" {
				counts = "{}"
			}
			if _, err := stmt.ExecContext(ctx, e.ID, e.RequestID, e.Time.UnixMilli(), e.OrgID, e.TeamID, e.KeyID,
				e.Guardrail, e.Stage, e.Action, boolInt(e.Failed), e.Reason, counts, e.LatencyMS); err != nil {
				return err
			}
		}
		return nil
	})
}

// GuardrailEventQuery filters events. Zero values mean no filter.
type GuardrailEventQuery struct {
	OrgID, TeamID, KeyID, Guardrail, Action, RequestID string
	From, To                                           time.Time
}

// ListGuardrailEvents returns events newest first; After is an event id.
func (s *Store) ListGuardrailEvents(ctx context.Context, gq GuardrailEventQuery, p Page) ([]*GuardrailEvent, bool, error) {
	var where []string
	var args []any
	for _, f := range []struct{ col, v string }{
		{"org_id", gq.OrgID}, {"team_id", gq.TeamID}, {"key_id", gq.KeyID},
		{"guardrail", gq.Guardrail}, {"action", gq.Action}, {"request_id", gq.RequestID},
	} {
		if f.v != "" {
			where, args = append(where, f.col+" = ?"), append(args, f.v)
		}
	}
	if !gq.From.IsZero() {
		where, args = append(where, "ts >= ?"), append(args, gq.From.UnixMilli())
	}
	if !gq.To.IsZero() {
		where, args = append(where, "ts < ?"), append(args, gq.To.UnixMilli())
	}
	if p.After != "" {
		where, args = append(where, "id < ?"), append(args, p.After)
	}
	query := `SELECT id, request_id, ts, org_id, team_id, key_id, guardrail, stage, action, failed, reason, counts, latency_ms FROM guardrail_events`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, " AND ")
	}
	n := limit(p)
	rows, err := s.db.QueryContext(ctx, s.q(query+` ORDER BY id DESC LIMIT ?`), append(args, n+1)...)
	if err != nil {
		return nil, false, err
	}
	return collect(rows, n, func(row interface{ Scan(...any) error }) (*GuardrailEvent, error) {
		var e GuardrailEvent
		var ts int64
		var failed int
		var counts string
		if err := row.Scan(&e.ID, &e.RequestID, &ts, &e.OrgID, &e.TeamID, &e.KeyID, &e.Guardrail, &e.Stage, &e.Action,
			&failed, &e.Reason, &counts, &e.LatencyMS); err != nil {
			return nil, err
		}
		e.Time, e.Failed = toTime(ts), failed != 0
		if counts != "{}" {
			e.Counts = json.RawMessage(counts)
		}
		return &e, nil
	})
}

// PruneGuardrailEvents deletes events older than a time.
func (s *Store) PruneGuardrailEvents(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM guardrail_events WHERE ts < ?`), before.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
