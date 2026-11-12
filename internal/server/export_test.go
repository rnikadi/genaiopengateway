package server

import (
	"context"
	"encoding/csv"
	"strings"
	"testing"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/store"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

func TestUsageExports(t *testing.T) {
	e := newAdminEnv(t, "")
	var org, other, team, formula idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"acme"}`, 201, &org)
	e.call("POST", "/admin/v1/orgs", `{"name":"other"}`, 201, &other)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"search"}`, 201, &team)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"=cmd()"}`, 201, &formula)
	day := time.Date(2026, 3, 9, 12, 0, 0, 0, time.UTC)
	rec := func(id, orgID, teamID string, at time.Time, cost float64) usage.Record {
		return usage.Record{RequestID: id, Time: at, OrgID: orgID, TeamID: teamID, KeyID: "k", Endpoint: "chat.completions",
			Model: "chat", Provider: "up", UpstreamModel: "m", Status: 200, Tokens: usage.Tokens{InputTokens: 100, OutputTokens: 20}, CostUSD: cost}
	}
	if err := e.srv.store.InsertUsage(context.Background(), []usage.Record{
		rec("x1", org.ID, team.ID, day, 3), rec("x2", org.ID, team.ID, day.Add(time.Hour), 3),
		rec("x3", org.ID, formula.ID, day, 2), rec("x4", org.ID, "team_gone", day.Add(24*time.Hour), 1),
		rec("x5", other.ID, "", day, 1),
	}); err != nil {
		t.Fatal(err)
	}
	rng := "&from=2026-03-01T00:00:00Z&to=2026-04-01T00:00:00Z"
	read := func(path, key string, want int) [][]string {
		t.Helper()
		rec := do(e.h, "GET", path, key, "")
		if rec.Code != want {
			t.Fatalf("GET %s = %d %s", path, rec.Code, rec.Body)
		}
		if want != 200 {
			return nil
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") || !strings.Contains(rec.Header().Get("Content-Disposition"), "20260301-20260331.csv") {
			t.Fatalf("headers: %v", rec.Header())
		}
		rows, err := csv.NewReader(rec.Body).ReadAll()
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	col := func(rows [][]string, name string) int {
		for i, h := range rows[0] {
			if h == name {
				return i
			}
		}
		t.Fatalf("no column %s in %v", name, rows[0])
		return -1
	}

	// The chargeback statement: one line per team, biggest first, with its share.
	cb := read("/admin/v1/usage/export?org_id="+org.ID+rng, adminToken, 200)
	if len(cb) != 4 {
		t.Fatalf("chargeback rows: %v", cb)
	}
	teamCol, costCol, shareCol := col(cb, "team"), col(cb, "cost_usd"), col(cb, "share_of_total")
	if cb[1][teamCol] != "search" || cb[1][costCol] != "6.000000" || cb[1][shareCol] != "0.6667" {
		t.Fatalf("first line: %v", cb[1])
	}
	// A name that would run as a formula in a spreadsheet is quoted, and a deleted team keeps its id.
	if cb[2][teamCol] != "'=cmd()" || cb[3][teamCol] != "team_gone" {
		t.Fatalf("names: %v %v", cb[2], cb[3])
	}
	// Across organizations, for owners.
	if all := read("/admin/v1/usage/export?format=chargeback"+rng, adminToken, 200); len(all) != 5 {
		t.Fatalf("all organizations: %v", all)
	}

	// FOCUS: one row per day, team, upstream model and endpoint.
	fc := read("/admin/v1/usage/export?format=focus&org_id="+org.ID+rng, adminToken, 200)
	if len(fc) != 4 || len(fc[0]) != len(focusColumns) {
		t.Fatalf("focus rows: %v", fc)
	}
	get := func(row []string, name string) string { return row[col(fc, name)] }
	first := fc[1]
	for name, want := range map[string]string{
		"BillingCurrency": "USD", "BillingAccountId": org.ID, "BillingAccountName": "acme",
		"BillingPeriodStart": "2026-03-01T00:00:00Z", "BillingPeriodEnd": "2026-04-01T00:00:00Z",
		"ChargePeriodStart": "2026-03-09T00:00:00Z", "ChargePeriodEnd": "2026-03-10T00:00:00Z",
		"ChargeCategory": "Usage", "ConsumedUnit": "Tokens", "ServiceCategory": "AI and Machine Learning",
		"SkuId": "m", "ResourceId": "chat", "ProviderName": "up",
	} {
		if got := get(first, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	var sum float64
	for _, row := range fc[1:] {
		switch get(row, "BilledCost") {
		case "6.000000":
			sum += 6
			if get(row, "ConsumedQuantity") != "240" || get(row, "x_Requests") != "2" || !strings.Contains(get(row, "Tags"), `"team":"search"`) {
				t.Errorf("search row: %v", row)
			}
		case "2.000000":
			sum += 2
		case "1.000000":
			sum += 1
		}
	}
	if sum != 9 {
		t.Fatalf("focus costs sum to %v", sum)
	}

	if rec := do(e.h, "GET", "/admin/v1/usage/export?format=xlsx", adminToken, ""); rec.Code != 400 {
		t.Fatalf("unknown format = %d", rec.Code)
	}
	// An organization's viewer exports only their organization.
	viewer := e.session("v@example.com", store.Grant{Role: "viewer", OrgID: org.ID})
	if rec := e.browser(viewer, "GET", "/admin/v1/usage/export?org_id="+org.ID+rng, ""); rec.Code != 200 {
		t.Fatalf("viewer, own organization = %d", rec.Code)
	}
	for _, path := range []string{"/admin/v1/usage/export?org_id=" + other.ID, "/admin/v1/usage/export"} {
		if rec := e.browser(viewer, "GET", path, ""); rec.Code != 403 {
			t.Fatalf("viewer GET %s = %d", path, rec.Code)
		}
	}
}
