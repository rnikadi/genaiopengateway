package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

// Secret values in the config below; none may appear in any response.
var configSecrets = []string{
	"upstream-key", "sk-bedrock-secret", "hooks-token-path", "guard-hook-secret", "alert-hook-secret",
	"static-key-0123456789", adminToken, testEncryptionKey, "tY9vZ1Jm2fJ0W0Yb0d3VbF3mR2w5xN8kq1yQ7t6u4sE=",
}

const configViewConfig = `
admin: {token: ` + adminToken + `}
store: {driver: sqlite, dsn: unused}
providers:
  up: {base_url: "https://api.example.com/v1", api_key: upstream-key}
  br: {type: bedrock, region: eu-west-1, api_key: sk-bedrock-secret}
  vx: {type: vertex, project: acme-ml, location: global}
models:
  chat: {targets: [{provider: up, model: m}]}
  embed: {targets: [{provider: up, model: e}]}
  claude: {targets: [{provider: br, model: anthropic.claude-sonnet-4-5}, {provider: up, model: m}]}
  gemini: {targets: [{provider: vx, model: gemini-2.5-flash}]}
guardrails:
  no-pii: {type: pii, detect: [email], action: redact, stages: [input, output]}
  policy-hook: {type: webhook, url: "https://guard.example.com/check/hooks-token-path", secret: guard-hook-secret, action: block, stages: [tool_input], timeout: 2s, on_error: allow}
alerts:
  budget_thresholds: [0.5, 0.9]
  webhooks:
    - {name: ops, url: "https://hooks.slack.com/services/hooks-token-path", format: slack, secret: alert-hook-secret, events: [budget.threshold]}
secrets:
  encryption_key: "` + testEncryptionKey + `"
  previous_keys: ["tY9vZ1Jm2fJ0W0Yb0d3VbF3mR2w5xN8kq1yQ7t6u4sE="]
keys:
  - {name: ci, key: static-key-0123456789, rpm: 60}
mcp:
  servers:
    docs: {url: "http://127.0.0.1:1/mcp"}
  virtual_servers:
    tools: {servers: [{server: docs}], guardrails: [policy-hook]}
`

func TestConfigView(t *testing.T) {
	e := newAdminEnvConfig(t, configViewConfig)
	now := time.Now()
	if err := e.srv.store.InsertUsage(context.Background(), []usage.Record{
		{RequestID: "a", Time: now, OrgID: "o", TeamID: "t", KeyID: "k", Endpoint: "messages", Model: "claude", Provider: "br", UpstreamModel: "anthropic.claude-sonnet-4-5", Status: 200, LatencyMS: 100},
		{RequestID: "b", Time: now, OrgID: "o", TeamID: "t", KeyID: "k", Endpoint: "messages", Model: "claude", Provider: "br", UpstreamModel: "anthropic.claude-sonnet-4-5", Status: 429, LatencyMS: 300},
	}); err != nil {
		t.Fatal(err)
	}

	var providers struct {
		Data []struct {
			Name, Type, Region, Project, Location, Auth string
			Models                                      []string
			Health                                      struct {
				Requests, Errors int64
				ErrorRate        float64 `json:"error_rate"`
			}
		}
	}
	e.call("GET", "/admin/v1/providers", "", 200, &providers)
	got := map[string]string{}
	for _, p := range providers.Data {
		got[p.Name] = p.Type + " " + p.Auth + " " + p.Region + p.Project + p.Location + " " + strings.Join(p.Models, ",")
	}
	want := map[string]string{
		"br": "bedrock api_key eu-west-1 claude",
		"vx": "vertex application_default acme-mlglobal gemini",
		"up": "openai api_key  chat,claude,embed",
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("provider %s = %q, want %q", name, got[name], w)
		}
	}
	if h := providers.Data[0].Health; providers.Data[0].Name != "br" || h.Requests != 2 || h.Errors != 1 || h.ErrorRate != 0.5 {
		t.Errorf("bedrock health = %+v", providers.Data[0])
	}

	var models struct {
		Data []struct {
			Name      string
			Endpoints []string
			Targets   []struct {
				Provider     string
				ProviderType string `json:"provider_type"`
				Endpoints    []string
			}
		}
	}
	e.call("GET", "/admin/v1/models", "", 200, &models)
	for _, m := range models.Data {
		if m.Name == "claude" {
			if m.Targets[0].ProviderType != "bedrock" || strings.Join(m.Targets[0].Endpoints, ",") != "chat.completions,messages" ||
				strings.Join(m.Endpoints, ",") != "chat.completions,responses,messages,embeddings" {
				t.Errorf("claude = %+v", m)
			}
		}
		if m.Name == "embed" && strings.Join(m.Endpoints, ",") != "chat.completions,responses,embeddings" {
			t.Errorf("embed endpoints = %v", m.Endpoints)
		}
	}

	var guards struct {
		Data []struct {
			Name, Type, Timeout string
			WebhookHost         string   `json:"webhook_host"`
			VirtualServers      []string `json:"virtual_servers"`
		}
	}
	e.call("GET", "/admin/v1/guardrails", "", 200, &guards)
	if len(guards.Data) != 2 || guards.Data[1].WebhookHost != "https://guard.example.com" || guards.Data[1].Timeout != "2s" ||
		len(guards.Data[1].VirtualServers) != 1 {
		t.Errorf("guardrails = %+v", guards.Data)
	}

	var hooks struct {
		Data []struct {
			Name, Host string
			Signed     bool
		}
		Thresholds []float64 `json:"budget_thresholds"`
	}
	e.call("GET", "/admin/v1/alerts/webhooks", "", 200, &hooks)
	if len(hooks.Data) != 1 || hooks.Data[0].Host != "https://hooks.slack.com" || !hooks.Data[0].Signed || len(hooks.Thresholds) != 2 {
		t.Errorf("webhooks = %+v", hooks)
	}

	var settings struct {
		Version string
		Secrets struct {
			EncryptionKey bool `json:"encryption_key"`
			PreviousKeys  int  `json:"previous_keys"`
		}
		AdminToken bool `json:"admin_token"`
		StaticKeys []struct {
			Name string
			RPM  int64
		} `json:"static_keys"`
	}
	e.call("GET", "/admin/v1/settings", "", 200, &settings)
	if settings.Version == "" || !settings.Secrets.EncryptionKey || settings.Secrets.PreviousKeys != 1 || !settings.AdminToken ||
		len(settings.StaticKeys) != 1 || settings.StaticKeys[0].RPM != 60 {
		t.Errorf("settings = %+v", settings)
	}

	// No secret leaves the gateway.
	for _, path := range []string{"/admin/v1/providers", "/admin/v1/models", "/admin/v1/guardrails", "/admin/v1/alerts/webhooks", "/admin/v1/settings", "/admin/v1/me"} {
		body := do(e.h, "GET", path, adminToken, "").Body.String()
		for _, secret := range configSecrets {
			if strings.Contains(body, secret) {
				t.Errorf("%s leaks %q", path, secret)
			}
		}
		if !json.Valid([]byte(body)) {
			t.Errorf("%s: %s", path, body)
		}
	}
}

func TestDurationFormat(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0: "", 2 * time.Second: "2s", 5 * time.Minute: "5m", 90 * time.Minute: "1h30m", 2 * time.Hour: "2h",
		30 * 24 * time.Hour: "30d", 1500 * time.Millisecond: "1.5s",
	} {
		if got := duration(config.Duration(d)); got != want {
			t.Errorf("duration(%v) = %q, want %q", d, got, want)
		}
	}
}
