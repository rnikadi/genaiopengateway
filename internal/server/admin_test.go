package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/store"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

const adminToken = "admin-token-0123456789-0123456789-xx"

type adminEnv struct {
	t   *testing.T
	srv *Server
	h   http.Handler
}

func newAdminEnv(t *testing.T, extra string) *adminEnv {
	t.Helper()
	var hits int
	up := upstream(t, 200, &hits)
	cfg, err := config.Parse([]byte(`
admin: {token: ` + adminToken + `}
store: {driver: sqlite, dsn: unused}
providers: {up: {base_url: "` + up.URL + `", api_key: upstream-key}}
models:
  chat: {targets: [{provider: up, model: m, price: {input_per_mtok: 1000000, output_per_mtok: 1000000}}]}
  embed: {targets: [{provider: up, model: e}]}
` + extra))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), "sqlite", "file:"+filepath.Join(t.TempDir(), "gw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), st)
	if err != nil {
		t.Fatal(err)
	}
	return &adminEnv{t: t, srv: srv, h: srv.Handler()}
}

// call makes an admin request and decodes the JSON response into out.
func (e *adminEnv) call(method, path, body string, wantStatus int, out any) {
	e.t.Helper()
	rec := do(e.h, method, path, adminToken, body)
	if rec.Code != wantStatus {
		e.t.Fatalf("%s %s = %d, want %d: %s", method, path, rec.Code, wantStatus, rec.Body)
	}
	if out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			e.t.Fatalf("decode %s: %v", rec.Body, err)
		}
	}
}

type idResp struct {
	ID  string `json:"id"`
	Key string `json:"key"`
}

func TestAdminKeyLifecycle(t *testing.T) {
	e := newAdminEnv(t, "")
	var org, team, key idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"acme","rpm":100}`, 201, &org)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"search","allowed_models":["chat"]}`, 201, &team)
	e.call("POST", "/admin/v1/teams/"+team.ID+"/keys", `{"name":"ci","budget":{"amount_usd":10,"period":"monthly"}}`, 201, &key)
	if !strings.HasPrefix(key.Key, "sk-gog-") {
		t.Fatalf("key secret = %q", key.Key)
	}

	// The new key works at once and inherits the team's allow list.
	if rec := do(e.h, "POST", "/v1/chat/completions", key.Key, `{"model":"chat"}`); rec.Code != 200 {
		t.Fatalf("chat = %d %s", rec.Code, rec.Body)
	}
	if rec := do(e.h, "POST", "/v1/embeddings", key.Key, `{"model":"embed"}`); rec.Code != 403 {
		t.Fatalf("embed = %d, want 403 from the team allow list", rec.Code)
	}

	// Listing never shows the secret.
	rec := do(e.h, "GET", "/admin/v1/teams/"+team.ID+"/keys", adminToken, "")
	if strings.Contains(rec.Body.String(), key.Key) || strings.Contains(rec.Body.String(), "secret_hash") {
		t.Fatalf("secret leaked in list: %s", rec.Body)
	}

	// Revocation applies at once.
	e.call("POST", "/admin/v1/keys/"+key.ID+"/revoke", "", 200, nil)
	if rec := do(e.h, "POST", "/v1/chat/completions", key.Key, `{"model":"chat"}`); rec.Code != 401 {
		t.Fatalf("revoked key = %d", rec.Code)
	}
	e.call("PATCH", "/admin/v1/keys/"+key.ID, `{"name":"x"}`, 409, nil)

	// Usage was recorded for the successful call.
	e.srv.Close(context.Background())
	var usage struct {
		Data []store.UsageRow `json:"data"`
	}
	e.call("GET", "/admin/v1/usage?org_id="+org.ID+"&group_by=model", "", 200, &usage)
	if len(usage.Data) != 1 || usage.Data[0].Group != "chat" || usage.Data[0].Requests != 1 || usage.Data[0].CostUSD != 15 {
		t.Fatalf("usage = %+v", usage.Data)
	}

	var audit struct {
		Data []store.AuditEntry `json:"data"`
	}
	// The $15 request spent the key's $10 budget, which raises its three
	// alerts in the background.
	var changes []string
	alerts := 0
	for deadline := time.Now().Add(5 * time.Second); alerts < 3 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		e.call("GET", "/admin/v1/audit?org_id="+org.ID, "", 200, &audit)
		changes, alerts = nil, 0
		for _, a := range audit.Data {
			if a.Action == "alert.sent" {
				alerts++
			} else {
				changes = append(changes, a.Action)
			}
		}
	}
	if strings.Join(changes, ",") != "key.revoke,key.create,team.create,org.create" || alerts != 3 {
		t.Fatalf("audit = %v and %d alerts", changes, alerts)
	}

	// Deletes respect the hierarchy.
	e.call("DELETE", "/admin/v1/orgs/"+org.ID, "", 409, nil)
	e.call("DELETE", "/admin/v1/teams/"+team.ID, "", 204, nil)
	e.call("DELETE", "/admin/v1/orgs/"+org.ID, "", 204, nil)
	e.call("GET", "/admin/v1/orgs/"+org.ID, "", 404, nil)
}

