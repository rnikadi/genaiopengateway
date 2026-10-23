// Package config loads and validates the gateway's YAML configuration.
// See docs/config.md for the reference.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/rnikadi/genaiopengateway/internal/policy"
)

// Config is the root of the configuration file.
type Config struct {
	Listen    string              `yaml:"listen"`
	TLS       TLS                 `yaml:"tls"`
	Admin     Admin               `yaml:"admin"`
	Metrics   Metrics             `yaml:"metrics"`
	Store     Store               `yaml:"store"`
	Limiter   Limiter             `yaml:"limiter"`
	Limits    Limits              `yaml:"limits"`
	Providers map[string]Provider `yaml:"providers"`
	Models    map[string]Model    `yaml:"models"`
	Keys      []StaticKey         `yaml:"keys"`
	MCP       MCP                 `yaml:"mcp"`
	SSO       SSO                 `yaml:"sso"`
	UI        UI                  `yaml:"ui"`
	Alerts    Alerts              `yaml:"alerts"`
}

// Alerts configures budget alerts and the webhooks that receive alerts
// (docs/alerts.md).
type Alerts struct {
	// BudgetThresholds are shares of a budget at which to alert.
	BudgetThresholds []float64 `yaml:"budget_thresholds"`
	Webhooks         []Webhook `yaml:"webhooks"`
}

// Alert events.
const (
	EventBudgetThreshold = "budget.threshold"
	EventMCPToolPending  = "mcp.tool_pending"
	EventKeyExpiring     = "key.expiring"
)

// Webhook formats.
const (
	FormatJSON  = "json"
	FormatSlack = "slack"
)

// Webhook receives alerts.
type Webhook struct {
	Name   string   `yaml:"name"`
	URL    Secret   `yaml:"url"`
	Secret Secret   `yaml:"secret"`
	Format string   `yaml:"format"`
	Events []string `yaml:"events"`
	// Org limits the webhook to alerts about one organization, by name or
	// id; empty receives all.
	Org string `yaml:"org"`
}

// Wants reports whether the webhook receives an event.
func (w *Webhook) Wants(event string) bool { return slices.Contains(w.Events, event) }

// TLS enables HTTPS on the main listener when both files are set.
type TLS struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// Admin configures the admin API.
type Admin struct {
	Token Secret `yaml:"token"`
}

// SSO configures operator sign-in to the admin UI and API (ADR 0016).
type SSO struct {
	OIDC       *OIDC         `yaml:"oidc"`
	SessionTTL Duration      `yaml:"session_ttl"`
	Roles      []RoleMapping `yaml:"roles"`
}

// OIDC is the identity provider operators sign in with.
type OIDC struct {
	Issuer       string   `yaml:"issuer"`
	ClientID     string   `yaml:"client_id"`
	ClientSecret Secret   `yaml:"client_secret"`
	RedirectURL  string   `yaml:"redirect_url"`
	GroupsClaim  string   `yaml:"groups_claim"`
	Scopes       []string `yaml:"scopes"`
}

// Operator roles.
const (
	RoleOwner  = "owner"
	RoleAdmin  = "admin"
	RoleViewer = "viewer"
)

// RoleMapping grants a role to the members of a group, or to one email,
// gateway-wide or for one organization (named or by id).
type RoleMapping struct {
	Group string `yaml:"group"`
	Email string `yaml:"email"`
	Role  string `yaml:"role"`
	Org   string `yaml:"org"`
}

// UI configures the admin UI. It is on whenever the admin API is.
type UI struct {
	Enabled *bool `yaml:"enabled"`
}

// AdminEnabled reports whether the admin API, and with it the UI, is on:
// it needs a store and a way to sign in.
func (c *Config) AdminEnabled() bool {
	return c.Store.Driver != "" && (c.Admin.Token.Value() != "" || c.SSO.OIDC != nil)
}

