package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/server"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

const token = "gogctl-test-admin-token-0000000000000"

// gateway runs a real gateway on SQLite with the admin API on.
func gateway(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Parse([]byte(`admin: {token: ` + token + `}
store: {driver: sqlite, dsn: "file:` + filepath.ToSlash(filepath.Join(dir, "g.db")) + `"}
secrets: {encryption_key: "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="}
`))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), cfg.Store.Driver, cfg.Store.DSN.Value())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv, err := server.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), st)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv.Start(ctx)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs.URL
}

func ctl(t *testing.T, url string, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	if err := run(append([]string{"-url", url, "-token", token}, args...), &out); err != nil {
		t.Fatalf("gogctl %s: %v", strings.Join(args, " "), err)
	}
	return out.String()
}

func TestTenantsAndKeys(t *testing.T) {
	url := gateway(t)
	org := ctl(t, url, "orgs", "create", "acme")
	orgID := field(t, org, "id")
	if !strings.Contains(ctl(t, url, "orgs", "list"), "acme") {
		t.Fatal("orgs list misses acme")
	}
	teamID := field(t, ctl(t, url, "teams", "create", orgID, "search"), "id")
	key := ctl(t, url, "keys", "create", teamID, "prod")
	if !strings.Contains(field(t, key, "key"), "sk-") {
		t.Fatalf("keys create did not print the key: %s", key)
	}
	keyID := field(t, key, "id")
	if !strings.Contains(ctl(t, url, "keys", "list", teamID), "prod") {
		t.Fatal("keys list misses prod")
	}
	rotated := ctl(t, url, "keys", "rotate", keyID, "1h")
	if field(t, rotated, "key") == field(t, key, "key") {
		t.Fatal("rotate returned the old key")
	}
	ctl(t, url, "keys", "revoke", keyID)
	ctl(t, url, "usage", "-org", orgID)
	if !strings.Contains(ctl(t, url, "catalog", "list"), "anthropic") {
		t.Fatal("catalog list misses anthropic")
	}
}

func TestConfigExportImport(t *testing.T) {
	src, dst := gateway(t), gateway(t)
	file := filepath.Join(t.TempDir(), "objects.yaml")
	if err := os.WriteFile(file, []byte(`objects:
  models:
    chat: {targets: [{provider: up, model: up-chat, price: {input_per_mtok: 1, output_per_mtok: 2}}]}
  providers:
    up: {type: openai, base_url: "http://127.0.0.1:9/v1", api_key: secret-value}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := ctl(t, src, "config", "import", file, "-dry-run"); !strings.Contains(out, "would put providers/up") {
		t.Fatalf("dry run: %s", out)
	}
	if out := ctl(t, src, "config", "import", file); !strings.Contains(out, "2 changes applied") {
		t.Fatalf("import: %s", out)
	}
	exported := ctl(t, src, "config", "export")
	if strings.Contains(exported, "secret-value") || !strings.Contains(exported, "api_key: set") || !strings.Contains(exported, "up-chat") {
		t.Fatalf("export leaked a secret or missed an object:\n%s", exported)
	}
	// A second gateway takes the export once its secret is filled in.
	again := filepath.Join(t.TempDir(), "again.yaml")
	if err := os.WriteFile(again, []byte(strings.Replace(exported, "api_key: set", "api_key: other-secret", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	ctl(t, dst, "config", "import", again)
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(ctl(t, dst, "models", "list"), "chat") {
		if time.Now().After(deadline) {
			t.Fatal("imported model never appeared")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// field reads a top-level string field from printed JSON.
func field(t *testing.T, out, name string) string {
	t.Helper()
	i := strings.Index(out, `"`+name+`": "`)
	if i < 0 {
		t.Fatalf("no %s in %s", name, out)
	}
	rest := out[i+len(name)+5:]
	return rest[:strings.Index(rest, `"`)]
}

func TestDeclarativeTenants(t *testing.T) {
	gw := gateway(t)
	write := func(body string) string {
		t.Helper()
		f := filepath.Join(t.TempDir(), "gateway.yaml")
		if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return f
	}
	first := write(`objects:
  providers:
    up: {type: openai, base_url: "http://127.0.0.1:9/v1", api_key: secret-value}
  models:
    chat: {targets: [{provider: up, model: m}]}
orgs:
  acme:
    rpm: 100
    budget: {amount_usd: 50, period: monthly}
    cedar: |
      @id("base")
      permit (principal, action, resource);
    teams:
      search: {allowed_models: [chat]}
      ads: {}
  globex: {}
`)
	if out := ctl(t, gw, "config", "import", first, "-dry-run"); !strings.Contains(out, "would create org acme") || !strings.Contains(out, "would create team acme/search") || !strings.Contains(out, "would update cedar policies of acme") {
		t.Fatalf("dry run:\n%s", out)
	}
	if out := ctl(t, gw, "orgs", "list"); strings.Contains(out, "acme") {
		t.Fatalf("a dry run changed the gateway:\n%s", out)
	}
	if out := ctl(t, gw, "config", "import", first); !strings.Contains(out, "7 changes applied") {
		t.Fatalf("import:\n%s", out)
	}
	// Importing the same file again only puts the provider again, since
	// its secret cannot be compared; importing the export, whose secrets
	// read "set", changes nothing.
	if out := ctl(t, gw, "config", "import", first); !strings.Contains(out, "put providers/up\n1 changes applied") {
		t.Fatalf("second import:\n%s", out)
	}
	exported := ctl(t, gw, "config", "export")
	for _, want := range []string{"acme:", "rpm: 100", "amount_usd: 50", "search:", "- chat", `@id("base")`, "globex: {}"} {
		if !strings.Contains(exported, want) {
			t.Fatalf("export lacks %q:\n%s", want, exported)
		}
	}
	if out := ctl(t, gw, "config", "import", write(exported)); !strings.Contains(out, "nothing to change") {
		t.Fatalf("importing the export:\n%s", out)
	}

	// A smaller file updates what differs; -prune removes the rest.
	second := write(`orgs:
  acme:
    rpm: 200
    teams:
      search: {}
`)
	out := ctl(t, gw, "config", "import", second)
	if !strings.Contains(out, "updated org acme") || !strings.Contains(out, "updated team acme/search") || strings.Contains(out, "deleted") {
		t.Fatalf("update:\n%s", out)
	}
	out = ctl(t, gw, "config", "import", second, "-prune")
	for _, want := range []string{"deleted team acme/ads", "deleted cedar policies of acme", "deleted org globex", "3 changes applied"} {
		if !strings.Contains(out, want) {
			t.Fatalf("prune lacks %q:\n%s", want, out)
		}
	}
	exported = ctl(t, gw, "config", "export")
	if strings.Contains(exported, "budget") || strings.Contains(exported, "globex") || strings.Contains(exported, "ads") || !strings.Contains(exported, "rpm: 200") {
		t.Fatalf("after prune:\n%s", exported)
	}

	// Pruning objects deletes stored ones the file leaves out and keeps
	// organizations, since the file has no orgs section.
	out = ctl(t, gw, "config", "import", write("objects:\n  providers:\n    up: {type: openai, base_url: \"http://127.0.0.1:9/v1\", api_key: set}\n"), "-prune")
	if !strings.Contains(out, "deleted models/chat\n1 changes applied") {
		t.Fatalf("prune objects:\n%s", out)
	}

	if err := run([]string{"-url", gw, "-token", token, "config", "import", write("orgs: {acme: {teams: {x: {bogus: 1}}}}\n")}, io.Discard); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("unknown setting: %v", err)
	}
}
