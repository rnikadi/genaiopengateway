package store

import (
	"context"
	"testing"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/usage"
)

func TestUsageRollups(t *testing.T) {
	ctx := context.Background()
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			org := "roll_" + name
			d0 := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
			rec := func(id string, at time.Time, model string, status int, cost float64) usage.Record {
				return usage.Record{RequestID: org + id, Time: at, OrgID: org, TeamID: "t", KeyID: "k", Endpoint: "chat.completions", Model: model,
					Provider: "up", UpstreamModel: "m", Status: status, LatencyMS: 10, Tokens: usage.Tokens{InputTokens: 10, OutputTokens: 2}, CostUSD: cost}
			}
			if err := s.InsertUsage(ctx, []usage.Record{
				rec("1", d0.Add(time.Hour), "a", 200, 1), rec("2", d0.Add(2*time.Hour), "a", 500, 2), rec("3", d0.Add(3*time.Hour), "b", 200, 4),
				rec("4", d0.Add(25*time.Hour), "a", 200, 8),
				rec("5", d0.Add(49*time.Hour), "a", 200, 16),
			}); err != nil {
				t.Fatal(err)
			}
			total := func(uq UsageQuery) (float64, int64, int) {
				t.Helper()
				uq.OrgID = org
				rows, err := s.QueryUsage(ctx, uq)
				if err != nil {
					t.Fatal(err)
				}
				var cost float64
				var n int64
				for _, r := range rows {
					cost += r.CostUSD
					n += r.Requests
				}
				return cost, n, len(rows)
			}
			all := UsageQuery{From: d0, To: d0.Add(72 * time.Hour), GroupBy: "day"}
			if cost, n, groups := total(all); cost != 31 || n != 5 || groups != 3 {
				t.Fatalf("raw: %v %v %v", cost, n, groups)
			}

			// Roll up twice: the second run replaces, it does not add.
			for range 2 {
				if err := s.RollupUsage(ctx, d0, d0.Add(72*time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			last, first, err := s.RollupBounds(ctx)
			if err != nil || last.Before(d0.Add(48*time.Hour)) || first.After(d0) {
				t.Fatalf("bounds: %v %v %v", last, first, err)
			}

			// The first two days' rows are pruned; reports still add up.
			if _, err := s.db.ExecContext(ctx, s.q(`DELETE FROM usage WHERE org_id = ? AND ts < ?`), org, d0.Add(48*time.Hour).UnixMilli()); err != nil {
				t.Fatal(err)
			}
			if cost, _, _ := total(all); cost != 16 {
				t.Fatalf("after pruning, without rollups: %v", cost)
			}
			all.RawFrom = d0.Add(48 * time.Hour)
			if cost, n, groups := total(all); cost != 31 || n != 5 || groups != 3 {
				t.Fatalf("with rollups: %v %v %v", cost, n, groups)
			}
			byModel := all
			byModel.GroupBy = "model"
			rows, err := s.QueryUsage(ctx, func() UsageQuery { byModel.OrgID = org; return byModel }())
			if err != nil || len(rows) != 2 || rows[0].Group != "a" || rows[0].CostUSD != 27 || rows[0].InputTokens != 40 || rows[1].CostUSD != 4 {
				t.Fatalf("by model: %+v, %v", rows, err)
			}
			// A range that starts inside a rolled-up day takes the whole day.
			part := UsageQuery{From: d0.Add(30 * time.Hour), To: d0.Add(72 * time.Hour), GroupBy: "day", RawFrom: all.RawFrom}
			if cost, _, groups := total(part); cost != 24 || groups != 2 {
				t.Fatalf("partial day: %v %v", cost, groups)
			}
			// A range that ends before the raw rows start reads rollups only.
			old := UsageQuery{From: d0, To: d0.Add(24 * time.Hour), GroupBy: "day", RawFrom: all.RawFrom}
			if cost, n, _ := total(old); cost != 7 || n != 3 {
				t.Fatalf("old range: %v %v", cost, n)
			}
			exp, err := s.UsageExport(ctx, UsageQuery{OrgID: org, From: d0, To: d0.Add(72 * time.Hour), RawFrom: all.RawFrom})
			if err != nil || len(exp) != 4 || !exp[0].Day.Equal(d0) || exp[3].CostUSD != 16 {
				t.Fatalf("export: %+v, %v", exp, err)
			}

			if n, err := s.PruneRollups(ctx, d0.Add(24*time.Hour)); err != nil || n < 2 {
				t.Fatalf("prune rollups: %d, %v", n, err)
			}
			if cost, _, _ := total(all); cost != 24 {
				t.Fatalf("after pruning rollups: %v", cost)
			}
		})
	}
}
