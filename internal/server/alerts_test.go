package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/alerts"
	"github.com/rnikadi/genaiopengateway/internal/limits"
	"github.com/rnikadi/genaiopengateway/internal/policy"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

func TestBudgetCrossings(t *testing.T) {
	b := func(alertAt []float64) policy.Budget {
		return policy.Budget{AmountUSD: 100, Period: policy.PeriodMonthly, AlertAt: alertAt}
	}
	defaults := []float64{0.5, 0.8, 1}
	for _, tc := range []struct {
		name          string
		budget        policy.Budget
		before, after float64
		want          []float64
	}{
		{"below", b(nil), 10, 49.99, nil},
		{"reaches exactly", b(nil), 10, 50, []float64{0.5}},
		{"already past", b(nil), 50, 60, nil},
		{"several at once", b(nil), 40, 120, []float64{0.5, 0.8, 1}},
		{"own thresholds", b([]float64{0.9}), 40, 95, []float64{0.9}},
		{"alerts off", b([]float64{}), 40, 120, nil},
		{"no budget amount", policy.Budget{AmountUSD: 0, Period: policy.PeriodDaily}, 0, 1, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := budgetCrossings([]limits.Spend{{Budget: tc.budget, Before: tc.before, After: tc.after}}, defaults)[0]
			if len(got) != len(tc.want) {
				t.Fatalf("crossings = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("crossings = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// hookReceiver records webhook deliveries.
type hookReceiver struct {
	mu      sync.Mutex
	bodies  [][]byte
	headers []http.Header
	got     chan struct{}
}

func newHookReceiver(t *testing.T) (*hookReceiver, string) {
	h := &hookReceiver{got: make(chan struct{}, 100)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.bodies, h.headers = append(h.bodies, b), append(h.headers, r.Header.Clone())
		h.mu.Unlock()
		h.got <- struct{}{}
	}))
	t.Cleanup(srv.Close)
	return h, srv.URL
}

func (h *hookReceiver) wait(t *testing.T, n int) []map[string]any {
	t.Helper()
	for range n {
		select {
		case <-h.got:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %d webhook deliveries", n)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []map[string]any
	for _, b := range h.bodies {
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		out = append(out, m)
	}
	return out
}

func TestBudgetAlerts(t *testing.T) {
	ops, opsURL := newHookReceiver(t)
	other, otherURL := newHookReceiver(t)
	e := newAdminEnv(t, `
alerts:
  webhooks:
    - {name: ops, url: "`+opsURL+`", secret: hook-secret, events: [budget.threshold, key.expiring]}
    - {name: elsewhere, url: "`+otherURL+`", events: [budget.threshold], org: someone-else}
`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.srv.notifier.Run(ctx)

	var org, team, key idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"acme"}`, 201, &org)
	// Every chat request costs $15 at the test prices.
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"search","budget":{"amount_usd":40,"period":"monthly"}}`, 201, &team)
	e.call("POST", "/admin/v1/teams/"+team.ID+"/keys", `{"name":"ci"}`, 201, &key)
	for i, want := range []int{200, 200, 200, 429} {
		if rec := do(e.h, "POST", "/v1/chat/completions", key.Key, `{"model":"chat"}`); rec.Code != want {
			t.Fatalf("request %d = %d, want %d: %s", i+1, rec.Code, want, rec.Body)
		}
	}

	// $15 and $30 cross 50%; $45 crosses 80% and 100%.
	got := ops.wait(t, 3)
	thresholds := map[float64]bool{}
	for _, m := range got {
		data := m["data"].(map[string]any)
		thresholds[data["threshold"].(float64)] = true
		if m["event"] != "budget.threshold" || data["scope"] != "team" || data["budget_usd"] != 40.0 ||
			m["org"].(map[string]any)["name"] != "acme" || m["team"].(map[string]any)["name"] != "search" || m["key"] != nil {
			t.Fatalf("alert = %v", m)
		}
	}
	if len(thresholds) != 3 || !thresholds[0.5] || !thresholds[0.8] || !thresholds[1] {
		t.Fatalf("thresholds = %v", thresholds)
	}
	ops.mu.Lock()
	for i, b := range ops.bodies {
		sig := ops.headers[i].Get(alerts.SignatureHeader)
		ts, _, _ := strings.Cut(strings.TrimPrefix(sig, "t="), ",")
		unix, _ := strconv.ParseInt(ts, 10, 64)
		if sig != alerts.Sign("hook-secret", time.Unix(unix, 0), b) {
			t.Fatalf("delivery %d has a bad signature %q", i, sig)
		}
	}
	ops.mu.Unlock()
	select {
	case <-other.got:
		t.Fatal("a webhook for another organization got the alert")
	case <-time.After(50 * time.Millisecond):
	}

	// The same thresholds do not fire again, here or on another replica.
	p, _ := e.srv.keys.Load().Lookup(key.Key)
	e.srv.budgetAlerts(context.Background(), p, []limits.Spend{{
		Level: 1, ScopeID: team.ID, Budget: policy.Budget{AmountUSD: 40, Period: policy.PeriodMonthly},
		Before: 10, After: 45, PeriodStart: limits.PeriodStart(policy.PeriodMonthly, time.Now()),
	}})
	select {
	case <-ops.got:
		t.Fatal("an alert fired twice")
	case <-time.After(100 * time.Millisecond):
	}

	var list struct {
		Data []store.Alert `json:"data"`
	}
	e.call("GET", "/admin/v1/alerts?org_id="+org.ID, "", 200, &list)
	if len(list.Data) != 3 {
		t.Fatalf("listed alerts = %+v", list.Data)
	}
	var audit struct {
		Data []store.AuditEntry `json:"data"`
	}
	e.call("GET", "/admin/v1/audit?org_id="+org.ID, "", 200, &audit)
	sent := 0
	for _, a := range audit.Data {
		if a.Action == "alert.sent" {
			sent++
		}
	}
	if sent != 3 {
		t.Fatalf("audit has %d alert.sent entries, want 3", sent)
	}
}

func TestKeyExpiryAndToolAlerts(t *testing.T) {
	ops, opsURL := newHookReceiver(t)
	e := newAdminEnv(t, `
alerts:
  webhooks:
    - {name: ops, url: "`+opsURL+`", format: slack, events: [key.expiring, mcp.tool_pending]}
`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.srv.notifier.Run(ctx)

	var org, team, soon, later idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"acme"}`, 201, &org)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"search"}`, 201, &team)
	in3days := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
	in30days := time.Now().Add(720 * time.Hour).UTC().Format(time.RFC3339)
	e.call("POST", "/admin/v1/teams/"+team.ID+"/keys", `{"name":"soon","expires_at":"`+in3days+`"}`, 201, &soon)
	e.call("POST", "/admin/v1/teams/"+team.ID+"/keys", `{"name":"later","expires_at":"`+in30days+`"}`, 201, &later)

	e.srv.checkKeyExpiry(ctx, time.Now())
	e.srv.checkKeyExpiry(ctx, time.Now())
	got := ops.wait(t, 1)
	if text, _ := got[0]["text"].(string); !strings.Contains(text, "Key *soon* of acme / search expires on") {
		t.Fatalf("slack text = %v", got[0])
	}

	e.srv.toolPending("github", "search", "h1", true)
	e.srv.toolPending("github", "search", "h1", true)
	got = ops.wait(t, 1)
	if text, _ := got[1]["text"].(string); !strings.Contains(text, "github/search* changed its definition") {
		t.Fatalf("slack text = %v", got[1])
	}
	select {
	case <-ops.got:
		t.Fatal("an alert fired twice")
	case <-time.After(100 * time.Millisecond):
	}

	var res map[string]any
	e.call("POST", "/admin/v1/alerts/test", `{"webhook":"ops"}`, 200, &res)
	if res["ok"] != true || res["status"] != 200.0 {
		t.Fatalf("test delivery = %v", res)
	}
	e.call("POST", "/admin/v1/alerts/test", `{"webhook":"nope"}`, 400, nil)
}