// UIEnabled reports whether the admin UI is served.
func (c *Config) UIEnabled() bool {
	return c.AdminEnabled() && (c.UI.Enabled == nil || *c.UI.Enabled)
}

// Metrics configures the Prometheus endpoint.
type Metrics struct {
	Listen string `yaml:"listen"`
}

// Store configures the database. An empty Driver means no store.
type Store struct {
	Driver          string   `yaml:"driver"`
	DSN             Secret   `yaml:"dsn"`
	RefreshInterval Duration `yaml:"refresh_interval"`
	UsageRetention  Duration `yaml:"usage_retention"`
}

// Limiter configures where rate limit and budget counters live.
type Limiter struct {
	Driver             string `yaml:"driver"`
	Address            string `yaml:"address"`
	Password           Secret `yaml:"password"`
	FailOpenRateLimits bool   `yaml:"fail_open_rate_limits"`
	FailOpenBudgets    bool   `yaml:"fail_open_budgets"`
}

// Limits are global request limits.
type Limits struct {
	MaxRequestBytes int64 `yaml:"max_request_bytes"`
}

// Provider types.
const (
	ProviderOpenAI    = "openai"
	ProviderAnthropic = "anthropic"
)

// Provider is an upstream that serves models.
type Provider struct {
	Type    string            `yaml:"type"`
	BaseURL string            `yaml:"base_url"`
	APIKey  Secret            `yaml:"api_key"`
	Timeout Duration          `yaml:"timeout"`
	Headers map[string]string `yaml:"headers"`
}

// Model is a virtual model. Targets are tried in order.
type Model struct {
	Targets []Target `yaml:"targets"`
}

// Target maps a virtual model onto one provider's model.
type Target struct {
	Provider string `yaml:"provider" json:"provider"`
	Model    string `yaml:"model" json:"model"`
	Price    Price  `yaml:"price" json:"price"`
}

// String identifies the target as provider/model.
func (t Target) String() string { return t.Provider + "/" + t.Model }

// Price is the cost of a target in USD per million tokens.
type Price struct {
	InputPerMTok       float64 `yaml:"input_per_mtok" json:"input_per_mtok"`
	OutputPerMTok      float64 `yaml:"output_per_mtok" json:"output_per_mtok"`
	CachedInputPerMTok float64 `yaml:"cached_input_per_mtok" json:"cached_input_per_mtok"`
}

// IsZero reports whether no price is configured.
func (p Price) IsZero() bool { return p == Price{} }

// MCP configures the MCP gateway. See docs/mcp.md.
type MCP struct {
	// PublicURL is the gateway's external URL, used to name each virtual
	// server as an OAuth protected resource.
	PublicURL       string                   `yaml:"public_url"`
	RefreshInterval Duration                 `yaml:"refresh_interval"`
	Servers         map[string]MCPServer     `yaml:"servers"`
	VirtualServers  map[string]VirtualServer `yaml:"virtual_servers"`
	OAuth           *OAuth                   `yaml:"oauth"`
}

// OAuth accepts access tokens from an external authorization server on
// MCP endpoints (ADR 0014).
type OAuth struct {
	Issuer string `yaml:"issuer"`
	// JWKSURL overrides discovery from the issuer's metadata.
	JWKSURL string `yaml:"jwks_url"`
	// Audience is accepted in addition to each virtual server's URL.
	Audience string `yaml:"audience"`
	// Organization is the id of the organization tokens map into.
	Organization string `yaml:"organization"`
	// TeamClaim names the claim whose value names the caller's team.
	TeamClaim string   `yaml:"team_claim"`
	Scopes    []string `yaml:"scopes"`
}

// MCPServer is an upstream MCP server reached over Streamable HTTP.
type MCPServer struct {
	URL      string            `yaml:"url"`
	Headers  map[string]Secret `yaml:"headers"`
	Timeout  Duration          `yaml:"timeout"`
	PinTools PinMode           `yaml:"pin_tools"`
}

// PinMode says how an upstream's tool definitions are pinned.
type PinMode string

