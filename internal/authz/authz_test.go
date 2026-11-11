package authz

import (
	"slices"
	"testing"
	"time"
)

const doc = `
@id("base")
permit (principal, action, resource);

@id("no-gpt4-for-interns")
forbid (principal in Team::"team_interns", action in Action::"model", resource == Model::"gpt-4o");

@id("office-hours-only")
forbid (principal, action == Action::"call_tool", resource in MCPServer::"github")
unless { context.hour >= 8 && context.hour < 18 };

@id("internal-network")
forbid (principal, action in Action::"model", resource)
when { context has ip && !context.ip.isInRange(ip("10.0.0.0/8")) && principal.name like "internal-*" };
`

func TestDecide(t *testing.T) {
	p, err := Parse(doc)
	if err != nil {
		t.Fatal(err)
	}
	if p.Count() != 4 {
		t.Fatalf("count = %d", p.Count())
	}
	noon := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	night := noon.Add(10 * time.Hour)
	for _, tc := range []struct {
		name string
		r    Request
		ok   bool
		by   []string
	}{
		{"permitted", Request{OrgID: "o", TeamID: "team_eng", KeyID: "k", Action: "chat_completions", Model: "gpt-4o", Time: noon}, true, []string{"base"}},
		{"forbidden model", Request{OrgID: "o", TeamID: "team_interns", KeyID: "k", Action: "chat_completions", Model: "gpt-4o", Time: noon}, false, []string{"no-gpt4-for-interns"}},
		{"other model", Request{OrgID: "o", TeamID: "team_interns", KeyID: "k", Action: "chat_completions", Model: "gpt-4o-mini", Time: noon}, true, []string{"base"}},
		{"tool in hours", Request{OrgID: "o", TeamID: "t", KeyID: "k", Server: "github", Tool: "create_issue", Time: noon}, true, []string{"base"}},
		{"tool at night", Request{OrgID: "o", TeamID: "t", KeyID: "k", Server: "github", Tool: "create_issue", Time: night}, false, []string{"office-hours-only"}},
		{"other server at night", Request{OrgID: "o", TeamID: "t", KeyID: "k", Server: "jira", Tool: "x", Time: night}, true, []string{"base"}},
		{"outside network", Request{OrgID: "o", TeamID: "t", KeyID: "k", KeyName: "internal-ci", Action: "embeddings", Model: "m", IP: "203.0.113.9:4000", Time: noon}, false, []string{"internal-network"}},
		{"inside network", Request{OrgID: "o", TeamID: "t", KeyID: "k", KeyName: "internal-ci", Action: "embeddings", Model: "m", IP: "::ffff:10.1.2.3", Time: noon}, true, []string{"base"}},
	} {
		d := p.Decide(tc.r)
		if d.Allowed != tc.ok || !slices.Equal(d.Policies, tc.by) || len(d.Errors) > 0 {
			t.Errorf("%s: %+v", tc.name, d)
		}
	}
}

func TestDefaultDeny(t *testing.T) {
	p, err := Parse(`forbid (principal, action, resource == Model::"x");`)
	if err != nil {
		t.Fatal(err)
	}
	d := p.Decide(Request{Action: "chat_completions", Model: "y"})
	if d.Allowed || len(d.Policies) != 0 || d.Reason() != "no Cedar policy of this organization permits the request" {
		t.Fatalf("%+v", d)
	}
}

func TestParseErrors(t *testing.T) {
	for _, text := range []string{`permit (principal, action, resource)`, `@id("a") permit (principal, action, resource); @id("a") forbid (principal, action, resource);`} {
		if _, err := Parse(text); err == nil {
			t.Errorf("%q parsed", text)
		}
	}
}
