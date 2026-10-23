package config

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestParseDefaultsAndEnv(t *testing.T) {
	t.Setenv("UPSTREAM_KEY", "secret-value")
	c, err := Parse([]byte(`
# a comment mentioning ${NOT_SET} must not break parsing
providers:
  openai: {base_url: "https://api.openai.com/v1/", api_key: "${UPSTREAM_KEY}"}
models:
  chat: {targets: [{provider: openai, model: gpt-x}]}
`))
	if err != nil {
		t.Fatal(err)
	}
	p := c.Providers["openai"]
	if c.Listen != DefaultListen || p.Type != ProviderOpenAI || p.APIKey.Value() != "secret-value" || p.BaseURL != "https://api.openai.com/v1" {
		t.Fatalf("unexpected config: %+v", c)
	}
	if p.Timeout.Std() != DefaultProviderTimeout || c.Limits.MaxRequestBytes != DefaultMaxRequestBytes || c.Limiter.Driver != "memory" || !c.Limiter.FailOpenRateLimits || c.Limiter.FailOpenBudgets {
		t.Fatalf("defaults not applied: %+v", c)
	}
}

func TestParseEmptyFile(t *testing.T) {
	if _, err := Parse(nil); err != nil {
		t.Fatal(err)
	}
}

func TestUnsetEnvIsAnError(t *testing.T) {
	_, err := Parse([]byte(`providers: {p: {base_url: "http://x", api_key: "${SURELY_UNSET_VAR_1}"}}`))
	if err == nil || !strings.Contains(err.Error(), "SURELY_UNSET_VAR_1") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseRejectsBadConfig(t *testing.T) {
	cases := map[string]string{
		"unknown provider":        `models: {chat: {targets: [{provider: nope, model: m}]}}`,
		"no targets":              `models: {chat: {targets: []}}`,
		"empty target model":      `{providers: {p: {base_url: "http://x"}}, models: {chat: {targets: [{provider: p}]}}}`,
		"negative price":          `{providers: {p: {base_url: "http://x"}}, models: {chat: {targets: [{provider: p, model: m, price: {input_per_mtok: -1}}]}}}`,
		"no base url":             `providers: {p: {type: openai}}`,
		"bad base url":            `providers: {p: {base_url: "ftp://x"}}`,
		"bad type":                `providers: {p: {type: magic, base_url: "http://x"}}`,
		"short key":               `keys: [{name: a, key: short}]`,
		"duplicate key name":      `keys: [{name: a, key: aaaaaaaaaaaaaaaaaa}, {name: a, key: bbbbbbbbbbbbbbbbbb}]`,
		"duplicate key value":     `keys: [{name: a, key: aaaaaaaaaaaaaaaaaa}, {name: b, key: aaaaaaaaaaaaaaaaaa}]`,
		"key allows unknown":      `keys: [{name: a, key: aaaaaaaaaaaaaaaaaa, allowed_models: [nope]}]`,
		"bad budget period":       `keys: [{name: a, key: aaaaaaaaaaaaaaaaaa, budget: {amount_usd: 5, period: weekly}}]`,
		"short admin token":       `admin: {token: short}`,
		"bad store":               `store: {driver: mysql, dsn: x}`,
		"store without dsn":       `store: {driver: sqlite}`,
		"valkey no address":       `limiter: {driver: valkey}`,
		"tls half set":            `tls: {cert_file: a.crt}`,
		"bad duration":            `providers: {p: {base_url: "http://x", timeout: soon}}`,
		"unknown field type":      `listen: [1, 2]`,
		"mcp bad url":             `mcp: {servers: {a: {url: "ftp://x"}}}`,
		"mcp bad name":            `mcp: {servers: {"A b": {url: "http://x"}}}`,
		"mcp unknown server":      `mcp: {virtual_servers: {v: {servers: [{server: nope}]}}}`,
		"mcp no servers":          `mcp: {virtual_servers: {v: {servers: []}}}`,
		"mcp same prefix":         `mcp: {servers: {a: {url: "http://x"}, b: {url: "http://y"}}, virtual_servers: {v: {servers: [{server: a, prefix: p}, {server: b, prefix: p}]}}}`,
		"mcp shared no prefix":    `mcp: {servers: {a: {url: "http://x"}, b: {url: "http://y"}}, virtual_servers: {v: {servers: [{server: a, prefix: ""}, {server: b}]}}}`,
		"mcp bad prefix":          `mcp: {servers: {a: {url: "http://x"}}, virtual_servers: {v: {servers: [{server: a, prefix: "a b"}]}}}`,
		"mcp bad pin mode":        `mcp: {servers: {a: {url: "http://x", pin_tools: maybe}}}`,
		"mcp strict no store":     `mcp: {servers: {a: {url: "http://x", pin_tools: strict}}}`,
		"mcp oauth no public url": `{store: {driver: sqlite, dsn: x}, mcp: {oauth: {issuer: "https://id", organization: o, team_claim: g}}}`,
		"mcp oauth no store":      `{mcp: {public_url: "https://gw", oauth: {issuer: "https://id", organization: o, team_claim: g}}}`,
		"mcp oauth no team claim": `{store: {driver: sqlite, dsn: x}, mcp: {public_url: "https://gw", oauth: {issuer: "https://id", organization: o}}}`,
		"mcp oauth bad issuer":    `{store: {driver: sqlite, dsn: x}, mcp: {public_url: "https://gw", oauth: {issuer: "id", organization: o, team_claim: g}}}`,
		"mcp fast refresh":        `mcp: {refresh_interval: 10ms}`,
		"key allows unknown mcp":  `keys: [{name: a, key: aaaaaaaaaaaaaaaaaa, allowed_mcp_servers: [nope]}]`,
		"alert threshold over 1":  `alerts: {budget_thresholds: [0.5, 1.5]}`,
		"alert webhook http":      `{store: {driver: sqlite, dsn: x}, alerts: {webhooks: [{name: a, url: "http://hooks.example.com/x", events: [budget.threshold]}]}}`,
		"alert webhook no events": `{store: {driver: sqlite, dsn: x}, alerts: {webhooks: [{name: a, url: "https://h/x"}]}}`,
		"alert unknown event":     `{store: {driver: sqlite, dsn: x}, alerts: {webhooks: [{name: a, url: "https://h/x", events: [cpu.high]}]}}`,
		"alert bad format":        `{store: {driver: sqlite, dsn: x}, alerts: {webhooks: [{name: a, url: "https://h/x", format: xml, events: [key.expiring]}]}}`,
		"alert duplicate webhook": `{store: {driver: sqlite, dsn: x}, alerts: {webhooks: [{name: a, url: "https://h/x", events: [key.expiring]}, {name: a, url: "https://h/y", events: [key.expiring]}]}}`,
		"alert webhooks no store": `alerts: {webhooks: [{name: a, url: "https://h/x", events: [key.expiring]}]}`,
		"budget alert_at over 1":  `keys: [{name: a, key: aaaaaaaaaaaaaaaaaa, budget: {amount_usd: 5, period: daily, alert_at: [2]}}]`,
		"sso no roles":            `{store: {driver: sqlite, dsn: x}, sso: {oidc: {issuer: "https://id", client_id: c, client_secret: s, redirect_url: "https://gw/ui/auth/callback"}}}`,
		"sso no store":            `{sso: {oidc: {issuer: "https://id", client_id: c, client_secret: s, redirect_url: "https://gw/ui/auth/callback"}, roles: [{group: g, role: owner}]}}`,
		"sso no secret":           `{store: {driver: sqlite, dsn: x}, sso: {oidc: {issuer: "https://id", client_id: c, redirect_url: "https://gw/ui/auth/callback"}, roles: [{group: g, role: owner}]}}`,
		"sso bad redirect":        `{store: {driver: sqlite, dsn: x}, sso: {oidc: {issuer: "https://id", client_id: c, client_secret: s, redirect_url: "https://gw/callback"}, roles: [{group: g, role: owner}]}}`,
		"sso bad role":            `sso: {roles: [{group: g, role: god}]}`,
		"sso owner with org":      `sso: {roles: [{group: g, role: owner, org: acme}]}`,
		"sso admin without org":   `sso: {roles: [{group: g, role: admin}]}`,
		"sso group and email":     `sso: {roles: [{group: g, email: a@b.c, role: viewer}]}`,
		"sso short session":       `sso: {session_ttl: 10s}`,
	}
	for name, raw := range cases {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestSSOConfig(t *testing.T) {
	c, err := Parse([]byte(`
store: {driver: sqlite, dsn: x}
sso:
  oidc: {issuer: "https://id", client_id: c, client_secret: s, redirect_url: "https://gw/ui/auth/callback"}
  roles: [{group: platform, role: owner}, {email: a@example.com, role: viewer, org: acme}]
`))
	if err != nil {
		t.Fatal(err)
	}
	if !c.AdminEnabled() || !c.UIEnabled() || c.SSO.OIDC.GroupsClaim != "groups" || len(c.SSO.OIDC.Scopes) != 3 || c.SSO.SessionTTL.Std() != DefaultSessionTTL {
		t.Fatalf("defaults: %+v %+v", c.SSO, c.SSO.OIDC)
	}
	c, _ = Parse([]byte(`{store: {driver: sqlite, dsn: x}}`))
	if c.AdminEnabled() {
		t.Fatal("the admin API needs a token or SSO")
	}
}

func TestAlertsConfig(t *testing.T) {
	c, err := Parse([]byte(`
store: {driver: sqlite, dsn: x}
alerts:
  webhooks:
    - {name: dev, url: "http://localhost:9000/hook", events: [budget.threshold]}
    - {name: slack, url: "https://hooks.slack.com/services/x", format: slack, events: [mcp.tool_pending]}
`))
	if err != nil {
		t.Fatal(err)
	}
	a := c.Alerts
	if len(a.BudgetThresholds) != 3 || a.Webhooks[0].Format != FormatJSON || !a.Webhooks[1].Wants(EventMCPToolPending) || a.Webhooks[1].Wants(EventBudgetThreshold) {
		t.Fatalf("alerts = %+v", a)
	}
}

func TestReportsAllErrorsAtOnce(t *testing.T) {
	_, err := Parse([]byte(`{keys: [{name: a, key: x}], store: {driver: mysql}}`))
	if err == nil || strings.Count(err.Error(), "\n") < 1 {
		t.Fatalf("expected several errors, got %v", err)
	}
}

func TestDurationDays(t *testing.T) {
	for in, want := range map[string]time.Duration{"90d": 90 * 24 * time.Hour, "30s": 30 * time.Second, "1h30m": 90 * time.Minute} {
		if got, err := ParseDuration(in); err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := ParseDuration("-1d"); err == nil {
		t.Error("negative days accepted")
	}
}

func TestSecretNeverPrints(t *testing.T) {
	s := Secret("hunter2-hunter2")
	for _, out := range []string{s.String(), fmt.Sprint(s), fmt.Sprintf("%v", struct{ S Secret }{s})} {
		if strings.Contains(out, "hunter2") {
			t.Fatalf("secret printed: %s", out)
		}
	}
}

func TestExampleConfigIsValid(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	t.Setenv("GATEWAY_DEV_KEY", "dev-key-0123456789")
	raw, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(raw); err != nil {
		t.Fatal(err)
	}

	// The commented-out sections must be valid once uncommented.
	t.Setenv("GATEWAY_ADMIN_TOKEN", strings.Repeat("a", 32))
	t.Setenv("GITHUB_MCP_TOKEN", "ghp_test")
	section := regexp.MustCompile(`^# ( |[a-z_]+:)`)
	lines := strings.Split(string(raw), "\n")
	for i, l := range lines {
		if section.MatchString(l) {
			lines[i] = l[2:]
		}
	}
	c, err := Parse([]byte(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatalf("uncommented example: %v", err)
	}
	if c.Store.Driver != "sqlite" || len(c.MCP.VirtualServers) != 1 {
		t.Fatalf("uncommented example did not enable store and MCP: %+v", c)
	}
}

func TestComposeConfigIsValid(t *testing.T) {
	for _, v := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY"} {
		t.Setenv(v, "")
	}
	t.Setenv("GATEWAY_ADMIN_TOKEN", strings.Repeat("a", 32))
	t.Setenv("GATEWAY_DEV_KEY", "dev-key-0123456789")
	t.Setenv("OLLAMA_MODEL", "qwen2.5:0.5b")
	raw, err := os.ReadFile("../../deploy/compose/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(raw); err != nil {
		t.Fatal(err)
	}
	// The SSO variant is config.yaml plus an sso section, so the two
	// cannot drift apart.
	sso, err := os.ReadFile("../../deploy/compose/config.sso.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(sso, raw) {
		t.Fatal("config.sso.yaml must start with the whole of config.yaml; regenerate it")
	}
	c, err := Parse(sso)
	if err != nil {
		t.Fatal(err)
	}
	if c.SSO.OIDC == nil || len(c.SSO.Roles) != 3 {
		t.Fatalf("config.sso.yaml does not configure sign-in: %+v", c.SSO)
	}
}
