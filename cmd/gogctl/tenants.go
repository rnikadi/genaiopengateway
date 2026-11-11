package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
)

// Declarative tenants (docs/gogctl.md): an export's orgs section holds
// each organization by name with its policy, its Cedar policies and its
// teams by name with theirs. Import creates what is missing, updates
// what differs, and with -prune deletes what the file leaves out. Keys
// are never part of it, since their secrets are shown only once.

// policyFields are the policy settings an organization or team takes.
var policyFields = []string{"allowed_models", "allowed_mcp_servers", "allowed_agents", "rpm", "tpm", "budget", "log_bodies", "guardrails", "cache", "residency"}

// listFields are the policy settings that are lists; the others clear
// to zero or null.
var listFields = []string{"allowed_models", "allowed_mcp_servers", "allowed_agents", "guardrails", "residency"}

// policyOf keeps the policy settings of an API object, normalized so a
// file and the API compare equal: no empty values, JSON numbers.
func policyOf(m map[string]any) map[string]any {
	out := map[string]any{}
	for _, f := range policyFields {
		v, ok := m[f]
		if !ok || v == nil {
			continue
		}
		if l, ok := v.([]any); ok && len(l) == 0 {
			continue
		}
		raw, _ := json.Marshal(jsonable(v))
		var n any
		_ = json.Unmarshal(raw, &n)
		if n == float64(0) {
			continue
		}
		out[f] = n
	}
	return out
}

// fullPolicy is a PATCH body that sets every policy setting, clearing
// those the file leaves out.
func fullPolicy(p map[string]any) map[string]any {
	body := map[string]any{}
	for _, f := range policyFields {
		switch {
		case p[f] != nil:
			body[f] = p[f]
		case slices.Contains(listFields, f):
			body[f] = []any{}
		case f == "rpm" || f == "tpm":
			body[f] = 0
		default:
			body[f] = nil
		}
	}
	return body
}

// exportTenants reads every organization with its teams and Cedar policies.
func (c *client) exportTenants() (map[string]map[string]any, error) {
	orgs, err := c.all("/admin/v1/orgs")
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]any{}
	for _, o := range orgs {
		id, name := o["id"].(string), o["name"].(string)
		doc := policyOf(o)
		teams, err := c.all("/admin/v1/orgs/" + url.PathEscape(id) + "/teams")
		if err != nil {
			return nil, err
		}
		if len(teams) > 0 {
			td := map[string]any{}
			for _, t := range teams {
				td[t["name"].(string)] = policyOf(t)
			}
			doc["teams"] = td
		}
		cedar, err := c.do(http.MethodGet, "/admin/v1/orgs/"+url.PathEscape(id)+"/cedar", nil)
		if err != nil {
			return nil, err
		}
		if text, _ := cedar["text"].(string); text != "" {
			doc["cedar"] = text
		}
		out[name] = doc
	}
	return out, nil
}

// all reads every page of a list endpoint.
func (c *client) all(path string) ([]map[string]any, error) {
	var items []map[string]any
	after := ""
	for {
		q := url.Values{"limit": {"500"}}
		if after != "" {
			q.Set("after", after)
		}
		out, err := c.do(http.MethodGet, path+"?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		data, _ := out["data"].([]any)
		for _, d := range data {
			if m, ok := d.(map[string]any); ok {
				items = append(items, m)
			}
		}
		more, _ := out["has_more"].(bool)
		if !more || len(data) == 0 {
			return items, nil
		}
		after, _ = items[len(items)-1]["id"].(string)
	}
}

// orgDoc is one organization of a file.
type orgDoc struct {
	policy map[string]any
	teams  map[string]map[string]any
	cedar  *string
}

func parseOrgs(raw map[string]map[string]any) (map[string]orgDoc, error) {
	out := map[string]orgDoc{}
	for name, m := range raw {
		d := orgDoc{policy: map[string]any{}, teams: map[string]map[string]any{}}
		for k, v := range m {
			switch {
			case k == "teams":
				teams, ok := v.(map[string]any)
				if !ok && v != nil {
					return nil, fmt.Errorf("orgs.%s.teams must map team names to policies", name)
				}
				for tn, tv := range teams {
					tm, ok := tv.(map[string]any)
					if !ok && tv != nil {
						return nil, fmt.Errorf("orgs.%s.teams.%s must be a policy", name, tn)
					}
					for f := range tm {
						if !slices.Contains(policyFields, f) {
							return nil, fmt.Errorf("orgs.%s.teams.%s: unknown setting %q", name, tn, f)
						}
					}
					d.teams[tn] = policyOf(tm)
				}
			case k == "cedar":
				s, ok := v.(string)
				if !ok {
					return nil, fmt.Errorf("orgs.%s.cedar must be text", name)
				}
				d.cedar = &s
			case slices.Contains(policyFields, k):
			default:
				return nil, fmt.Errorf("orgs.%s: unknown setting %q", name, k)
			}
		}
		d.policy = policyOf(m)
		out[name] = d
	}
	return out, nil
}

// applier makes changes, or prints them on a dry run.
type applier struct {
	c       *client
	dry     bool
	changes int
}

func (a *applier) do(verb, what, method, path string, body any) (map[string]any, error) {
	if a.dry {
		fmt.Fprintf(a.c.stdout, "would %s %s\n", verb, what)
		a.changes++
		return nil, nil
	}
	out, err := a.c.do(method, path, body)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", verb, what, err)
	}
	done := strings.TrimSuffix(verb, "e") + "ed"
	if verb == "put" {
		done = "put"
	}
	fmt.Fprintf(a.c.stdout, "%s %s\n", done, what)
	a.changes++
	return out, nil
}

