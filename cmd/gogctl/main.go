// Command gogctl manages a GenAIOpenGateway over its admin API: tenants,
// keys, usage, and configuration export and import (docs/gogctl.md).
//
//	export GOG_URL=http://localhost:8080 GOG_ADMIN_TOKEN=...
//	gogctl orgs create acme
//	gogctl keys create <team-id> prod
//	gogctl config export > gateway-objects.yaml
//	gogctl config import gateway-objects.yaml
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"gopkg.in/yaml.v3"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `gogctl manages a GenAIOpenGateway over its admin API.

Usage: gogctl [-url URL] [-token TOKEN] [-o table|json] <command> [arguments]

Commands:
  orgs list
  orgs create NAME
  orgs delete ORG_ID
  teams list ORG_ID
  teams create ORG_ID NAME
  teams delete TEAM_ID
  keys list [TEAM_ID]
  keys create TEAM_ID NAME          prints the key once
  keys revoke KEY_ID
  keys rotate KEY_ID [GRACE]        GRACE such as 24h; prints the new key once
  usage [-org ORG_ID] [-group-by model|team|key|provider|endpoint|day] [-from RFC3339] [-to RFC3339]
  models list
  providers list
  catalog list
  config export [-all]              stored objects and organizations as YAML; -all adds the config file's objects
  config import FILE [-dry-run] [-prune]
                                    makes the gateway match FILE: creates or updates what
                                    differs; -prune deletes what FILE's sections leave out
  operator [-namespace NS] [-interval 15s] [-once] [-kube-url URL]
                                    reconciles GatewayOrganization and GatewayConfigObject
                                    resources from Kubernetes into the gateway
  version

The URL and token come from -url and -token, or GOG_URL and
GOG_ADMIN_TOKEN. The URL defaults to http://localhost:8080.
`