// Pin modes. In YAML, true is PinOn and false is PinOff.
const (
	// PinOn approves a tool when first seen and hides it when its
	// definition changes, until approved again.
	PinOn PinMode = "on"
	// PinStrict also requires approval for tools seen for the first time.
	PinStrict PinMode = "strict"
	PinOff    PinMode = "off"
)

// UnmarshalYAML accepts true, false or strict.
func (m *PinMode) UnmarshalYAML(n *yaml.Node) error {
	switch n.Value {
	case "true":
		*m = PinOn
	case "false":
		*m = PinOff
	case "strict":
		*m = PinStrict
	default:
		return fmt.Errorf("line %d: pin_tools must be true, false or strict", n.Line)
	}
	return nil
}

// VirtualServer is what clients connect to at /mcp/{name}.
type VirtualServer struct {
	Description string             `yaml:"description"`
	Servers     []VirtualServerRef `yaml:"servers"`
}

// VirtualServerRef exposes part of one upstream server in a virtual server.
type VirtualServerRef struct {
	Server string `yaml:"server"`
	// Tools lists the upstream tool names to expose; nil exposes all.
	Tools []string `yaml:"tools"`
	// Prefix is prepended to tool and prompt names with an underscore.
	// Nil means the server name; an empty string means no prefix.
	Prefix    *string `yaml:"prefix"`
	Prompts   *bool   `yaml:"prompts"`
	Resources *bool   `yaml:"resources"`
}

// NamePrefix returns the prefix applied to tool and prompt names,
// including the separator.
func (r VirtualServerRef) NamePrefix() string {
	p := r.Server
	if r.Prefix != nil {
		p = *r.Prefix
	}
	if p == "" {
		return ""
	}
	return p + "_"
}

// ExposesPrompts reports whether the upstream's prompts are included.
func (r VirtualServerRef) ExposesPrompts() bool { return r.Prompts == nil || *r.Prompts }

// ExposesResources reports whether the upstream's resources are included.
func (r VirtualServerRef) ExposesResources() bool { return r.Resources == nil || *r.Resources }

// StaticKey is an API key declared in the config file.
type StaticKey struct {
	Name          string `yaml:"name"`
	Key           Secret `yaml:"key"`
	policy.Policy `yaml:",inline"`
}

