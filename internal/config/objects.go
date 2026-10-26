package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"

	"gopkg.in/yaml.v3"
)

// Kinds of configuration that can also be stored through the admin API
// (ADR 0023).
const (
	KindProviders         = "providers"
	KindModels            = "models"
	KindGuardrails        = "guardrails"
	KindMCPServers        = "mcp_servers"
	KindMCPVirtualServers = "mcp_virtual_servers"
	KindAlertWebhooks     = "alert_webhooks"
)

// Kinds lists the storable kinds in the order a merge adds them.
var Kinds = []string{KindProviders, KindModels, KindGuardrails, KindMCPServers, KindMCPVirtualServers, KindAlertWebhooks}

// kindPath is where a kind lives in the config file. Alert webhooks are
// a list keyed by name; the others are maps.
var kindPath = map[string][]string{
	KindProviders:         {"providers"},
	KindModels:            {"models"},
	KindGuardrails:        {"guardrails"},
	KindMCPServers:        {"mcp", "servers"},
	KindMCPVirtualServers: {"mcp", "virtual_servers"},
	KindAlertWebhooks:     {"alerts", "webhooks"},
}

// SecretFields lists the fields of each kind's spec that hold secrets.
// "*" matches every key of a map.
var SecretFields = map[string][][]string{
	KindProviders:     {{"api_key"}},
	KindGuardrails:    {{"url"}, {"secret"}},
	KindMCPServers:    {{"headers", "*"}, {"user_auth", "client_secret"}},
	KindAlertWebhooks: {{"url"}, {"secret"}},
	KindSettings:      {{"sso", "oidc", "client_secret"}},
}

// objectName is what a stored object may be called: it appears in URLs,
// metrics labels and tool name prefixes.
var objectName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// Object is a stored configuration object: a spec in the same shape as
// the file's entry for that kind and name, as JSON.
type Object struct {
	Kind string
	Name string
	Spec []byte
}

// ValidObjectName reports whether a stored object may have this name.
func ValidObjectName(name string) bool { return objectName.MatchString(name) }

// ParseWith decodes raw like Parse and adds stored objects to it. A
// stored object replaces the file's object of the same name, and one
// whose spec is null removes it (ADR 0028). Stored values are not
// expanded as ${VAR}, so the admin API cannot read the gateway's
// environment.
func ParseWith(raw []byte, objects []Object) (*Config, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := expandEnv(&doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	root := doc.Content[0]
	if root.Kind == yaml.ScalarNode && root.Tag == "!!null" {
		root.Kind, root.Tag = yaml.MappingNode, "!!map"
	}
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("parse config: the file must be a mapping")
	}
	for _, kind := range Kinds {
		for _, o := range objects {
			if o.Kind != kind {
				continue
			}
			if err := addObject(root, o); err != nil {
				return nil, err
			}
		}
	}
	for _, o := range objects {
		if o.Kind == KindSettings {
			if err := addSettings(root, o); err != nil {
				return nil, err
			}
		}
	}
	c, err := decode(root)
	if err != nil {
		return nil, err
	}
	c.Raw = raw
	return c, nil
}

// IsDeletion reports whether a stored spec is a deletion marker, which
// removes a file object of the same name.
func IsDeletion(spec []byte) bool { return string(bytes.TrimSpace(spec)) == "null" }

// addObject puts an object into the file's mapping, replacing one of the
// same name, or removes that one for a deletion marker.
func addObject(root *yaml.Node, o Object) error {
	path, ok := kindPath[o.Kind]
	if !ok {
		return fmt.Errorf("unknown kind %q", o.Kind)
	}
	var value *yaml.Node
	if !IsDeletion(o.Spec) {
		var spec yaml.Node
		if err := yaml.Unmarshal(o.Spec, &spec); err != nil || len(spec.Content) == 0 || spec.Content[0].Kind != yaml.MappingNode {
			return fmt.Errorf("%s %q: the spec must be a JSON object", o.Kind, o.Name)
		}
		value = spec.Content[0]
	}
	parent := root
	for _, key := range path[:len(path)-1] {
		parent = child(parent, key, yaml.MappingNode)
	}
	last := path[len(path)-1]
	if o.Kind == KindAlertWebhooks {
		list := child(parent, last, yaml.SequenceNode)
		list.Content = slices.DeleteFunc(list.Content, func(item *yaml.Node) bool {
			n := lookup(item, "name")
			return n != nil && n.Value == o.Name
		})
		if value != nil {
			setKey(value, "name", o.Name)
			list.Content = append(list.Content, value)
		}
		return nil
	}
	m := child(parent, last, yaml.MappingNode)
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == o.Name {
			m.Content = slices.Delete(m.Content, i, i+2)
			break
		}
	}
	if value != nil {
		m.Content = append(m.Content, scalar(o.Name), value)
	}
	return nil
}

