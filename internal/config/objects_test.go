package config

import (
	"strings"
	"testing"
)

const objectsBase = `
store: {driver: sqlite, dsn: x}
providers:
  openai: {base_url: "https://api.openai.com/v1", api_key: "${OBJ_TEST_KEY}"}
models:
  chat: {targets: [{provider: openai, model: gpt-4.1-mini}]}
alerts:
  webhooks:
    - {name: ops, url: "https://hooks.example.com/a", events: [budget.threshold]}
`

func TestParseWith(t *testing.T) {
	t.Setenv("OBJ_TEST_KEY", "sk-file")
	objects := []Object{
		{KindProviders, "bedrock", []byte(`{"type":"bedrock","region":"eu-west-1"}`)},
		{KindModels, "claude", []byte(`{"targets":[{"provider":"bedrock","model":"anthropic.claude-sonnet-4-5"}]}`)},
		{KindProviders, "openai", []byte(`{"base_url":"https://proxy.example.com/v1","api_key":"x"}`)},
		{KindGuardrails, "no-pii", []byte(`{"type":"pii","action":"redact","stages":["input"]}`)},
		{KindMCPServers, "docs", []byte(`{"url":"https://mcp.example.com/mcp","headers":{"Authorization":"${OBJ_TEST_KEY}"}}`)},
		{KindMCPVirtualServers, "tools", []byte(`{"servers":[{"server":"docs"}],"guardrails":["no-pii"]}`)},
		{KindAlertWebhooks, "siem", []byte(`{"url":"https://siem.example.com/h","events":["key.expiring"]}`)},
		{KindAlertWebhooks, "ops", []byte(`null`)},
	}
	c, err := ParseWith([]byte(objectsBase), objects)
	if err != nil {
		t.Fatal(err)
	}
	if c.Providers["openai"].BaseURL != "https://proxy.example.com/v1" || c.Providers["openai"].APIKey != "x" {
		t.Errorf("the stored object must replace the file's: %+v", c.Providers["openai"])
	}
	if p := c.Providers["bedrock"]; p.Region != "eu-west-1" || p.Timeout == 0 {
		t.Errorf("stored providers get defaults: %+v", p)
	}
	if c.Models["claude"].Targets[0].Provider != "bedrock" || c.Guardrails["no-pii"].Action != "redact" {
		t.Errorf("models and guardrails: %+v %+v", c.Models, c.Guardrails)
	}
	if got := c.MCP.Servers["docs"].Headers["Authorization"]; got != "${OBJ_TEST_KEY}" {
		t.Errorf("stored values must not be expanded: %q", got)
	}
	if len(c.MCP.VirtualServers["tools"].Servers) != 1 || len(c.Alerts.Webhooks) != 1 || c.Alerts.Webhooks[0].Name != "siem" {
		t.Errorf("mcp and webhooks: %+v %+v", c.MCP, c.Alerts.Webhooks)
	}

	// A deletion marker removes a file object; one for a name the file
	// lacks does nothing. Removing what others use is refused.
	c, err = ParseWith([]byte(objectsBase), []Object{{KindModels, "chat", []byte(`null`)}, {KindGuardrails, "none", []byte(` null`)}})
	if err != nil || len(c.Models) != 0 {
		t.Errorf("deleted model: %+v, %v", c.Models, err)
	}
	if _, err := ParseWith([]byte(objectsBase), []Object{{KindProviders, "openai", []byte(`null`)}}); err == nil || !strings.Contains(err.Error(), `unknown provider "openai"`) {
		t.Errorf("deleting a used provider: %v", err)
	}

	// A file object's spec, as JSON with ${VAR} expanded.
	for _, tc := range []struct{ kind, name, want string }{
		{KindProviders, "openai", `{"api_key":"sk-file","base_url":"https://api.openai.com/v1"}`},
		{KindAlertWebhooks, "ops", `{"events":["budget.threshold"],"url":"https://hooks.example.com/a"}`},
		{KindModels, "nope", ``},
	} {
		got, err := FileSpec([]byte(objectsBase), tc.kind, tc.name)
		if err != nil || string(got) != tc.want {
			t.Errorf("FileSpec(%s, %s) = %s, %v; want %s", tc.kind, tc.name, got, err, tc.want)
		}
	}

	// Stored objects are validated like the file.
	_, err = ParseWith([]byte(objectsBase), []Object{{KindModels, "bad", []byte(`{"targets":[{"provider":"nope","model":"m"}]}`)}})
	if err == nil || !strings.Contains(err.Error(), `model "bad" target 0: unknown provider "nope"`) {
		t.Errorf("invalid object: %v", err)
	}
	if _, err := ParseWith([]byte(objectsBase), []Object{{KindModels, "x", []byte(`[1]`)}}); err == nil {
		t.Error("a non-object spec was accepted")
	}
	// An empty file still takes objects.
	c, err = ParseWith(nil, []Object{{KindProviders, "p", []byte(`{"base_url":"https://x.example.com"}`)}})
	if err != nil || c.Providers["p"].Type != ProviderOpenAI {
		t.Errorf("empty file: %+v, %v", c, err)
	}
}

func TestCheckSpec(t *testing.T) {
	if err := CheckSpec(KindProviders, []byte(`{"type":"openai","base_url":"https://x"}`)); err != nil {
		t.Error(err)
	}
	if err := CheckSpec(KindProviders, []byte(`{"type":"openai","base_ulr":"https://x"}`)); err == nil {
		t.Error("an unknown field was accepted")
	}
	if err := CheckSpec("nope", []byte(`{}`)); err == nil {
		t.Error("an unknown kind was accepted")
	}
	if !IsSecretPath(KindMCPServers, []string{"headers", "Authorization"}) || IsSecretPath(KindMCPServers, []string{"url"}) {
		t.Error("IsSecretPath")
	}
}