// Defaults applied before the file is decoded.
const (
	DefaultListen          = ":8080"
	DefaultMaxRequestBytes = 32 << 20
	DefaultProviderTimeout = 5 * time.Minute
	DefaultRefreshInterval = 5 * time.Second
	DefaultMCPRefresh      = time.Minute
	DefaultMCPTimeout      = time.Minute
	DefaultSessionTTL      = 8 * time.Hour
	MinAdminTokenLength    = 32
	MinStaticKeyLength     = 16
)

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// Parse decodes and validates config bytes. ${VAR} references in values
// are replaced from the environment; an unset variable is an error.
func Parse(raw []byte) (*Config, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := expandEnv(&doc); err != nil {
		return nil, err
	}
	c := Config{
		Listen:  DefaultListen,
		Limits:  Limits{MaxRequestBytes: DefaultMaxRequestBytes},
		Limiter: Limiter{Driver: "memory", FailOpenRateLimits: true},
		Store:   Store{RefreshInterval: Duration(DefaultRefreshInterval)},
		MCP:     MCP{RefreshInterval: Duration(DefaultMCPRefresh)},
		SSO:     SSO{SessionTTL: Duration(DefaultSessionTTL)},
		Alerts:  Alerts{BudgetThresholds: DefaultBudgetThresholds},
	}
	if len(doc.Content) > 0 {
		dec := doc.Content[0]
		if err := dec.Decode(&c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// expandEnv replaces ${VAR} in every scalar value. Keys and comments are
// left alone so documentation in the file cannot break parsing.
func expandEnv(n *yaml.Node) error {
	var missing []string
	var walk func(*yaml.Node, bool)
	walk = func(n *yaml.Node, isKey bool) {
		if n.Kind == yaml.ScalarNode && !isKey && strings.Contains(n.Value, "${") {
			n.Value = os.Expand(n.Value, func(name string) string {
				v, ok := os.LookupEnv(name)
				if !ok {
					missing = append(missing, name)
				}
				return v
			})
		}
		for i, c := range n.Content {
			walk(c, n.Kind == yaml.MappingNode && i%2 == 0)
		}
	}
	walk(n, false)
	if len(missing) > 0 {
		return fmt.Errorf("config references unset environment variables: %s", strings.Join(missing, ", "))
	}
	return nil
}

func (c *Config) validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if (c.TLS.CertFile == "") != (c.TLS.KeyFile == "") {
		add("tls: cert_file and key_file must be set together")
	}
	if t := c.Admin.Token.Value(); t != "" && len(t) < MinAdminTokenLength {
		add("admin.token must be at least %d characters", MinAdminTokenLength)
	}
	switch c.Store.Driver {
	case "", "sqlite", "postgres":
	default:
		add("store.driver must be sqlite or postgres, got %q", c.Store.Driver)
	}
	if c.Store.Driver != "" && c.Store.DSN.Value() == "" {
		add("store.dsn is required when store.driver is set")
	}
	switch c.Limiter.Driver {
	case "memory":
	case "valkey":
		if c.Limiter.Address == "" {
			add("limiter.address is required for the valkey driver")
		}
	default:
		add("limiter.driver must be memory or valkey, got %q", c.Limiter.Driver)
	}
	if c.Limits.MaxRequestBytes <= 0 {
		add("limits.max_request_bytes must be positive")
	}

	for name, p := range c.Providers {
		switch p.Type {
		case "":
			p.Type = ProviderOpenAI
		case ProviderOpenAI, ProviderAnthropic:
		default:
			add("provider %q: type must be openai or anthropic, got %q", name, p.Type)
		}
		if p.BaseURL == "" {
			add("provider %q: base_url is required", name)
		} else if u, err := url.Parse(p.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			add("provider %q: base_url must be an http or https URL", name)
		}
		p.BaseURL = strings.TrimRight(p.BaseURL, "/")
		if p.Timeout <= 0 {
			p.Timeout = Duration(DefaultProviderTimeout)
		}
		c.Providers[name] = p
	}

	for name, m := range c.Models {
		if len(m.Targets) == 0 {
			add("model %q: at least one target is required", name)
		}
		for i, t := range m.Targets {
			if _, ok := c.Providers[t.Provider]; !ok {
				add("model %q target %d: unknown provider %q", name, i, t.Provider)
			}
			if t.Model == "" {
				add("model %q target %d: model is required", name, i)
			}
			if t.Price.InputPerMTok < 0 || t.Price.OutputPerMTok < 0 || t.Price.CachedInputPerMTok < 0 {
				add("model %q target %d: prices must not be negative", name, i)
			}
		}
	}

	c.validateMCP(add)
	c.validateSSO(add)
	c.validateAlerts(add)

	seen := map[string]bool{}
	names := map[string]bool{}
	for i, k := range c.Keys {
		switch {
		case k.Name == "":
			add("keys[%d]: name is required", i)
		case names[k.Name]:
			add("keys[%d]: duplicate name %q", i, k.Name)
		}
		names[k.Name] = true
		v := k.Key.Value()
		switch {
		case len(v) < MinStaticKeyLength:
			add("key %q: key must be at least %d characters", k.Name, MinStaticKeyLength)
		case seen[v]:
			add("key %q: duplicate key value", k.Name)
		}
		seen[v] = true
		if err := k.Policy.Validate(); err != nil {
			add("key %q: %v", k.Name, err)
		}
		for _, m := range k.AllowedModels {
			if _, ok := c.Models[m]; !ok {
				add("key %q: allowed model %q is not configured", k.Name, m)
			}
		}
		for _, v := range k.AllowedMCPServers {
			if _, ok := c.MCP.VirtualServers[v]; !ok {
				add("key %q: allowed MCP server %q is not configured", k.Name, v)
			}
		}
	}
	return errors.Join(errs...)
}

// DefaultBudgetThresholds are the shares of a budget that raise alerts.
var DefaultBudgetThresholds = []float64{0.5, 0.8, 1.0}

func (c *Config) validateAlerts(add func(string, ...any)) {
	a := &c.Alerts
	for _, t := range a.BudgetThresholds {
		if t <= 0 || t > 1 {
			add("alerts.budget_thresholds values must be above 0 and at most 1, got %v", t)
		}
	}
	names := map[string]bool{}
	for i := range a.Webhooks {
		w := &a.Webhooks[i]
		if w.Name == "" {
			add("alerts.webhooks[%d]: name is required", i)
		} else if names[w.Name] {
			add("alerts.webhooks: name %q is used twice", w.Name)
		}
		names[w.Name] = true
		if !webhookURL(w.URL.Value()) {
			add("alerts webhook %q: url must be https, or http to localhost", w.Name)
		}
		switch w.Format {
		case "":
			w.Format = FormatJSON
		case FormatJSON, FormatSlack:
		default:
			add("alerts webhook %q: format must be json or slack, got %q", w.Name, w.Format)
		}
		if len(w.Events) == 0 {
			add("alerts webhook %q: events must list at least one event", w.Name)
		}
		for _, e := range w.Events {
			switch e {
			case EventBudgetThreshold, EventMCPToolPending, EventKeyExpiring:
			default:
				add("alerts webhook %q: unknown event %q", w.Name, e)
			}
		}
	}
	if len(a.Webhooks) > 0 && c.Store.Driver == "" {
		add("alerts need a store, to send each alert once")
	}
}

// webhookURL allows https, and http only to this machine for development.
func webhookURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		h := u.Hostname()
		return h == "localhost" || h == "127.0.0.1" || h == "::1"
	}
	return false
}