// FileSpec returns a file object's spec as JSON, with ${VAR} expanded,
// or nil when the file does not define it.
func FileSpec(raw []byte, kind, name string) ([]byte, error) {
	path, ok := kindPath[kind]
	if !ok {
		return nil, fmt.Errorf("unknown kind %q", kind)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := expandEnv(&doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	n := doc.Content[0]
	for _, key := range path {
		if n = lookup(n, key); n == nil {
			return nil, nil
		}
	}
	var node *yaml.Node
	if kind == KindAlertWebhooks {
		for _, item := range n.Content {
			if v := lookup(item, "name"); v != nil && v.Value == name {
				node = item
			}
		}
	} else {
		node = lookup(n, name)
	}
	if node == nil {
		return nil, nil
	}
	var v any
	if err := node.Decode(&v); err != nil {
		return nil, err
	}
	if m, ok := v.(map[string]any); ok && kind == KindAlertWebhooks {
		delete(m, "name")
	}
	if v == nil {
		v = map[string]any{}
	}
	return json.Marshal(v)
}

// child returns the value of key in a mapping, creating it with kind when
// missing or null.
func child(m *yaml.Node, key string, kind yaml.Kind) *yaml.Node {
	if n := lookup(m, key); n != nil {
		if n.Kind == yaml.ScalarNode && n.Tag == "!!null" {
			n.Kind, n.Tag, n.Value = kind, "", ""
		}
		return n
	}
	n := &yaml.Node{Kind: kind}
	m.Content = append(m.Content, scalar(key), n)
	return n
}

func lookup(m *yaml.Node, key string) *yaml.Node {
	if m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func setKey(m *yaml.Node, key, value string) {
	if n := lookup(m, key); n != nil {
		n.Kind, n.Tag, n.Value = yaml.ScalarNode, "!!str", value
		return
	}
	m.Content = append(m.Content, scalar(key), scalar(value))
}

func scalar(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v} }

// specTypes are the types a spec decodes into, to check its fields.
var specTypes = map[string]func() any{
	KindProviders:         func() any { return new(Provider) },
	KindModels:            func() any { return new(Model) },
	KindGuardrails:        func() any { return new(Guardrail) },
	KindMCPServers:        func() any { return new(MCPServer) },
	KindMCPVirtualServers: func() any { return new(VirtualServer) },
	KindAlertWebhooks:     func() any { return new(Webhook) },
	KindSettings:          func() any { return new(Settings) },
}

// CheckSpec reports a spec that is not a JSON object of the kind's
// fields. Values are checked when the spec is merged.
func CheckSpec(kind string, spec []byte) error {
	newSpec, ok := specTypes[kind]
	if !ok {
		return fmt.Errorf("unknown kind %q", kind)
	}
	dec := yaml.NewDecoder(bytes.NewReader(spec))
	dec.KnownFields(true)
	if err := dec.Decode(newSpec()); err != nil {
		return fmt.Errorf("spec: %w", err)
	}
	return nil
}

// IsSecretPath reports whether a path in a kind's spec holds a secret.
func IsSecretPath(kind string, path []string) bool {
	return slices.ContainsFunc(SecretFields[kind], func(p []string) bool {
		if len(p) != len(path) {
			return false
		}
		for i := range p {
			if p[i] != "*" && p[i] != path[i] {
				return false
			}
		}
		return true
	})
}