func TestAdminPatchSemantics(t *testing.T) {
	e := newAdminEnv(t, "")
	var org struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		RPM    int64  `json:"rpm"`
		Budget *struct {
			AmountUSD float64 `json:"amount_usd"`
		} `json:"budget"`
	}
	e.call("POST", "/admin/v1/orgs", `{"name":"a","rpm":5,"budget":{"amount_usd":1,"period":"daily"}}`, 201, &org)
	e.call("PATCH", "/admin/v1/orgs/"+org.ID, `{"name":"b"}`, 200, &org)
	if org.Name != "b" || org.RPM != 5 || org.Budget == nil {
		t.Fatalf("absent fields changed: %+v", org)
	}
	org.Budget = nil
	e.call("PATCH", "/admin/v1/orgs/"+org.ID, `{"budget":null}`, 200, &org)
	if org.Budget != nil || org.RPM != 5 {
		t.Fatalf("null did not clear budget: %+v", org)
	}
}

func TestAdminValidation(t *testing.T) {
	e := newAdminEnv(t, "")
	var org idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"a"}`, 201, &org)
	cases := []struct{ method, path, body string }{
		{"POST", "/admin/v1/orgs", `{}`},
		{"POST", "/admin/v1/orgs", `{"name":"a","unknown":1}`},
		{"POST", "/admin/v1/orgs", `{"name":"a","rpm":-1}`},
		{"POST", "/admin/v1/orgs", `{"name":"a","allowed_models":["nope"]}`},
		{"POST", "/admin/v1/orgs", `{"name":"a","allowed_mcp_servers":["nope"]}`},
		{"POST", "/admin/v1/orgs", `{"name":"a","budget":{"amount_usd":1,"period":"weekly"}}`},
		{"POST", "/admin/v1/orgs", `{"name":"a","expires_at":"2030-01-01T00:00:00Z"}`},
		{"POST", "/admin/v1/orgs", `not json`},
		{"GET", "/admin/v1/orgs?limit=0", ``},
		{"GET", "/admin/v1/usage?group_by=bogus", ``},
		{"GET", "/admin/v1/usage?from=yesterday", ``},
	}
	for _, c := range cases {
		e.call(c.method, c.path, c.body, 400, nil)
	}
	e.call("POST", "/admin/v1/orgs/org_missing/teams", `{"name":"t"}`, 404, nil)
	e.call("GET", "/admin/v1/teams/team_missing/keys", ``, 404, nil)
}

func TestAdminAuth(t *testing.T) {
	e := newAdminEnv(t, "keys: [{name: dev, key: "+devKey+"}]\n")
	if rec := do(e.h, "GET", "/admin/v1/orgs", "", ""); rec.Code != 401 {
		t.Fatalf("no token = %d", rec.Code)
	}
	if rec := do(e.h, "GET", "/admin/v1/orgs", devKey, ""); rec.Code != 401 {
		t.Fatalf("model key on admin API = %d", rec.Code)
	}
	// Without a token configured the admin API does not exist.
	var hits int
	h := gateway(t, basicConfig(upstream(t, 200, &hits).URL), nil)
	if rec := do(h, "GET", "/admin/v1/orgs", adminToken, ""); rec.Code != 404 {
		t.Fatalf("disabled admin API = %d", rec.Code)
	}
}

func TestExpiredKeyRejected(t *testing.T) {
	e := newAdminEnv(t, "")
	var org, team, key idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"a"}`, 201, &org)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"t"}`, 201, &team)
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	e.call("POST", "/admin/v1/teams/"+team.ID+"/keys", `{"name":"k","expires_at":"`+past+`"}`, 201, &key)
	if rec := do(e.h, "POST", "/v1/chat/completions", key.Key, `{"model":"chat"}`); rec.Code != 401 {
		t.Fatalf("expired key = %d", rec.Code)
	}
}

func TestReadyAfterFirstSnapshot(t *testing.T) {
	e := newAdminEnv(t, "")
	if rec := do(e.h, "GET", "/readyz", "", ""); rec.Code != 503 {
		t.Fatalf("readyz before snapshot = %d", rec.Code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.srv.Start(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for do(e.h, "GET", "/readyz", "", "").Code != 200 {
		if time.Now().After(deadline) {
			t.Fatal("never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestBudgetSeededFromStoredUsage(t *testing.T) {
	e := newAdminEnv(t, "")
	var org, team, key idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"a"}`, 201, &org)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"t","budget":{"amount_usd":5,"period":"monthly"}}`, 201, &team)
	e.call("POST", "/admin/v1/teams/"+team.ID+"/keys", `{"name":"k"}`, 201, &key)
	// Spend recorded before this process started, as after a restart.
	err := e.srv.store.InsertUsage(context.Background(), []usage.Record{{
		RequestID: "r", Time: time.Now(), OrgID: org.ID, TeamID: team.ID, KeyID: key.ID,
		Endpoint: "chat.completions", Model: "chat", Provider: "up", UpstreamModel: "m", Status: 200, CostUSD: 6,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if rec := do(e.h, "POST", "/v1/chat/completions", key.Key, `{"model":"chat"}`); rec.Code != 200 {
		t.Fatalf("before seeding = %d", rec.Code)
	}
	e.srv.seedBudgets(context.Background())
	if rec := do(e.h, "POST", "/v1/chat/completions", key.Key, `{"model":"chat"}`); rec.Code != 429 || errorCode(t, rec) != "budget_exceeded" {
		t.Fatalf("after seeding = %d %s", rec.Code, rec.Body)
	}
}
