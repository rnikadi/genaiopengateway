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
		{KindProviders, "openai", []byte(`{"base_url":"https://evil.example.com","api_key":"x"}`)},
		{KindGuardrails, "no-pii", []byte(`{"type":"pii","action":"redact","stages":["input"]}`)},
		{KindMCPServers, "docs", []byte(`{"url":"https://mcp.example.com/mcp","headers":{"Authorization":"${OBJ_TEST_KEY}"}}`)},
		{KindMCPVirtualServers, "tools", []byte(`{"servers":[{"server":"docs"}],"guardrails":["no-pii"]}`)},
		{KindAlertWebhooks, "siem", []byte(`{"url":"https://siem.example.com/h","events":["key.expiring"]}`)},
		{KindAlertWebhooks, "ops", []byte(`{"url":"https://evil.example.com","events":["key.expiring"]}`)},
	}
	c, shadowed, err := ParseWith([]byte(objectsBase), objects)
	if err != nil {
		t.Fatal(err)
	}
	if len(shadowed) != 2 || shadowed[0].Name != "openai" || shadowed[1].Name != "ops" {
		t.Fatalf("shadowed = %+v", shadowed)
	}
	if c.Providers["openai"].BaseURL != "https://api.openai.com/v1" || c.Providers["openai"].APIKey != "sk-file" {
		t.Errorf("the file must win: %+v", c.Providers["openai"])
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
	if len(c.MCP.VirtualServers["tools"].Servers) != 1 || len(c.Alerts.Webhooks) != 2 || c.Alerts.Webhooks[1].Name != "siem" {
		t.Errorf("mcp and webhooks: %+v %+v", c.MCP, c.Alerts.Webhooks)
	}

	// Stored objects are validated like the file.
	_, _, err = ParseWith([]byte(objectsBase), []Object{{KindModels, "bad", []byte(`{"targets":[{"provider":"nope","model":"m"}]}`)}})
	if err == nil || !strings.Contains(err.Error(), `model "bad" target 0: unknown provider "nope"`) {
		t.Errorf("invalid object: %v", err)
	}
	if _, _, err := ParseWith([]byte(objectsBase), []Object{{KindModels, "x", []byte(`[1]`)}}); err == nil {
		t.Error("a non-object spec was accepted")
	}
	// An empty file still takes objects.
	c, _, err = ParseWith(nil, []Object{{KindProviders, "p", []byte(`{"base_url":"https://x.example.com"}`)}})
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
