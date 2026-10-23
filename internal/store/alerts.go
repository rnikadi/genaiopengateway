package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/id"
)

// Alert is an alert the gateway raised (docs/alerts.md).
type Alert struct {
	ID    string    `json:"id"`
	Time  time.Time `json:"ts"`
	Event string    `json:"event"`
	// DedupeKey identifies the condition, so it raises one alert.
	DedupeKey string          `json:"-"`
	OrgID     string          `json:"org_id,omitempty"`
	Data      json.RawMessage `json:"data"`
}

// RecordAlert stores an alert unless one with the same dedupe key exists,
// and reports whether it stored it. Replicas race on the insert, and only
// the winner sends the alert.
func (s *Store) RecordAlert(ctx context.Context, a *Alert) (bool, error) {
	if a.ID == "" {
		a.ID = id.New("alert")
	}
	if a.Time.IsZero() {
		a.Time = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx, s.q(`INSERT INTO alerts_sent (id, ts, event, dedupe_key, org_id, data)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (dedupe_key) DO NOTHING`),
		a.ID, a.Time.UnixMilli(), a.Event, a.DedupeKey, a.OrgID, string(a.Data))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ListAlerts lists alerts newest first, of one organization when orgID is
// set and of one event when event is set.
func (s *Store) ListAlerts(ctx context.Context, orgID, event string, p Page) ([]*Alert, bool, error) {
	n := limit(p)
	query := `SELECT id, ts, event, org_id, data FROM alerts_sent WHERE (? = '' OR org_id = ?) AND (? = '' OR event = ?)`
	args := []any{orgID, orgID, event, event}
	if p.After != "" {
		query += ` AND id < ?`
		args = append(args, p.After)
	}
	rows, err := s.db.QueryContext(ctx, s.q(query+` ORDER BY id DESC LIMIT ?`), append(args, n+1)...)
	if err != nil {
		return nil, false, err
	}
	return collect(rows, n, func(row interface{ Scan(...any) error }) (*Alert, error) {
		var a Alert
		var ts int64
		var data string
		if err := row.Scan(&a.ID, &ts, &a.Event, &a.OrgID, &data); err != nil {
			return nil, err
		}
		a.Time, a.Data = toTime(ts), json.RawMessage(data)
		return &a, nil
	})
}
