// Package authz evaluates an organization's Cedar policies (ADR 0009,
// docs/cedar.md) against a model request or an MCP tool call.
//
// The principal is the API key, Key::"<key id>", a member of its team,
// Team::"<team id>", which is a member of Org::"<org id>". The action is
// Action::"<endpoint>" for a model request, in the group Action::"model",
// or Action::"call_tool" in the group Action::"mcp". The resource is
// Model::"<model>" or Tool::"<server>/<tool>", a member of
// MCPServer::"<server>".
package authz

import (
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/cedar-policy/cedar-go"
	"github.com/cedar-policy/cedar-go/types"
)

// Policies is a parsed set of an organization's policies.
type Policies struct {
	Version int64
	set     *cedar.PolicySet
}

// Parse reads policies in the Cedar language. Policies without an @id
// annotation are named policy0, policy1, ... in order.
func Parse(text string) (*Policies, error) {
	set, err := cedar.NewPolicySetFromBytes("policies.cedar", []byte(text))
	if err != nil {
		return nil, err
	}
	// Name policies by their @id annotation, so a denial can say which.
	named := cedar.NewPolicySet()
	for id, p := range set.All() {
		if v, ok := p.Annotations()["id"]; ok && v != "" {
			id = cedar.PolicyID(v)
		}
		if !named.Add(id, p) {
			return nil, fmt.Errorf("two policies have the id %q", id)
		}
	}
	return &Policies{set: named}, nil
}

// Count is how many policies the set holds.
func (p *Policies) Count() int {
	n := 0
	for range p.set.All() {
		n++
	}
	return n
}

// Request is what a decision is made about.
type Request struct {
	OrgID, TeamID, KeyID string
	// KeyName and TeamName are attributes of the principal.
	KeyName, TeamName string
	// Action is the endpoint of a model request, or "call_tool".
	Action string
	// Model is the requested model; Server and Tool name an MCP tool.
	Model, Server, Tool string
	// User is the end user the caller names, if any.
	User   string
	IP     string
	Stream bool
	Time   time.Time
}

// Decision is the outcome, with the policies that decided it.
type Decision struct {
	Allowed bool
	// Policies are the ids of the policies that permitted the request
	// or, when it is denied, forbade it; none when nothing permitted it.
	Policies []string
	// Errors are policies that failed to evaluate, which Cedar skips.
	Errors []string
}

// Reason explains a denial in a sentence.
func (d Decision) Reason() string {
	if len(d.Policies) == 0 {
		return "no Cedar policy of this organization permits the request"
	}
	return "forbidden by the Cedar policy " + strings.Join(d.Policies, ", ")
}

// Decide evaluates the request: Cedar denies unless a policy permits and
// none forbids.
func (p *Policies) Decide(r Request) Decision {
	key := cedar.NewEntityUID("Key", cedar.String(r.KeyID))
	team := cedar.NewEntityUID("Team", cedar.String(r.TeamID))
	org := cedar.NewEntityUID("Org", cedar.String(r.OrgID))
	entities := cedar.EntityMap{
		key: {UID: key, Parents: cedar.NewEntityUIDSet(team), Attributes: cedar.NewRecord(cedar.RecordMap{
			"name": cedar.String(r.KeyName),
		})},
		team: {UID: team, Parents: cedar.NewEntityUIDSet(org), Attributes: cedar.NewRecord(cedar.RecordMap{
			"name": cedar.String(r.TeamName),
		})},
		org: {UID: org},
	}
	var action, resource cedar.EntityUID
	if r.Tool != "" {
		action = cedar.NewEntityUID("Action", "call_tool")
		group := cedar.NewEntityUID("Action", "mcp")
		entities[action] = cedar.Entity{UID: action, Parents: cedar.NewEntityUIDSet(group)}
		server := cedar.NewEntityUID("MCPServer", cedar.String(r.Server))
		resource = cedar.NewEntityUID("Tool", cedar.String(r.Server+"/"+r.Tool))
		entities[server] = cedar.Entity{UID: server}
		entities[resource] = cedar.Entity{UID: resource, Parents: cedar.NewEntityUIDSet(server), Attributes: cedar.NewRecord(cedar.RecordMap{
			"name": cedar.String(r.Tool), "server": cedar.String(r.Server),
		})}
	} else {
		action = cedar.NewEntityUID("Action", cedar.String(r.Action))
		group := cedar.NewEntityUID("Action", "model")
		entities[action] = cedar.Entity{UID: action, Parents: cedar.NewEntityUIDSet(group)}
		resource = cedar.NewEntityUID("Model", cedar.String(r.Model))
		entities[resource] = cedar.Entity{UID: resource, Attributes: cedar.NewRecord(cedar.RecordMap{
			"name": cedar.String(r.Model),
		})}
	}
	t := r.Time
	if t.IsZero() {
		t = time.Now()
	}
	t = t.UTC()
	ctx := cedar.RecordMap{
		"stream":  cedar.Boolean(r.Stream),
		"hour":    cedar.Long(t.Hour()),
		"weekday": cedar.String(strings.ToLower(t.Weekday().String())),
		"user":    cedar.String(r.User),
	}
	if ip := normalizeIP(r.IP); ip != "" {
		if v, err := types.ParseIPAddr(ip); err == nil {
			ctx["ip"] = v
		}
	}
	ok, diag := cedar.Authorize(p.set, entities, cedar.Request{
		Principal: key, Action: action, Resource: resource, Context: cedar.NewRecord(ctx),
	})
	d := Decision{Allowed: ok == cedar.Allow}
	for _, reason := range diag.Reasons {
		d.Policies = append(d.Policies, string(reason.PolicyID))
	}
	for _, e := range diag.Errors {
		d.Errors = append(d.Errors, fmt.Sprintf("%s: %s", e.PolicyID, e.Message))
	}
	slices.Sort(d.Policies)
	return d
}

// normalizeIP strips a port and turns an IPv4-mapped IPv6 address into
// IPv4, which Cedar's ip() requires.
func normalizeIP(s string) string {
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return ""
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.String()
}
