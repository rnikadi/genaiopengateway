package store

import (
	"context"
	"encoding/json"
	"testing"
)

func TestAlertsFireOnce(t *testing.T) {
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			a := func(org, key string) *Alert {
				return &Alert{Event: "budget.threshold", DedupeKey: key, OrgID: org, Data: json.RawMessage(`{"threshold":0.8}`)}
			}
			if ok, err := s.RecordAlert(ctx, a("o1", "k1")); err != nil || !ok {
				t.Fatalf("first alert: %v, %v", ok, err)
			}
			// Another replica raising the same alert loses the race.
			if ok, err := s.RecordAlert(ctx, a("o1", "k1")); err != nil || ok {
				t.Fatalf("duplicate alert: %v, %v", ok, err)
			}
			if ok, _ := s.RecordAlert(ctx, a("o2", "k2")); !ok {
				t.Fatal("a different alert was refused")
			}
			if ok, _ := s.RecordAlert(ctx, &Alert{Event: "mcp.tool_pending", DedupeKey: "k3", Data: json.RawMessage(`{}`)}); !ok {
				t.Fatal("an alert without an organization was refused")
			}

			all, _, err := s.ListAlerts(ctx, "", "", Page{})
			if err != nil || len(all) != 3 {
				t.Fatalf("all alerts: %+v, %v", all, err)
			}
			mine, _, _ := s.ListAlerts(ctx, "o1", "", Page{})
			if len(mine) != 1 || string(mine[0].Data) != `{"threshold":0.8}` || mine[0].Time.IsZero() {
				t.Fatalf("o1 alerts: %+v", mine)
			}
			budget, more, _ := s.ListAlerts(ctx, "", "budget.threshold", Page{Limit: 1})
			if len(budget) != 1 || !more {
				t.Fatalf("first page of budget alerts: %+v, more %v", budget, more)
			}
			rest, more, _ := s.ListAlerts(ctx, "", "budget.threshold", Page{Limit: 1, After: budget[0].ID})
			if len(rest) != 1 || more || rest[0].ID == budget[0].ID {
				t.Fatalf("second page: %+v, more %v", rest, more)
			}
		})
	}
}
