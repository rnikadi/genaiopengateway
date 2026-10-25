package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/rnikadi/genaiopengateway/internal/cache"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

func TestSettingsFromTheUI(t *testing.T) {
	e := newAdminEnv(t, `secrets: {encryption_key: "`+testEncryptionKey+`"}
limits: {max_request_bytes: 2048}
body_logging: {retention: 7d}
`)
	ctx := context.Background()
	fields := func() map[string]settingField {
		var out struct {
			Fields  []settingField `json:"fields"`
			Startup []string       `json:"startup"`
		}
		e.call("GET", "/admin/v1/settings", "", 200, &out)
		if len(out.Startup) == 0 {
			t.Fatal("startup settings are not listed")
		}
		m := map[string]settingField{}
		for _, f := range out.Fields {
			m[f.Path] = f
		}
		return m
	}

	f := fields()
	for path, want := range map[string]settingField{
		"limits.max_request_bytes":    {Value: 2048.0, Default: 33554432.0, Source: "file"},
		"body_logging.retention":      {Value: "7d", Default: "30d", Source: "file"},
		"cache.semantic.threshold":    {Value: 0.95, Default: 0.95, Source: "default"},
		"limiter.fail_open_budgets":   {Value: false, Default: false, Source: "default"},
		"sso.oidc.client_secret":      {Value: nil, Default: nil, Source: "default"},
		"alerts.budget_thresholds":    {Value: []any{0.5, 0.8, 1.0}, Default: []any{0.5, 0.8, 1.0}, Source: "default"},
		"mcp.refresh_interval":        {Value: "1m", Default: "1m", Source: "default"},
		"store.usage_retention":       {Value: "", Default: "", Source: "default"},
		"cache.semantic.max_entries":  {Value: 10000.0, Default: 10000.0, Source: "default"},
		"limits.max_realtime_session": {Value: "30m", Default: "30m", Source: "default"},
	} {
		got, ok := f[path]
		gotJSON, _ := json.Marshal([]any{got.Value, got.Default, got.Source})
		wantJSON, _ := json.Marshal([]any{want.Value, want.Default, want.Source})
		if !ok || string(gotJSON) != string(wantJSON) {
			t.Errorf("%s = %s, want %s", path, gotJSON, wantJSON)
		}
	}

	// A stored setting wins over the file and applies without a restart.
	e.call("PUT", "/admin/v1/config/settings/gateway", `{
		"limits": {"max_request_bytes": 4096},
		"limiter": {"fail_open_rate_limits": false},
		"cache": {"max_memory_bytes": 1000, "semantic": {"max_entries": 5}},
		"store": {"usage_retention": "90d"},
		"alerts": {"budget_thresholds": [0.9]},
		"sso": {"roles": [{"email": "lead@example.com", "role": "admin", "org": "acme"}]}
	}`, http.StatusCreated, nil)
	cfg := e.srv.rt.Load().cfg
	if cfg.Limits.MaxRequestBytes != 4096 || cfg.Limiter.FailOpenRateLimits || cfg.BodyLogging.Retention.Format() != "7d" ||
		cfg.Store.UsageRetention.Format() != "90d" || len(cfg.Alerts.BudgetThresholds) != 1 || len(cfg.SSO.Roles) != 1 {
		t.Fatalf("not applied: limits %+v limiter %+v store %+v alerts %+v roles %+v", cfg.Limits, cfg.Limiter, cfg.Store, cfg.Alerts, cfg.SSO.Roles)
	}
	if e.srv.limiter.FailOpenRate() {
		t.Error("the limiter still fails open")
	}
	m := e.srv.cache.(*cache.Memory)
	_ = m.Set(ctx, "k", &cache.Entry{Body: make([]byte, 2000)}, 0)
	if got, _ := m.Get(ctx, "k"); got != nil {
		t.Error("the memory cache was not resized")
	}
	f = fields()
	if got := f["limits.max_request_bytes"]; got.Source != "stored" || got.Value != 4096.0 {
		t.Errorf("max_request_bytes = %+v", got)
	}
	if got := f["body_logging.retention"]; got.Source != "file" {
		t.Errorf("retention = %+v", got)
	}

	// Leaving a field out resets it to the file's value, then the default.
	e.call("PUT", "/admin/v1/config/settings/gateway", `{"limiter": {"fail_open_rate_limits": false}}`, http.StatusOK, nil)
	if got := e.srv.rt.Load().cfg.Limits.MaxRequestBytes; got != 2048 {
		t.Errorf("after reset max_request_bytes = %d, want the file's 2048", got)
	}
	e.call("DELETE", "/admin/v1/config/settings/gateway", "", http.StatusNoContent, nil)
	if !e.srv.rt.Load().cfg.Limiter.FailOpenRateLimits {
		t.Error("deleting the settings did not restore the default")
	}

	// The SSO client secret is a secret: encrypted, and shown as set.
	const secret = "oidc-client-secret-value"
	e.call("PUT", "/admin/v1/config/settings/gateway", `{"sso": {"oidc": {"issuer": "https://idp.example.com", "client_id": "gw",
		"client_secret": "`+secret+`", "redirect_url": "https://gw.example.com/ui/auth/callback"},
		"roles": [{"group": "ops", "role": "owner"}]}}`, http.StatusCreated, nil)
	if got := e.srv.rt.Load().cfg.SSO.OIDC; got == nil || got.ClientSecret.Value() != secret {
		t.Fatalf("oidc = %+v", got)
	}
	row, err := e.srv.store.GetConfigObject(ctx, "settings", "gateway")
	if err != nil || strings.Contains(row.Spec+row.Secrets, secret) {
		t.Fatalf("stored in the clear: %+v, %v", row, err)
	}
	if rec := do(e.h, "GET", "/admin/v1/settings", adminToken, ""); strings.Contains(rec.Body.String(), secret) {
		t.Fatal("the secret is returned")
	}
	if got := fields()["sso.oidc.client_secret"]; got.Value != true || got.Source != "stored" {
		t.Errorf("client_secret = %+v", got)
	}

	// Startup settings, unknown fields, other names and bad values are refused.
	for _, tc := range []struct{ path, body, want string }{
		{"settings/gateway", `{"store": {"dsn": "postgres://elsewhere"}}`, "dsn"},
		{"settings/gateway", `{"listen": ":9999"}`, "listen"},
		{"settings/gateway", `{"limits": {"max_request_bytes": -1}}`, "max_request_bytes"},
		{"settings/gateway", `{"cache": {"semantic": {"threshold": 2}}}`, "threshold"},
	} {
		rec := do(e.h, "PUT", "/admin/v1/config/"+tc.path, adminToken, tc.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("PUT %s %s = %d %s, want 400 with %q", tc.path, tc.body, rec.Code, rec.Body, tc.want)
		}
	}
	e.call("PUT", "/admin/v1/config/settings/other", `{}`, http.StatusNotFound, nil)

	audit, _, _ := e.srv.store.ListAudit(ctx, "", store.Page{})
	var changed string
	for _, a := range audit {
		if a.Action == "config.update" && a.ResourceType == "settings" {
			changed = string(a.Details)
			break
		}
	}
	if !strings.Contains(changed, `"limits.max_request_bytes"`) {
		t.Errorf("audit of the reset = %s", changed)
	}
}
