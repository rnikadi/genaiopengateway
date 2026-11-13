package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/store"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

func TestUsageRollupsOutliveRetention(t *testing.T) {
	e := newAdminEnv(t, "")
	e.call("PUT", "/admin/v1/config/settings/gateway", `{"store":{"usage_retention":"30d"}}`, 201, nil)
	if got := e.srv.rt.Load().cfg.Store.UsageRetention.Std(); got != 30*24*time.Hour {
		t.Fatalf("retention = %v", got)
	}
	var org idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"acme"}`, 201, &org)
	ctx := context.Background()
	now := time.Now().UTC()
	var recs []usage.Record
	for i, age := range []int{90, 60, 31, 10, 0} { // days ago
		recs = append(recs, usage.Record{RequestID: fmt.Sprintf("r%d", i), Time: now.AddDate(0, 0, -age), OrgID: org.ID, TeamID: "t", KeyID: "k",
			Endpoint: "chat.completions", Model: "chat", Provider: "up", UpstreamModel: "m", Status: 200, Tokens: usage.Tokens{InputTokens: 100}, CostUSD: 1})
	}
	if err := e.srv.store.InsertUsage(ctx, recs); err != nil {
		t.Fatal(err)
	}
	spend := func() float64 {
		t.Helper()
		var out struct {
			Data []store.UsageRow `json:"data"`
		}
		e.call("GET", "/admin/v1/usage?group_by=day&org_id="+org.ID+"&from="+now.AddDate(0, 0, -120).Format(time.RFC3339), "", 200, &out)
		var sum float64
		for _, r := range out.Data {
			sum += r.CostUSD
		}
		return sum
	}
	if got := spend(); got != 2 {
		// Before the first rollup, days past the retention are not reported:
		// their rows may be partly gone.
		t.Fatalf("before the rollup = %v", got)
	}
	// What the hourly loop does: sum, then prune whole days.
	if err := e.srv.rollupUsage(ctx, now); err != nil {
		t.Fatal(err)
	}
	e.srv.prune(ctx, e.srv.rawFrom(now))
	raw, err := e.srv.store.QueryUsage(ctx, store.UsageQuery{OrgID: org.ID, From: now.AddDate(0, 0, -120), To: now.Add(time.Hour), GroupBy: "day"})
	if err != nil || len(raw) != 2 {
		t.Fatalf("usage rows after pruning: %+v, %v", raw, err)
	}
	if got := spend(); got != 5 {
		t.Fatalf("spend over 120 days after pruning = %v, want 5", got)
	}
	// A second pass does not rebuild pruned days from nothing.
	if err := e.srv.rollupUsage(ctx, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := spend(); got != 5 {
		t.Fatalf("after a second rollup = %v", got)
	}

	// The rollups themselves, for a warehouse.
	rec := do(e.h, "GET", "/admin/v1/usage/rollups?org_id="+org.ID+"&from="+now.AddDate(0, 0, -120).Format(time.RFC3339), adminToken, "")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("rollups: %d %s", rec.Code, rec.Body)
	}
	var lines []store.Rollup
	sc := bufio.NewScanner(rec.Body)
	for sc.Scan() {
		var r store.Rollup
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, r)
	}
	if len(lines) != 5 || lines[0].Day != now.AddDate(0, 0, -90).Format("2006-01-02") || lines[0].Requests != 1 || lines[0].InputTokens != 100 || lines[0].KeyID != "k" {
		t.Fatalf("rollup lines: %+v", lines)
	}
	rec = do(e.h, "GET", "/admin/v1/usage/rollups?format=csv&org_id="+org.ID+"&from="+now.AddDate(0, 0, -5).Format(time.RFC3339), adminToken, "")
	if got := strings.Count(strings.TrimSpace(rec.Body.String()), "\n"); rec.Code != 200 || got != 1 {
		t.Fatalf("csv: %d %s", rec.Code, rec.Body)
	}
	if rec := do(e.h, "GET", "/admin/v1/usage/rollups?format=parquet", adminToken, ""); rec.Code != 400 {
		t.Fatalf("unknown format = %d", rec.Code)
	}
}
