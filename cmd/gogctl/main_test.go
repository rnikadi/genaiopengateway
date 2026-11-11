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
	if out := ctl(t, src, "config", "import", file); !strings.Contains(out, "2 objects imported") {
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
