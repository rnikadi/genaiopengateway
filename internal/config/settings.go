package config

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Gateway settings changed in the admin UI (docs/admin-ui-settings.md,
// ADR 0027) are one stored object of kind settings, named gateway, in
// the config file's shape. Unlike named objects, its fields win over
// the file's.
const (
	KindSettings = "settings"
	SettingsName = "gateway"
)

// StartupPaths are the settings read when the process starts, which stay
// in the environment and config file. Every other setting has a control
// in the admin UI; ui/coverage_test.go checks it.
var StartupPaths = []string{
	"listen", "tls.cert_file", "tls.key_file", "metrics.listen", "admin.token",
	"store.driver", "store.dsn",
	"limiter.driver", "limiter.address", "limiter.password",
	"secrets.encryption_key", "secrets.previous_keys",
	"ui.enabled",
	"keys",
}

// Settings are the fields a settings object may set: every gateway-wide
// setting that is not a startup setting or a named object.
type Settings struct {
	Store struct {
		RefreshInterval Duration `yaml:"refresh_interval"`
		UsageRetention  Duration `yaml:"usage_retention"`
	} `yaml:"store"`
	Limiter struct {
		FailOpenRateLimits bool `yaml:"fail_open_rate_limits"`
		FailOpenBudgets    bool `yaml:"fail_open_budgets"`
	} `yaml:"limiter"`
	Limits      Limits      `yaml:"limits"`
	BodyLogging BodyLogging `yaml:"body_logging"`
	Cache       Cache       `yaml:"cache"`
	Alerts      struct {
		BudgetThresholds []float64 `yaml:"budget_thresholds"`
	} `yaml:"alerts"`
	SSO SSO `yaml:"sso"`
	MCP struct {
		PublicURL       string   `yaml:"public_url"`
		RefreshInterval Duration `yaml:"refresh_interval"`
		MaxUserSessions int      `yaml:"max_user_sessions"`
		OAuth           *OAuth   `yaml:"oauth"`
	} `yaml:"mcp"`
}

// overlay sets every field of the mapping src in dst, recursing into
// mappings both have, so src's values win.
func overlay(dst, src *yaml.Node) {
	for i := 0; i+1 < len(src.Content); i += 2 {
		key, value := src.Content[i], src.Content[i+1]
		cur := lookup(dst, key.Value)
		switch {
		case cur != nil && cur.Kind == yaml.MappingNode && value.Kind == yaml.MappingNode:
			overlay(cur, value)
		case cur != nil:
			*cur = *value
		default:
			dst.Content = append(dst.Content, scalar(key.Value), value)
		}
	}
}

// addSettings lays a settings object over the file's mapping.
func addSettings(root *yaml.Node, o Object) error {
	if o.Name != SettingsName {
		return fmt.Errorf("settings must be named %q", SettingsName)
	}
	var spec yaml.Node
	if err := yaml.Unmarshal(o.Spec, &spec); err != nil || len(spec.Content) == 0 || spec.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("settings: the spec must be a JSON object")
	}
	overlay(root, spec.Content[0])
	return nil
}

// Format writes a duration the way config files do: 30d, 5m, 1h30m or
// 2s, and "" for zero.
func (d Duration) Format() string {
	std := d.Std()
	switch {
	case std == 0:
		return ""
	case std%(24*time.Hour) == 0:
		return strconv.Itoa(int(std/(24*time.Hour))) + "d"
	}
	s := std.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// SettingsOf returns the settings part of a config.
func SettingsOf(c *Config) Settings {
	var s Settings
	s.Store.RefreshInterval, s.Store.UsageRetention = c.Store.RefreshInterval, c.Store.UsageRetention
	s.Limiter.FailOpenRateLimits, s.Limiter.FailOpenBudgets = c.Limiter.FailOpenRateLimits, c.Limiter.FailOpenBudgets
	s.Limits, s.BodyLogging, s.Cache, s.SSO = c.Limits, c.BodyLogging, c.Cache, c.SSO
	s.Alerts.BudgetThresholds = c.Alerts.BudgetThresholds
	s.MCP.PublicURL, s.MCP.RefreshInterval, s.MCP.MaxUserSessions, s.MCP.OAuth = c.MCP.PublicURL, c.MCP.RefreshInterval, c.MCP.MaxUserSessions, c.MCP.OAuth
	return s
}

// Flatten lists every setting by its dotted path, such as
// cache.semantic.threshold, with its value as the admin API shows it:
// durations as text, secrets as whether they are set, and lists whole.
// Fields under an unset section are listed with nil values.
func (s Settings) Flatten() map[string]any {
	out := map[string]any{}
	var walk func(v reflect.Value, t reflect.Type, path string)
	walk = func(v reflect.Value, t reflect.Type, path string) {
		if t.Kind() == reflect.Pointer {
			if v.IsValid() && !v.IsNil() {
				v = v.Elem()
			} else {
				v = reflect.Value{}
			}
			t = t.Elem()
		}
		if t.Kind() == reflect.Struct && t != reflect.TypeOf(Duration(0)) {
			for i := 0; i < t.NumField(); i++ {
				f := t.Field(i)
				name := strings.Split(f.Tag.Get("yaml"), ",")[0]
				if name == "" || name == "-" {
					continue
				}
				var fv reflect.Value
				if v.IsValid() {
					fv = v.Field(i)
				}
				p := name
				if path != "" {
					p = path + "." + name
				}
				walk(fv, f.Type, p)
			}
			return
		}
		if !v.IsValid() {
			out[path] = nil
			return
		}
		switch x := v.Interface().(type) {
		case Duration:
			out[path] = x.Format()
		case Secret:
			out[path] = x != ""
		default:
			out[path] = x
		}
	}
	walk(reflect.ValueOf(s), reflect.TypeOf(s), "")
	return out
}

// SettingPaths are the paths Flatten reports.
func SettingPaths() []string {
	return slices.Sorted(maps.Keys(Settings{}.Flatten()))
}

// FilePaths reports which dotted paths a config file sets. A list is one
// path; the mappings inside it are not walked.
func FilePaths(raw []byte) map[string]bool {
	out := map[string]bool{}
	var doc yaml.Node
	if yaml.Unmarshal(raw, &doc) != nil || len(doc.Content) == 0 {
		return out
	}
	var walk func(n *yaml.Node, path string)
	walk = func(n *yaml.Node, path string) {
		if n.Kind != yaml.MappingNode {
			if path != "" && !(n.Kind == yaml.ScalarNode && n.Tag == "!!null") {
				out[path] = true
			}
			return
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			p := n.Content[i].Value
			if path != "" {
				p = path + "." + p
			}
			walk(n.Content[i+1], p)
		}
	}
	walk(doc.Content[0], "")
	return out
}
