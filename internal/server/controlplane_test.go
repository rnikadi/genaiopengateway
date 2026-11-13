package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

const planeToken = "data-plane-token-0123456789abcdef"

func dataPlane(t *testing.T, yaml string) *Server {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestControlAndDataPlanes(t *testing.T) {
	var hits int
	up := upstream(t, 200, &hits)
	e := newAdminEnvConfig(t, `
admin: {token: `+adminToken+`}
store: {driver: sqlite, dsn: unused}
data_planes: {token: `+planeToken+`}
providers: {up: {base_url: "`+up.URL+`", api_key: upstream-key}}
models:
  chat: {targets: [{provider: up, model: m, price: {input_per_mtok: 1, output_per_mtok: 1}}]}
`)
	cp := httptest.NewServer(e.h)
	t.Cleanup(cp.Close)
	var org, team, key, other idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"acme"}`, 201, &org)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"t"}`, 201, &team)
	e.call("POST", "/admin/v1/teams/"+team.ID+"/keys", `{"name":"k"}`, 201, &key)
	e.call("POST", "/admin/v1/teams/"+team.ID+"/keys", `{"name":"k2"}`, 201, &other)
	// A model stored on the control plane reaches the data plane.
	e.call("PUT", "/admin/v1/config/models/stored", `{"targets":[{"provider":"up","model":"m2"}]}`, http.StatusCreated, nil)

	if rec := do(e.h, "GET", "/cp/v1/snapshot", "wrong-token", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a wrong token = %d", rec.Code)
	}
	if rec := do(e.h, "GET", "/cp/v1/snapshot", key.Key, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a gateway key = %d", rec.Code)
	}

	cache := filepath.Join(t.TempDir(), "snapshot.json")
	dpConfig := func(url string) string {
		return `
control_plane: {url: "` + url + `", token: ` + planeToken + `, cache_file: "` + cache + `"}
providers: {up: {base_url: "` + up.URL + `", api_key: upstream-key}}
models:
  chat: {targets: [{provider: up, model: m, price: {input_per_mtok: 1, output_per_mtok: 1}}]}
`
	}
	dp := dataPlane(t, dpConfig(cp.URL))
	h := dp.Handler()
	if dp.ready.Load() {
		t.Fatal("ready before the first snapshot")
	}
	ctx := context.Background()
	if err := dp.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if !dp.ready.Load() {
		t.Fatal("not ready after the first snapshot")
	}
	for _, model := range []string{"chat", "stored"} {
		if rec := do(h, "POST", "/v1/chat/completions", key.Key, `{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`); rec.Code != 200 {
			t.Fatalf("%s through the data plane = %d %s", model, rec.Code, rec.Body)
		}
	}
	// A data plane has no admin API.
	if rec := do(h, "GET", "/admin/v1/orgs", adminToken, ""); rec.Code == 200 {
		t.Fatal("the data plane serves the admin API")
	}
	if fi, err := os.Stat(cache); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("cache file: %v %v", fi, err)
	}

	// Usage goes to the control plane's store.
	dp.Close(ctx)
	rows, err := e.srv.store.QueryUsage(ctx, store.UsageQuery{OrgID: org.ID, From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour), GroupBy: "model"})
	if err != nil || len(rows) != 2 || rows[0].Requests != 1 || rows[0].InputTokens != 10 {
		t.Fatalf("usage on the control plane = %+v, %v", rows, err)
	}

	// A data plane that starts while the control plane is down serves
	// the cached snapshot.
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	cold := dataPlane(t, dpConfig(down.URL))
	if err := cold.refresh(ctx); err != nil {
		t.Fatalf("from the cache: %v", err)
	}
	if rec := do(cold.Handler(), "POST", "/v1/chat/completions", other.Key, `{"model":"chat","messages":[{"role":"user","content":"hi"}]}`); rec.Code != 200 {
		t.Fatalf("from the cache = %d %s", rec.Code, rec.Body)
	}
	if err := cold.refresh(ctx); err == nil {
		t.Fatal("a second refresh with the control plane down reported no error")
	}
	cold.Close(ctx)

	// Revocation reaches the data plane on its next refresh.
	dp2 := dataPlane(t, dpConfig(cp.URL))
	if err := dp2.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	e.call("POST", "/admin/v1/keys/"+key.ID+"/revoke", "", 200, nil)
	if err := dp2.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if rec := do(dp2.Handler(), "POST", "/v1/chat/completions", key.Key, `{"model":"chat","messages":[{"role":"user","content":"hi"}]}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a revoked key = %d", rec.Code)
	}
	dp2.Close(ctx)
}

func TestPlaneConfigValidation(t *testing.T) {
	for name, raw := range map[string]string{
		"short token":        `control_plane: {url: "https://cp", token: short}`,
		"bad url":            `control_plane: {url: "ftp://cp", token: ` + planeToken + `}`,
		"with a store":       `{store: {driver: sqlite, dsn: x}, control_plane: {url: "https://cp", token: ` + planeToken + `}}`,
		"no url":             `control_plane: {token: ` + planeToken + `}`,
		"planes no store":    `data_planes: {token: ` + planeToken + `}`,
		"planes short token": `{store: {driver: sqlite, dsn: x}, data_planes: {token: short}}`,
	} {
		if _, err := config.Parse([]byte(raw)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
