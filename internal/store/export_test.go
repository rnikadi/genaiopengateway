package store

import (
	"context"
	"testing"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/usage"
)

func TestUsageExport(t *testing.T) {
	ctx := context.Background()
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			day := time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC)
			rec := func(id string, at time.Time, org, team, model string, in int64, cost float64) usage.Record {
				return usage.Record{RequestID: "exp-" + name + id, Time: at, OrgID: "exp_" + org, TeamID: team, KeyID: "k", Endpoint: "chat.completions",
					Model: "chat", Provider: "up", UpstreamModel: model, Status: 200, Tokens: usage.Tokens{InputTokens: in, OutputTokens: 1}, CostUSD: cost}
			}
			if err := s.InsertUsage(ctx, []usage.Record{
				rec("1", day.Add(time.Hour), "a", "t1", "m", 10, 1),
				rec("2", day.Add(23*time.Hour), "a", "t1", "m", 5, 0.5),
				rec("3", day.Add(25*time.Hour), "a", "t1", "m", 7, 2),
				rec("4", day.Add(2*time.Hour), "a", "t2", "n", 1, 4),
				rec("5", day.Add(2*time.Hour), "b", "t3", "m", 1, 8),
				rec("6", day.Add(72*time.Hour), "a", "t1", "m", 1, 16), // after the range
			}); err != nil {
				t.Fatal(err)
			}
			got, err := s.UsageExport(ctx, UsageQuery{OrgID: "exp_a", From: day, To: day.Add(48 * time.Hour)})
			if err != nil || len(got) != 3 {
				t.Fatalf("export = %+v, %v", got, err)
			}
			if r := got[0]; !r.Day.Equal(day) || r.TeamID != "t1" || r.Requests != 2 || r.InputTokens != 15 || r.CostUSD != 1.5 || r.UpstreamModel != "m" {
				t.Fatalf("first row = %+v", r)
			}
			if r := got[1]; r.TeamID != "t2" || r.CostUSD != 4 {
				t.Fatalf("second row = %+v", r)
			}
			if r := got[2]; !r.Day.Equal(day.Add(24*time.Hour)) || r.CostUSD != 2 {
				t.Fatalf("third row = %+v", r)
			}
			if got, err := s.UsageExport(ctx, UsageQuery{OrgID: "exp_a", TeamID: "t2", From: day, To: day.Add(48 * time.Hour)}); err != nil || len(got) != 1 {
				t.Fatalf("one team = %+v, %v", got, err)
			}
		})
	}
}
