package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

func TestPlayground(t *testing.T) {
	e := newAdminEnv(t, `
guardrails:
  no-pii: {type: pii, detect: [email], action: block, stages: [input]}
`)
	var org, team, other, otherTeam idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"acme","allowed_models":["chat"]}`, 201, &org)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"search","guardrails":["no-pii"]}`, 201, &team)
	e.call("POST", "/admin/v1/orgs", `{"name":"globex"}`, 201, &other)
	e.call("POST", "/admin/v1/orgs/"+other.ID+"/teams", `{"name":"ads"}`, 201, &otherTeam)
	admin := e.session("lead@example.com", store.Grant{Role: config.RoleAdmin, OrgID: org.ID})
	viewer := e.session("analyst@example.com", store.Grant{Role: config.RoleViewer, OrgID: org.ID})
	path := "/admin/v1/playground/chat/completions?team_id=" + team.ID
	msg := func(model, text string) string {
		return `{"model":"` + model + `","messages":[{"role":"user","content":"` + text + `"}]}`
	}

	rec := e.browser(admin, "POST", path, msg("chat", "hello"), "X-Request-Id", "req-play-1")
	if rec.Code != 200 || rec.Header().Get("X-Gateway-Target") != "up/m" || !strings.Contains(rec.Body.String(), `"prompt_tokens":10`) {
		t.Fatalf("playground = %d %v: %s", rec.Code, rec.Header(), rec.Body)
	}
	// The team's policies apply: the organization's allow list and the
	// team's guardrails.
	if rec := e.browser(admin, "POST", path, msg("embed", "hello")); rec.Code != 403 || !strings.Contains(rec.Body.String(), "model_not_allowed") {
		t.Errorf("disallowed model = %d: %s", rec.Code, rec.Body)
	}
	if rec := e.browser(admin, "POST", path, msg("chat", "mail jane@example.com")); rec.Code != 400 || !strings.Contains(rec.Body.String(), "guardrail_blocked") {
		t.Errorf("guardrail = %d: %s", rec.Code, rec.Body)
	}
	// Spending a team's budget needs write access to its organization.
	for _, tc := range []struct {
		who, cookie, team string
	}{{"viewer", viewer, team.ID}, {"admin of another org", admin, otherTeam.ID}, {"nobody", "", team.ID}} {
		rec := e.browser(tc.cookie, "POST", "/admin/v1/playground/chat/completions?team_id="+tc.team, msg("chat", "hello"))
		if rec.Code != 403 && rec.Code != 401 {
			t.Errorf("%s: %d", tc.who, rec.Code)
		}
	}
	if rec := e.browser(admin, "POST", "/admin/v1/playground/chat/completions", msg("chat", "hello")); rec.Code != 400 {
		t.Errorf("no team = %d", rec.Code)
	}

	e.flush()
	audit, _, _ := e.srv.store.ListAudit(context.Background(), org.ID, store.Page{})
	if len(audit) == 0 || audit[0].Action != "playground.chat" || audit[0].Actor != "lead@example.com" || !strings.Contains(string(audit[0].Details), `"request_id":"req`) {
		t.Fatalf("audit = %+v", audit[0])
	}
	rows, err := e.srv.store.QueryUsage(context.Background(), store.UsageQuery{TeamID: team.ID, From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour), GroupBy: "model"})
	if err != nil || len(rows) != 1 || rows[0].Requests != 1 || rows[0].CostUSD == 0 {
		t.Fatalf("metered under the team: %+v, %v", rows, err)
	}
}
