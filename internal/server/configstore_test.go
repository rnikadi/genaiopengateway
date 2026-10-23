package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

func TestConfigObjectsAPI(t *testing.T) {
	e := newAdminEnv(t, `secrets: {encryption_key: "`+testEncryptionKey+`"}`)
	ctx := context.Background()
	const key = "stored-provider-key-123"

	// A provider and a model that uses it.
	var got configEntry
	e.call("PUT", "/admin/v1/config/providers/extra", `{"type": "openai", "base_url": "https://llm.example.com/v1", "api_key": "`+key+`"}`, http.StatusCreated, &got)
	if got.Spec["api_key"] != "set" || got.Source != "store" || got.UpdatedBy != adminActor {
		t.Fatalf("created = %+v", got)
	}
	e.call("PUT", "/admin/v1/config/models/extra-chat", `{"targets": [{"provider": "extra", "model": "m"}]}`, http.StatusCreated, nil)
	cfg := e.srv.rt.Load().cfg
	if cfg.Providers["extra"].APIKey.Value() != key || len(cfg.Models["extra-chat"].Targets) != 1 {
		t.Fatalf("not applied: %+v %+v", cfg.Providers["extra"], cfg.Models["extra-chat"])
	}

	// The secret is encrypted at rest and never returned.
	row, err := e.srv.store.GetConfigObject(ctx, "providers", "extra")
	if err != nil || strings.Contains(row.Spec+row.Secrets, key) {
		t.Fatalf("stored in the clear: %+v, %v", row, err)
	}
	rec := do(e.h, "GET", "/admin/v1/config", adminToken, "")
	if rec.Code != 200 || strings.Contains(rec.Body.String(), key) || strings.Contains(rec.Body.String(), "upstream-key") {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}
	for _, want := range []string{`"name":"up","source":"file"`, `"name":"extra","source":"store"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("list lacks %s: %s", want, rec.Body)
		}
	}

	// An update that leaves the key out, or sends "set", keeps it.
	e.call("PUT", "/admin/v1/config/providers/extra", `{"type": "openai", "base_url": "https://llm2.example.com/v1", "api_key": "set"}`, http.StatusOK, nil)
	e.call("PUT", "/admin/v1/config/providers/extra", `{"type": "openai", "base_url": "https://llm2.example.com/v1", "timeout": "5s"}`, http.StatusOK, nil)
	if p := e.srv.rt.Load().cfg.Providers["extra"]; p.APIKey.Value() != key || p.BaseURL != "https://llm2.example.com/v1" {
		t.Fatalf("after update = %+v", p)
	}

	// Another replica picks the change up on its refresh.
	other, err := New(e.srv.file, slog.New(slog.NewTextHandler(io.Discard, nil)), e.srv.store)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if other.rt.Load().cfg.Providers["extra"].APIKey.Value() != key {
		t.Fatal("second replica did not apply the stored provider")
	}

	// Invalid objects are refused with the config's own messages.
	for _, tc := range []struct{ path, body, want string }{
		{"models/broken", `{"targets": [{"provider": "missing", "model": "m"}]}`, "missing"},
		{"providers/bad", `{"type": "openai", "colour": "blue"}`, "colour"},
		{"providers/Bad Name", `{}`, "name"},
		{"providers/x", `{"type": "openai", "api_key": "set"}`, "no value is stored"},
	} {
		rec := do(e.h, "PUT", "/admin/v1/config/"+strings.ReplaceAll(tc.path, " ", "%20"), adminToken, tc.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("PUT %s = %d %s, want 400 with %q", tc.path, rec.Code, rec.Body, tc.want)
		}
	}
	e.call("PUT", "/admin/v1/config/things/x", `{}`, http.StatusNotFound, nil)
	// The file wins: its names cannot be stored.
	e.call("PUT", "/admin/v1/config/providers/up", `{"type": "openai"}`, http.StatusConflict, nil)

	// A provider in use cannot be deleted; once the model is gone it can.
	e.call("DELETE", "/admin/v1/config/providers/extra", "", http.StatusBadRequest, nil)
	e.call("DELETE", "/admin/v1/config/models/extra-chat", "", http.StatusNoContent, nil)
	e.call("DELETE", "/admin/v1/config/providers/extra", "", http.StatusNoContent, nil)
	e.call("DELETE", "/admin/v1/config/providers/extra", "", http.StatusNotFound, nil)
	if _, ok := e.srv.rt.Load().cfg.Providers["extra"]; ok {
		t.Fatal("deleted provider still running")
	}

	var audit struct{ Data []store.AuditEntry }
	e.call("GET", "/admin/v1/audit", "", 200, &audit)
	var actions []string
	for _, a := range audit.Data {
		actions = append(actions, a.Action)
	}
	if got := strings.Join(actions, ","); !strings.HasPrefix(got, "config.delete,config.delete,config.update,config.update,config.create,config.create") {
		t.Fatalf("audit = %s", got)
	}
}

func TestConfigObjectSecretsInHeaders(t *testing.T) {
	e := newAdminEnv(t, `secrets: {encryption_key: "`+testEncryptionKey+`"}`)
	e.call("PUT", "/admin/v1/config/mcp_servers/docs", `{"url": "https://mcp.example.com/mcp", "headers": {"Authorization": "Bearer one", "X-Team": "two"}}`, http.StatusCreated, nil)
	// Sending the headers without X-Team removes it; Authorization is kept.
	e.call("PUT", "/admin/v1/config/mcp_servers/docs", `{"url": "https://mcp.example.com/mcp", "headers": {"Authorization": "set"}}`, http.StatusOK, nil)
	h := e.srv.rt.Load().cfg.MCP.Servers["docs"].Headers
	if len(h) != 1 || h["Authorization"].Value() != "Bearer one" {
		t.Fatalf("headers = %v", h)
	}
	// Leaving out the headers keeps them.
	e.call("PUT", "/admin/v1/config/mcp_servers/docs", `{"url": "https://mcp.example.com/mcp", "pin_tools": "strict"}`, http.StatusOK, nil)
	if h := e.srv.rt.Load().cfg.MCP.Servers["docs"].Headers; h["Authorization"].Value() != "Bearer one" {
		t.Fatalf("headers after update = %v", h)
	}
}

func TestConfigObjectsRoles(t *testing.T) {
	e := newAdminEnv(t, "")
	viewer := e.session("viewer@example.com", store.Grant{Role: "viewer"})
	for _, c := range []struct{ method, path string }{
		{"GET", "/admin/v1/config"},
		{"PUT", "/admin/v1/config/providers/x"},
		{"DELETE", "/admin/v1/config/providers/x"},
	} {
		if rec := e.browser(viewer, c.method, c.path, `{"type":"openai"}`); rec.Code != http.StatusForbidden {
			t.Errorf("viewer %s %s = %d", c.method, c.path, rec.Code)
		}
	}
	// Without an encryption key, objects without secrets can be stored.
	e.call("PUT", "/admin/v1/config/providers/plain", `{"type": "openai", "base_url": "http://127.0.0.1:1/v1"}`, http.StatusCreated, nil)
	rec := do(e.h, "PUT", "/admin/v1/config/providers/keyed", adminToken, `{"type": "openai", "base_url": "http://127.0.0.1:1/v1", "api_key": "k"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "encryption_key") {
		t.Fatalf("secret without a key = %d %s", rec.Code, rec.Body)
	}
}

// A stored object the file has made invalid does not stop the gateway:
// it starts with the file alone and reports the problem.
func TestConfigObjectsInvalidAtStartup(t *testing.T) {
	e := newAdminEnv(t, "")
	e.call("PUT", "/admin/v1/config/models/extra", `{"targets": [{"provider": "up", "model": "m"}]}`, http.StatusCreated, nil)
	file, err := config.Parse([]byte(`
admin: {token: ` + adminToken + `}
store: {driver: sqlite, dsn: unused}
providers: {other: {base_url: "http://127.0.0.1:1/v1"}}
`))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(file, slog.New(slog.NewTextHandler(io.Discard, nil)), e.srv.store)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := srv.rt.Load().cfg.Models["extra"]; ok || !srv.ready.Load() {
		t.Fatal("expected the file alone, and ready")
	}
	rec := do(srv.Handler(), "GET", "/admin/v1/config", adminToken, "")
	if !strings.Contains(rec.Body.String(), `"error":`) || !strings.Contains(rec.Body.String(), "up") {
		t.Fatalf("list = %s", rec.Body)
	}
}