type client struct {
	base   string
	token  string
	output string
	http   *http.Client
	stdout io.Writer
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "gogctl:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("gogctl", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	base := fs.String("url", envOr("GOG_URL", "http://localhost:8080"), "gateway URL")
	token := fs.String("token", os.Getenv("GOG_ADMIN_TOKEN"), "admin token")
	output := fs.String("o", "table", "output: table or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return errors.New("no command")
	}
	if rest[0] == "version" {
		fmt.Fprintln(stdout, version)
		return nil
	}
	if *token == "" {
		return errors.New("set GOG_ADMIN_TOKEN or -token")
	}
	c := &client{base: strings.TrimRight(*base, "/"), token: *token, output: *output, http: &http.Client{Timeout: 60 * time.Second}, stdout: stdout}
	cmd := strings.Join(rest[:min(2, len(rest))], " ")
	a := rest[min(2, len(rest)):]
	switch cmd {
	case "orgs list":
		return c.list("/admin/v1/orgs?limit=500", []string{"id", "name", "created_at"})
	case "orgs create":
		return c.create("/admin/v1/orgs", a, 1, func() any { return map[string]any{"name": a[0]} })
	case "orgs delete":
		return c.del("/admin/v1/orgs/", a)
	case "teams list":
		if len(a) != 1 {
			return errors.New("usage: teams list ORG_ID")
		}
		return c.list("/admin/v1/orgs/"+url.PathEscape(a[0])+"/teams?limit=500", []string{"id", "name", "created_at"})
	case "teams create":
		if len(a) != 2 {
			return errors.New("usage: teams create ORG_ID NAME")
		}
		return c.create("/admin/v1/orgs/"+url.PathEscape(a[0])+"/teams", a, 2, func() any { return map[string]any{"name": a[1]} })
	case "teams delete":
		return c.del("/admin/v1/teams/", a)
	case "keys list":
		path := "/admin/v1/keys?limit=500"
		if len(a) == 1 {
			path = "/admin/v1/teams/" + url.PathEscape(a[0]) + "/keys?limit=500"
		}
		return c.list(path, []string{"id", "name", "prefix", "team_id", "owner", "expires_at", "revoked_at"})
	case "keys create":
		if len(a) != 2 {
			return errors.New("usage: keys create TEAM_ID NAME")
		}
		return c.create("/admin/v1/teams/"+url.PathEscape(a[0])+"/keys", a, 2, func() any { return map[string]any{"name": a[1]} })
	case "keys revoke":
		if len(a) != 1 {
			return errors.New("usage: keys revoke KEY_ID")
		}
		return c.print(c.do(http.MethodPost, "/admin/v1/keys/"+url.PathEscape(a[0])+"/revoke", map[string]any{}))
	case "keys rotate":
		if len(a) < 1 || len(a) > 2 {
			return errors.New("usage: keys rotate KEY_ID [GRACE]")
		}
		body := map[string]any{}
		if len(a) == 2 {
			body["grace"] = a[1]
		}
		return c.print(c.do(http.MethodPost, "/admin/v1/keys/"+url.PathEscape(a[0])+"/rotate", body))
	case "models list":
		return c.list("/admin/v1/models", []string{"name", "endpoints"})
	case "providers list":
		return c.list("/admin/v1/providers", []string{"name", "type", "base_url", "models"})
	case "catalog list":
		return c.catalog()
	case "config export":
		ef := flag.NewFlagSet("config export", flag.ContinueOnError)
		all := ef.Bool("all", false, "include objects from the config file")
		if err := ef.Parse(a); err != nil {
			return err
		}
		return c.export(*all)
	case "config import":
		imf := flag.NewFlagSet("config import", flag.ContinueOnError)
		dry := imf.Bool("dry-run", false, "print what would change without changing it")
		prune := imf.Bool("prune", false, "delete stored objects, organizations and teams the file leaves out")
		if err := imf.Parse(reorder(a)); err != nil {
			return err
		}
		if imf.NArg() != 1 {
			return errors.New("usage: config import FILE [-dry-run] [-prune]")
		}
		return c.importFile(imf.Arg(0), *dry, *prune)
	}
	if rest[0] == "usage" {
		return c.usage(rest[1:])
	}
	if rest[0] == "operator" {
		return c.runOperator(rest[1:])
	}
	fs.Usage()
	return fmt.Errorf("unknown command %q", cmd)
}

// reorder puts flags before positional arguments, so "FILE -dry-run" works.
func reorder(a []string) []string {
	var flags, pos []string
	for _, s := range a {
		if strings.HasPrefix(s, "-") {
			flags = append(flags, s)
		} else {
			pos = append(pos, s)
		}
	}
	return append(flags, pos...)
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// do sends one admin request and decodes the JSON answer.
func (c *client) do(method, path string, body any) (map[string]any, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", "gogctl/"+version)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, fmt.Errorf("%s %s: status %d: %.200s", method, path, resp.StatusCode, data)
		}
	}
	if resp.StatusCode >= 300 {
		msg := resp.Status
		if e, ok := out["error"].(map[string]any); ok {
			msg = fmt.Sprintf("%s: %v", resp.Status, e["message"])
		}
		return nil, fmt.Errorf("%s %s: %s", method, path, msg)
	}
	return out, nil
}

