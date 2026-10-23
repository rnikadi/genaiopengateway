package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestGuardrailEvents(t *testing.T) {
	ctx := context.Background()
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			now := time.Now().UTC()
			events := []GuardrailEvent{
				{ID: "gev_1", RequestID: "req-1", Time: now.Add(-48 * time.Hour), OrgID: "org_a", Guardrail: "pii", Stage: "input", Action: "redact", Counts: json.RawMessage(`{"email":2}`)},
				{ID: "gev_2", RequestID: "req-2", Time: now, OrgID: "org_a", TeamID: "team_a", Guardrail: "dlp", Stage: "output", Action: "block", Failed: true, Reason: "check timed out", LatencyMS: 2000},
				{ID: "gev_3", RequestID: "req-3", Time: now, OrgID: "org_b", Guardrail: "pii", Stage: "input", Action: "log"},
			}
			if err := s.InsertGuardrailEvents(ctx, events); err != nil {
				t.Fatal(err)
			}
			got, more, err := s.ListGuardrailEvents(ctx, GuardrailEventQuery{OrgID: "org_a"}, Page{})
			if err != nil || more || len(got) != 2 || got[0].ID != "gev_2" || !got[0].Failed || got[0].Reason != "check timed out" ||
				string(got[1].Counts) != `{"email":2}` || got[0].Counts != nil {
				t.Fatalf("list = %+v, %v", got, err)
			}
			got, _, _ = s.ListGuardrailEvents(ctx, GuardrailEventQuery{Guardrail: "pii", Action: "log"}, Page{})
			if len(got) != 1 || got[0].OrgID != "org_b" {
				t.Fatalf("filtered = %+v", got)
			}
			got, more, _ = s.ListGuardrailEvents(ctx, GuardrailEventQuery{}, Page{Limit: 1})
			if len(got) != 1 || !more {
				t.Fatalf("page = %+v, more %v", got, more)
			}
			n, err := s.PruneGuardrailEvents(ctx, now.Add(-time.Hour))
			if err != nil || n != 1 {
				t.Fatalf("prune = %d, %v", n, err)
			}
		})
	}
}
