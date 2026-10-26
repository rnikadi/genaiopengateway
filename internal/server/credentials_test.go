package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

// A team's own provider credential replaces the shared one for its
// requests, and only for them (docs/team-credentials.md).
func TestTeamCredentials(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		status := http.StatusOK
		if r.Header.Get("Authorization") == "Bearer revoked-team-key" {
			status = http.StatusUnauthorized
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1}})
	}))
	t.Cleanup(up.Close)
	last := func() string {
		mu.Lock()
		defer mu.Unlock()
		return seen[len(seen)-1]
	}
	e := newAdminEnvConfig(t, `
admin: {token: `+adminToken+`}
store: {driver: sqlite, dsn: unused}
secrets: {encryption_key: "`+testEncryptionKey+`"}
providers:
  up: {base_url: "`+up.URL+`", api_key: shared-key}
  backup: {base_url: "`+up.URL+`", api_key: backup-key}
models:
  chat: {targets: [{provider: up, model: m}, {provider: backup, model: m}]}
`)
	var org, a, b, keyA, keyB idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"acme"}`, 201, &org)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"a"}`, 201, &a)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"b"}`, 201, &b)
	e.call("POST", "/admin/v1/teams/"+a.ID+"/keys", `{"name":"k"}`, 201, &keyA)
	e.call("POST", "/admin/v1/teams/"+b.ID+"/keys", `{"name":"k"}`, 201, &keyB)

	const teamKey = "team-a-own-key"
	e.call("PUT", "/admin/v1/teams/"+a.ID+"/credentials/up", `{"api_key":"`+teamKey+`"}`, 200, nil)
	chat := func(key string) int {
		return do(e.h, "POST", "/v1/chat/completions", key, `{"model":"chat"}`).Code
	}
	if chat(keyA.Key) != 200 || last() != "Bearer "+teamKey {
		t.Errorf("team a's request carried %q", last())
	}
	if chat(keyB.Key) != 200 || last() != "Bearer shared-key" {
		t.Errorf("team b's request carried %q", last())
	}

	// When the team's key fails, fallback goes to the next target, never to
	// the shared key of the same provider.
	e.call("PUT", "/admin/v1/teams/"+a.ID+"/credentials/up", `{"api_key":"revoked-team-key"}`, 200, nil)
	mu.Lock()
	seen = nil
	mu.Unlock()
	if code := chat(keyA.Key); code != 401 {
		t.Errorf("failed team key = %d", code)
	}
	for _, h := range seen {
		if h == "Bearer shared-key" {
			t.Error("the shared key of the same provider was used")
		}
	}

	// The list shows each provider as set or not, never the secret.
	rec := do(e.h, "GET", "/admin/v1/teams/"+a.ID+"/credentials", adminToken, "")
	var list struct{ Data []credentialEntry }
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if strings.Contains(rec.Body.String(), "revoked-team-key") || len(list.Data) != 2 || !list.Data[1].Set || list.Data[0].Set || !list.Data[0].Supported {
		t.Errorf("list = %s", rec.Body)
	}
	row, _ := e.srv.store.ListTeamCredentials(t.Context(), a.ID)
	if len(row) != 1 || strings.Contains(row[0].Secret, "revoked") {
		t.Fatalf("stored in the clear: %+v", row)
	}

	// Removing it brings the shared key back.
	e.call("DELETE", "/admin/v1/teams/"+a.ID+"/credentials/up", "", 204, nil)
	e.call("DELETE", "/admin/v1/teams/"+a.ID+"/credentials/up", "", 404, nil)
	if chat(keyA.Key) != 200 || last() != "Bearer shared-key" {
		t.Errorf("after removal team a carried %q", last())
	}

	// Refusals, and scope: an organization admin elsewhere cannot change it.
	e.call("PUT", "/admin/v1/teams/"+a.ID+"/credentials/nope", `{"api_key":"x"}`, 400, nil)
	e.call("PUT", "/admin/v1/teams/"+a.ID+"/credentials/up", `{"api_key":""}`, 400, nil)
	var other idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"other"}`, 201, &other)
	lead := e.session("lee@example.com", store.Grant{Role: config.RoleAdmin, OrgID: other.ID})
	if rec := e.browser(lead, "PUT", "/admin/v1/teams/"+a.ID+"/credentials/up", `{"api_key":"x"}`); rec.Code != 403 {
		t.Errorf("another organization's admin = %d", rec.Code)
	}

	audit, _, _ := e.srv.store.ListAudit(t.Context(), org.ID, store.Page{})
	var actions []string
	for _, x := range audit {
		actions = append(actions, x.Action)
		if strings.Contains(string(x.Details), "team-a-own-key") {
			t.Error("the secret is in the audit log")
		}
	}
	if got := strings.Join(actions, " "); !strings.Contains(got, "team.credential.set") || !strings.Contains(got, "team.credential.delete") {
		t.Errorf("audit = %s", got)
	}
}

// Without an encryption key, credentials cannot be stored.
func TestTeamCredentialsNeedEncryptionKey(t *testing.T) {
	e := newAdminEnv(t, "")
	var org, team idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"acme"}`, 201, &org)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"a"}`, 201, &team)
	rec := do(e.h, "PUT", "/admin/v1/teams/"+team.ID+"/credentials/up", adminToken, `{"api_key":"x"}`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "encryption_key") {
		t.Errorf("= %d %s", rec.Code, rec.Body)
	}
}