func (c *Config) validateSSO(add func(string, ...any)) {
	s := &c.SSO
	if o := s.OIDC; o != nil {
		if !httpURL(o.Issuer) {
			add("sso.oidc.issuer must be an http or https URL")
		}
		if o.ClientID == "" || o.ClientSecret.Value() == "" {
			add("sso.oidc.client_id and sso.oidc.client_secret are required")
		}
		if u, err := url.Parse(o.RedirectURL); err != nil || !httpURL(o.RedirectURL) || u.Path != "/ui/auth/callback" {
			add("sso.oidc.redirect_url must be the gateway's URL ending in /ui/auth/callback")
		}
		if o.GroupsClaim == "" {
			o.GroupsClaim = "groups"
		}
		if len(o.Scopes) == 0 {
			o.Scopes = []string{"openid", "email", "profile"}
		}
		if len(s.Roles) == 0 {
			add("sso.roles is required with sso.oidc: without roles nobody can sign in")
		}
		if c.Store.Driver == "" {
			add("sso.oidc needs a store, where sessions are kept")
		}
	}
	if s.SessionTTL < Duration(time.Minute) {
		add("sso.session_ttl must be at least 1m")
	}
	for i, r := range s.Roles {
		if (r.Group == "") == (r.Email == "") {
			add("sso.roles[%d]: set exactly one of group and email", i)
		}
		switch {
		case r.Role == RoleOwner && r.Org != "":
			add("sso.roles[%d]: owner is gateway-wide and takes no org", i)
		case r.Role == RoleAdmin && r.Org == "":
			add("sso.roles[%d]: admin needs an org", i)
		case r.Role != RoleOwner && r.Role != RoleAdmin && r.Role != RoleViewer:
			add("sso.roles[%d]: role must be owner, admin or viewer, got %q", i, r.Role)
		}
	}
}

