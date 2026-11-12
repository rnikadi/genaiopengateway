package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/store"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

func TestSpendForecastAndAnomaly(t *testing.T) {
	e := newAdminEnv(t, "")
	var org, fast, slow, spiky idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"acme","budget":{"amount_usd":10000,"period":"monthly"}}`, 201, &org)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"fast","budget":{"amount_usd":100,"period":"monthly"}}`, 201, &fast)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"slow","budget":{"amount_usd":100,"period":"monthly"}}`, 201, &slow)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"spiky"}`, 201, &spiky)
	ctx := context.Background()
	now := time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC)
	var recs []usage.Record
	add := func(team string, at time.Time, cost float64) {
		recs = append(recs, usage.Record{RequestID: fmt.Sprintf("f%d", len(recs)), Time: at, OrgID: org.ID, TeamID: team, KeyID: "k",
			Endpoint: "chat.completions", Model: "chat", Provider: "up", UpstreamModel: "m", Status: 200, CostUSD: cost})
	}
	for d := 1; d <= 10; d++ {
		at := now.Add(-time.Duration(d) * 24 * time.Hour).Add(time.Hour)
		add(fast.ID, at, 6)  // $6 a day: $100 runs out mid-month
		add(slow.ID, at, 1)  // $1 a day: fine
		add(spiky.ID, at, 2) // $2 a day until today
	}
	add(spiky.ID, now.Add(-time.Hour), 30)
	if err := e.srv.store.InsertUsage(ctx, recs); err != nil {
		t.Fatal(err)
	}

	fs, err := e.srv.forecasts(ctx, now)
	if err != nil || len(fs) != 3 {
		t.Fatalf("forecasts = %+v, %v", fs, err)
	}
	f := fs[0]
	// Ten days of $6 fall in March; the rate is the last week's $42 over 7 days.
	if f.TeamName != "fast" || f.SpentUSD != 60 || f.DailyUSD != 6 || f.ExhaustsAt == nil || f.Exhausted {
		t.Fatalf("fast team: %+v", f)
	}
	if want := time.Date(2026, 3, 18, 4, 0, 0, 0, time.UTC); !f.ExhaustsAt.Equal(want) {
		t.Fatalf("exhausts at %v, want %v", f.ExhaustsAt, want)
	}
	if want := 60 + 6*20.5; f.ProjectedUSD != want {
		t.Fatalf("projected %v, want %v", f.ProjectedUSD, want)
	}
	for _, f := range fs[1:] {
		if f.ExhaustsAt != nil {
			t.Fatalf("%s %s should last the month: %+v", f.Scope, f.TeamName, f)
		}
	}

	e.srv.checkSpend(ctx, now)
	e.srv.checkSpend(ctx, now.Add(time.Hour)) // each alert is raised once
	alerts, _, err := e.srv.store.ListAlerts(ctx, "", "", store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, a := range alerts {
		if _, dup := seen[a.Event]; dup {
			t.Fatalf("alert %s raised twice: %+v", a.Event, alerts)
		}
		seen[a.Event] = string(a.Data)
	}
	if len(seen) != 2 || seen["budget.forecast"] == "" || seen["spend.anomaly"] == "" {
		t.Fatalf("alerts: %v", seen)
	}

	// The API shows forecasts to those who may read the organization's usage.
	var list struct {
		Data []forecast `json:"data"`
	}
	e.call("GET", "/admin/v1/usage/forecast?org_id="+org.ID, "", 200, &list)
	if len(list.Data) != 3 {
		t.Fatalf("API forecasts: %+v", list.Data)
	}
	viewer := e.session("v@example.com", store.Grant{Role: "viewer", OrgID: "org_other"})
	if rec := e.browser(viewer, "GET", "/admin/v1/usage/forecast?org_id="+org.ID, ""); rec.Code != 403 {
		t.Fatalf("another organization's viewer = %d", rec.Code)
	}
}