// importTenants makes the gateway's organizations match the file's.
func (a *applier) importTenants(want map[string]orgDoc, prune bool) error {
	have, err := a.c.all("/admin/v1/orgs")
	if err != nil {
		return err
	}
	byName := map[string]map[string]any{}
	for _, o := range have {
		byName[strings.ToLower(o["name"].(string))] = o
	}
	for _, name := range slices.Sorted(maps.Keys(want)) {
		d := want[name]
		cur := byName[strings.ToLower(name)]
		delete(byName, strings.ToLower(name))
		var orgID string
		if cur == nil {
			body := fullPolicy(d.policy)
			body["name"] = name
			out, err := a.do("create", "org "+name, http.MethodPost, "/admin/v1/orgs", body)
			if err != nil {
				return err
			}
			if out != nil {
				orgID, _ = out["id"].(string)
			}
		} else {
			orgID = cur["id"].(string)
			if !reflect.DeepEqual(policyOf(cur), d.policy) {
				if _, err := a.do("update", "org "+name, http.MethodPatch, "/admin/v1/orgs/"+url.PathEscape(orgID), fullPolicy(d.policy)); err != nil {
					return err
				}
			}
		}
		if err := a.importTeams(name, orgID, d.teams, prune); err != nil {
			return err
		}
		if err := a.importCedar(name, orgID, d.cedar, prune); err != nil {
			return err
		}
	}
	if prune {
		for _, key := range slices.Sorted(maps.Keys(byName)) {
			o := byName[key]
			id := o["id"].(string)
			if err := a.importTeams(o["name"].(string), id, nil, true); err != nil {
				return err
			}
			if _, err := a.do("delete", "org "+o["name"].(string), http.MethodDelete, "/admin/v1/orgs/"+url.PathEscape(id), nil); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *applier) importTeams(org, orgID string, want map[string]map[string]any, prune bool) error {
	byName := map[string]map[string]any{}
	if orgID != "" {
		have, err := a.c.all("/admin/v1/orgs/" + url.PathEscape(orgID) + "/teams")
		if err != nil {
			return err
		}
		for _, t := range have {
			byName[t["name"].(string)] = t
		}
	}
	for _, name := range slices.Sorted(maps.Keys(want)) {
		p := want[name]
		cur := byName[name]
		delete(byName, name)
		what := "team " + org + "/" + name
		if cur == nil {
			body := fullPolicy(p)
			body["name"] = name
			if _, err := a.do("create", what, http.MethodPost, "/admin/v1/orgs/"+url.PathEscape(orgID)+"/teams", body); err != nil {
				return err
			}
		} else if !reflect.DeepEqual(policyOf(cur), p) {
			if _, err := a.do("update", what, http.MethodPatch, "/admin/v1/teams/"+url.PathEscape(cur["id"].(string)), fullPolicy(p)); err != nil {
				return err
			}
		}
	}
	if prune {
		for _, name := range slices.Sorted(maps.Keys(byName)) {
			if _, err := a.do("delete", "team "+org+"/"+name, http.MethodDelete, "/admin/v1/teams/"+url.PathEscape(byName[name]["id"].(string)), nil); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *applier) importCedar(org, orgID string, want *string, prune bool) error {
	cur := ""
	if orgID != "" {
		out, err := a.c.do(http.MethodGet, "/admin/v1/orgs/"+url.PathEscape(orgID)+"/cedar", nil)
		if err != nil {
			return err
		}
		cur, _ = out["text"].(string)
	}
	path := "/admin/v1/orgs/" + url.PathEscape(orgID) + "/cedar"
	switch {
	case want != nil && strings.TrimSpace(*want) != "" && strings.TrimSpace(*want) != strings.TrimSpace(cur):
		_, err := a.do("update", "cedar policies of "+org, http.MethodPut, path, map[string]any{"text": *want})
		return err
	case cur != "" && ((want != nil && strings.TrimSpace(*want) == "") || (want == nil && prune)):
		_, err := a.do("delete", "cedar policies of "+org, http.MethodDelete, path, nil)
		return err
	}
	return nil
}