var (
	// mcpName is a server or virtual server name: safe in a URL path and
	// as a metric label.
	mcpName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	// mcpPrefix fits the MCP tool name alphabet.
	mcpPrefix = regexp.MustCompile(`^[A-Za-z0-9.-][A-Za-z0-9_.-]{0,63}$`)
)

func (c *Config) validateMCP(add func(string, ...any)) {
	m := &c.MCP
	if m.RefreshInterval < Duration(time.Second) {
		add("mcp.refresh_interval must be at least 1s")
	}
	m.PublicURL = strings.TrimRight(m.PublicURL, "/")
	if m.PublicURL != "" && !httpURL(m.PublicURL) {
		add("mcp.public_url must be an http or https URL")
	}
	if o := m.OAuth; o != nil {
		if !httpURL(o.Issuer) {
			add("mcp.oauth.issuer must be an http or https URL")
		}
		if o.JWKSURL != "" && !httpURL(o.JWKSURL) {
			add("mcp.oauth.jwks_url must be an http or https URL")
		}
		if m.PublicURL == "" {
			add("mcp.oauth needs mcp.public_url, which names each virtual server to clients")
		}
		if o.Organization == "" || o.TeamClaim == "" {
			add("mcp.oauth.organization and mcp.oauth.team_claim are required")
		}
		if c.Store.Driver == "" {
			add("mcp.oauth needs a store, where organizations and teams are kept")
		}
	}
	for name, s := range m.Servers {
		if !mcpName.MatchString(name) {
			add("mcp server %q: names use lowercase letters, digits, - and _", name)
		}
		if !httpURL(s.URL) {
			add("mcp server %q: url must be an http or https URL", name)
		}
		if s.Timeout <= 0 {
			s.Timeout = Duration(DefaultMCPTimeout)
		}
		if s.PinTools == "" {
			s.PinTools = PinOn
		}
		if s.PinTools == PinStrict && c.Store.Driver == "" {
			add("mcp server %q: pin_tools: strict needs a store, where approvals are kept", name)
		}
		m.Servers[name] = s
	}
	for name, v := range m.VirtualServers {
		if !mcpName.MatchString(name) {
			add("mcp virtual server %q: names use lowercase letters, digits, - and _", name)
		}
		if len(v.Servers) == 0 {
			add("mcp virtual server %q: at least one server is required", name)
		}
		prefixes := map[string]bool{}
		for i, r := range v.Servers {
			if _, ok := m.Servers[r.Server]; !ok {
				add("mcp virtual server %q servers[%d]: unknown server %q", name, i, r.Server)
			}
			p := r.NamePrefix()
			switch {
			case p != "" && !mcpPrefix.MatchString(strings.TrimSuffix(p, "_")):
				add("mcp virtual server %q servers[%d]: prefix may use letters, digits, _, . and -", name, i)
			case p == "" && len(v.Servers) > 1:
				add("mcp virtual server %q servers[%d]: an empty prefix is allowed only for a single server", name, i)
			case prefixes[p]:
				add("mcp virtual server %q servers[%d]: prefix %q is used twice", name, i, strings.TrimSuffix(p, "_"))
			}
			prefixes[p] = true
		}
	}
}

func httpURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// Secret is a string that never prints its value.
type Secret string

// Value returns the secret itself.
func (s Secret) Value() string { return string(s) }

// String hides the value.
func (s Secret) String() string {
	if s == "" {
		return ""
	}
	return "[redacted]"
}

// LogValue hides the value in structured logs.
func (s Secret) LogValue() slog.Value { return slog.StringValue(s.String()) }

// MarshalText hides the value when encoded.
func (s Secret) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// Duration is a time.Duration that also accepts a "d" suffix for days.
type Duration time.Duration

// UnmarshalYAML parses values like "30s", "5m" or "90d".
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = Duration(v)
	return nil
}

// Std returns the value as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// ParseDuration parses a Go duration or a whole number of days ("90d").
func ParseDuration(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	return v, nil
}