func (c *client) print(v map[string]any, err error) error {
	if err != nil {
		return err
	}
	if v == nil {
		return nil
	}
	enc := json.NewEncoder(c.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (c *client) create(path string, a []string, n int, body func() any) error {
	if len(a) != n {
		return fmt.Errorf("want %d arguments, got %d", n, len(a))
	}
	return c.print(c.do(http.MethodPost, path, body()))
}

func (c *client) del(prefix string, a []string) error {
	if len(a) != 1 {
		return errors.New("want one id")
	}
	if _, err := c.do(http.MethodDelete, prefix+url.PathEscape(a[0]), nil); err != nil {
		return err
	}
	fmt.Fprintln(c.stdout, "deleted", a[0])
	return nil
}

// list prints a list endpoint's data as a table of the given columns.
func (c *client) list(path string, cols []string) error {
	out, err := c.do(http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if c.output == "json" {
		return c.print(out, nil)
	}
	rows, _ := out["data"].([]any)
	return c.table(cols, rows)
}

func (c *client) table(cols []string, rows []any) error {
	tw := tabwriter.NewWriter(c.stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, strings.ToUpper(strings.Join(cols, "\t")))
	for _, r := range rows {
		m, _ := r.(map[string]any)
		vals := make([]string, len(cols))
		for i, col := range cols {
			vals[i] = cell(m[col])
		}
		fmt.Fprintln(tw, strings.Join(vals, "\t"))
	}
	return tw.Flush()
}

func cell(v any) string {
	switch x := v.(type) {
	case nil:
		return "-"
	case []any:
		s := make([]string, len(x))
		for i, e := range x {
			s[i] = cell(e)
		}
		return strings.Join(s, ",")
	case float64:
		return fmt.Sprintf("%g", x)
	default:
		return fmt.Sprint(x)
	}
}

func (c *client) usage(a []string) error {
	uf := flag.NewFlagSet("usage", flag.ContinueOnError)
	org := uf.String("org", "", "organization id")
	group := uf.String("group-by", "model", "model, team, key, provider, endpoint or day")
	from := uf.String("from", "", "start, RFC 3339 (default: start of the month)")
	to := uf.String("to", "", "end, RFC 3339 (default: now)")
	if err := uf.Parse(a); err != nil {
		return err
	}
	q := url.Values{"group_by": {*group}}
	for k, v := range map[string]string{"org_id": *org, "from": *from, "to": *to} {
		if v != "" {
			q.Set(k, v)
		}
	}
	return c.list("/admin/v1/usage?"+q.Encode(), []string{"group", "requests", "input_tokens", "output_tokens", "cost_usd"})
}

func (c *client) catalog() error {
	out, err := c.do(http.MethodGet, "/admin/v1/catalog", nil)
	if err != nil {
		return err
	}
	if c.output == "json" {
		return c.print(out, nil)
	}
	var rows []any
	providers, _ := out["providers"].([]any)
	for _, p := range providers {
		pm, _ := p.(map[string]any)
		models, _ := pm["models"].([]any)
		for _, m := range models {
			mm, _ := m.(map[string]any)
			price, _ := mm["price"].(map[string]any)
			rows = append(rows, map[string]any{"provider": pm["id"], "model": mm["id"], "kind": mm["kind"],
				"input_per_mtok": price["input_per_mtok"], "output_per_mtok": price["output_per_mtok"]})
		}
	}
	return c.table([]string{"provider", "model", "kind", "input_per_mtok", "output_per_mtok"}, rows)
}

// exportDoc is the file config export writes and config import reads:
// objects by kind and name, each spec in the config file's shape.
type exportDoc struct {
	Objects map[string]map[string]any `yaml:"objects,omitempty"`
	// Orgs holds organizations by name (tenants.go).
	Orgs map[string]map[string]any `yaml:"orgs,omitempty"`
}

func (c *client) export(all bool) error {
	out, err := c.do(http.MethodGet, "/admin/v1/config", nil)
	if err != nil {
		return err
	}
	doc := exportDoc{Objects: map[string]map[string]any{}}
	entries, _ := out["data"].([]any)
	for _, e := range entries {
		m, _ := e.(map[string]any)
		src, _ := m["source"].(string)
		if src == "deleted" || (src == "file" && !all) {
			continue
		}
		kind, _ := m["kind"].(string)
		name, _ := m["name"].(string)
		if doc.Objects[kind] == nil {
			doc.Objects[kind] = map[string]any{}
		}
		doc.Objects[kind][name] = m["spec"]
	}
	if doc.Orgs, err = c.exportTenants(); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "# Exported by gogctl from %s at %s.\n", c.base, time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintln(c.stdout, "# Secrets read \"set\": importing into the same gateway keeps them; elsewhere, replace them.")
	enc := yaml.NewEncoder(c.stdout)
	enc.SetIndent(2)
	return enc.Encode(doc)
}

// kindOrder applies providers before the models that name them, and so on.
var kindOrder = []string{"providers", "models", "guardrails", "mcp_servers", "mcp_virtual_servers", "a2a_agents", "alert_webhooks", "streams"}

func (c *client) importFile(path string, dry, prune bool) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc exportDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	for kind := range doc.Objects {
		if !slices.Contains(kindOrder, kind) {
			return fmt.Errorf("%s: unknown kind %q", path, kind)
		}
	}
	orgs, err := parseOrgs(doc.Orgs)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	a := &applier{c: c, dry: dry}
	if doc.Objects != nil {
		if err := a.importObjects(doc.Objects, prune); err != nil {
			return err
		}
	}
	// Organizations come after the objects their policies name.
	if doc.Orgs != nil {
		if err := a.importTenants(orgs, prune); err != nil {
			return err
		}
	}
	switch {
	case dry:
		fmt.Fprintf(c.stdout, "%d changes\n", a.changes)
	case a.changes == 0:
		fmt.Fprintln(c.stdout, "nothing to change")
	default:
		fmt.Fprintf(c.stdout, "%d changes applied\n", a.changes)
	}
	return nil
}

// importObjects puts each object of the file, providers first, and with
// prune deletes stored objects the file leaves out.
func (a *applier) importObjects(objects map[string]map[string]any, prune bool) error {
	out, err := a.c.do(http.MethodGet, "/admin/v1/config", nil)
	if err != nil {
		return err
	}
	stored := map[string]map[string]any{}
	entries, _ := out["data"].([]any)
	for _, e := range entries {
		m, _ := e.(map[string]any)
		kind, _ := m["kind"].(string)
		name, _ := m["name"].(string)
		stored[kind+"/"+name] = m
	}
	for _, kind := range kindOrder {
		for _, name := range slices.Sorted(maps.Keys(objects[kind])) {
			spec := jsonable(objects[kind][name])
			if cur := stored[kind+"/"+name]; cur != nil && cur["source"] != "deleted" && sameJSON(cur["spec"], spec) {
				continue
			}
			if _, err := a.do("put", kind+"/"+name, http.MethodPut, "/admin/v1/config/"+kind+"/"+url.PathEscape(name), spec); err != nil {
				return err
			}
		}
	}
	if !prune {
		return nil
	}
	// Delete in reverse order, so models go before their providers.
	for i := len(kindOrder) - 1; i >= 0; i-- {
		kind := kindOrder[i]
		for _, key := range slices.Sorted(maps.Keys(stored)) {
			m := stored[key]
			name, _ := m["name"].(string)
			if m["kind"] != kind || m["source"] != "store" {
				continue
			}
			if _, ok := objects[kind][name]; ok {
				continue
			}
			if _, err := a.do("delete", kind+"/"+name, http.MethodDelete, "/admin/v1/config/"+kind+"/"+url.PathEscape(name), nil); err != nil {
				return err
			}
		}
	}
	return nil
}

// sameJSON compares two values by their JSON encoding.
func sameJSON(a, b any) bool {
	ra, err1 := json.Marshal(a)
	rb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	var na, nb any
	_ = json.Unmarshal(ra, &na)
	_ = json.Unmarshal(rb, &nb)
	return reflect.DeepEqual(na, nb)
}

// jsonable turns YAML's map[string]any trees into values encoding/json
// accepts (yaml.v3 already uses string keys for string-keyed maps).
func jsonable(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = jsonable(e)
		}
	case []any:
		for i, e := range x {
			x[i] = jsonable(e)
		}
	}
	return v
}
