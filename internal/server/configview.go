package server

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/policy"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/redact"
	"github.com/rnikadi/genaiopengateway/internal/router"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

// cacheStoreName is where cached answers live: valkey or memory.
func cacheStoreName(cfg *config.Config) string {
	if cfg.Limiter.Driver == "valkey" {
		return "valkey"
	}
	return "memory"
}

// Version is the gateway's version, set by main.
var Version = "dev"

// healthWindow is how far back provider and target health looks.
const healthWindow = time.Hour

// The handlers in this file show the live configuration read-only
// (docs/admin-ui-config.md). They never return a secret: secrets are
// reported only as set or not set.

// health loads target health keyed by provider and upstream model.
func (s *Server) health(r *http.Request) (map[[2]string]store.TargetHealth, error) {
	rows, err := s.store.Health(r.Context(), time.Now().Add(-healthWindow))
	if err != nil {
		return nil, err
	}
	out := map[[2]string]store.TargetHealth{}
	for _, h := range rows {
		out[[2]string{h.Provider, h.UpstreamModel}] = h
	}
	return out, nil
}

// add sums health into a provider total.
func add(total *store.TargetHealth, h store.TargetHealth) {
	if total.Requests+h.Requests > 0 {
		total.AvgLatencyMS = (total.AvgLatencyMS*total.Requests + h.AvgLatencyMS*h.Requests) / (total.Requests + h.Requests)
	}
	total.Requests += h.Requests
	total.Errors += h.Errors
	if total.Requests > 0 {
		total.ErrorRate = float64(total.Errors) / float64(total.Requests)
	}
	if h.LastErrorAt != nil && (total.LastErrorAt == nil || h.LastErrorAt.After(*total.LastErrorAt)) {
		total.LastErrorAt = h.LastErrorAt
	}
}

// configuredTarget is a model target as the admin UI shows it.
type configuredTarget struct {
	config.Target
	ProviderType string              `json:"provider_type"`
	Endpoints    []provider.Endpoint `json:"endpoints"`
	Health       store.TargetHealth  `json:"health"`
}

// configuredModel is a virtual model as the admin UI shows it: targets
// and prices, never credentials.
type configuredModel struct {
	Name      string              `json:"name"`
	Endpoints []provider.Endpoint `json:"endpoints"`
	Targets   []configuredTarget  `json:"targets"`
}

func (s *Server) listConfiguredModels(w http.ResponseWriter, r *http.Request) error {
	rt := s.rt.Load()
	health, err := s.health(r)
	if err != nil {
		return err
	}
	out := []configuredModel{}
	for _, name := range slices.Sorted(maps.Keys(rt.cfg.Models)) {
		m := configuredModel{Name: name, Endpoints: []provider.Endpoint{}}
		served := map[provider.Endpoint]bool{}
		for _, t := range rt.cfg.Models[name].Targets {
			eps := rt.router.Endpoints(t.Provider, t.Model)
			m.Targets = append(m.Targets, configuredTarget{
				Target: t, ProviderType: rt.cfg.Providers[t.Provider].Type,
				Endpoints: eps, Health: health[[2]string{t.Provider, t.Model}],
			})
			for _, e := range eps {
				served[e] = true
			}
		}
		for _, e := range router.AllEndpoints {
			if served[e] {
				m.Endpoints = append(m.Endpoints, e)
			}
		}
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
	return nil
}

// Provider auth modes.
const (
	authAPIKey      = "api_key"
	authNone        = "none"
	authADC         = "application_default"
	authCredentials = "credentials_file"
	authAWSDefault  = "aws_default"
)

func providerAuth(p config.Provider) string {
	switch {
	case p.Type == config.ProviderVertex && p.CredentialsFile != "":
		return authCredentials
	case p.Type == config.ProviderVertex:
		return authADC
	case p.APIKey != "":
		return authAPIKey
	case p.Type == config.ProviderBedrock:
		return authAWSDefault
	}
	return authNone
}

type configuredProvider struct {
	Name        string             `json:"name"`
	Type        string             `json:"type"`
	BaseURL     string             `json:"base_url,omitempty"`
	Region      string             `json:"region,omitempty"`
	Project     string             `json:"project,omitempty"`
	Location    string             `json:"location,omitempty"`
	Auth        string             `json:"auth"`
	Timeout     string             `json:"timeout,omitempty"`
	HeaderNames []string           `json:"header_names,omitempty"`
	Models      []string           `json:"models"`
	Health      store.TargetHealth `json:"health"`
}

func (s *Server) listProviders(w http.ResponseWriter, r *http.Request) error {
	cfg := s.rt.Load().cfg
	health, err := s.health(r)
	if err != nil {
		return err
	}
	out := []configuredProvider{}
	for _, name := range slices.Sorted(maps.Keys(cfg.Providers)) {
		p := cfg.Providers[name]
		c := configuredProvider{
			Name: name, Type: p.Type, BaseURL: p.BaseURL, Region: p.Region, Project: p.Project, Location: p.Location,
			Auth: providerAuth(p), Timeout: duration(p.Timeout), HeaderNames: slices.Sorted(maps.Keys(p.Headers)), Models: []string{},
		}
		for _, model := range slices.Sorted(maps.Keys(cfg.Models)) {
			if slices.ContainsFunc(cfg.Models[model].Targets, func(t config.Target) bool { return t.Provider == name }) {
				c.Models = append(c.Models, model)
			}
		}
		for key, h := range health {
			if key[0] == name {
				add(&c.Health, h)
			}
		}
		out = append(out, c)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
	return nil
}

type configuredGuardrail struct {
	Name           string   `json:"name"`
	Type           string   `json:"type"`
	Action         string   `json:"action"`
	Stages         []string `json:"stages"`
	Detect         []string `json:"detect,omitempty"`
	Patterns       []string `json:"patterns,omitempty"`
	Model          string   `json:"model,omitempty"`
	UnsafeIf       string   `json:"unsafe_if,omitempty"`
	WebhookHost    string   `json:"webhook_host,omitempty"`
	Timeout        string   `json:"timeout,omitempty"`
	OnError        string   `json:"on_error,omitempty"`
	VirtualServers []string `json:"virtual_servers"`
}

func (s *Server) listGuardrails(w http.ResponseWriter, _ *http.Request) error {
	cfg := s.rt.Load().cfg
	out := []configuredGuardrail{}
	for _, name := range slices.Sorted(maps.Keys(cfg.Guardrails)) {
		g := cfg.Guardrails[name]
		c := configuredGuardrail{
			Name: name, Type: g.Type, Action: g.Action, Stages: g.Stages, Detect: g.Detect, Patterns: g.Patterns,
			Model: g.Model, UnsafeIf: g.UnsafeIf, WebhookHost: host(g.URL), Timeout: duration(g.Timeout), OnError: g.OnError,
			VirtualServers: []string{},
		}
		for _, vs := range slices.Sorted(maps.Keys(cfg.MCP.VirtualServers)) {
			if slices.Contains(cfg.MCP.VirtualServers[vs].Guardrails, name) {
				c.VirtualServers = append(c.VirtualServers, vs)
			}
		}
		out = append(out, c)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
	return nil
}

type configuredWebhook struct {
	Name   string   `json:"name"`
	Format string   `json:"format"`
	Events []string `json:"events"`
	Org    string   `json:"org,omitempty"`
	Host   string   `json:"host"`
	Signed bool     `json:"signed"`
}

// listWebhooks is for owners: a webhook can be limited to one
// organization, and its name and host say whom it notifies.
func (s *Server) listWebhooks(w http.ResponseWriter, r *http.Request) error {
	if err := requireOwner(r); err != nil {
		return err
	}
	cfg := s.rt.Load().cfg
	out := []configuredWebhook{}
	for _, h := range cfg.Alerts.Webhooks {
		out = append(out, configuredWebhook{Name: h.Name, Format: h.Format, Events: h.Events, Org: h.Org, Host: host(h.URL), Signed: h.Secret != ""})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "budget_thresholds": cfg.Alerts.BudgetThresholds})
	return nil
}

type staticKey struct {
	Name string `json:"name"`
	policy.Policy
}

// settings reports the gateway's own settings, for owners.
func (s *Server) settings(w http.ResponseWriter, r *http.Request) error {
	if err := requireOwner(r); err != nil {
		return err
	}
	cfg := s.rt.Load().cfg
	keys := []staticKey{}
	for _, k := range cfg.Keys {
		keys = append(keys, staticKey{Name: k.Name, Policy: k.Policy})
	}
	out := map[string]any{
		"version": Version,
		"store": map[string]any{
			"driver": cfg.Store.Driver, "refresh_interval": duration(cfg.Store.RefreshInterval), "usage_retention": duration(cfg.Store.UsageRetention),
		},
		"limiter": map[string]any{
			"driver": cfg.Limiter.Driver, "fail_open_rate_limits": cfg.Limiter.FailOpenRateLimits, "fail_open_budgets": cfg.Limiter.FailOpenBudgets,
		},
		"limits": map[string]any{"max_request_bytes": cfg.Limits.MaxRequestBytes},
		"cache": map[string]any{
			"store": cacheStoreName(cfg), "max_entry_bytes": cfg.Cache.MaxEntryBytes, "max_memory_bytes": cfg.Cache.MaxMemoryBytes,
			"semantic": map[string]any{
				"model": cfg.Cache.Semantic.Model, "threshold": cfg.Cache.Semantic.Threshold, "max_entries": cfg.Cache.Semantic.MaxEntries,
			},
		},
		"body_logging": map[string]any{
			"enabled": cfg.BodyLogging.Enabled, "retention": duration(cfg.BodyLogging.Retention), "max_body_bytes": cfg.BodyLogging.MaxBodyBytes,
			"detectors": cfg.BodyLogging.Redact.Builtin, "custom_patterns": len(cfg.BodyLogging.Redact.Patterns),
		},
		"secrets": map[string]any{
			"encryption_key": cfg.Secrets.EncryptionKey != "", "previous_keys": len(cfg.Secrets.PreviousKeys),
		},
		"tls":         cfg.TLS.CertFile != "",
		"metrics":     map[string]any{"listen": cfg.Metrics.Listen},
		"admin_token": cfg.Admin.Token != "",
		"static_keys": keys,
	}
	sso := map[string]any{"session_ttl": duration(cfg.SSO.SessionTTL), "roles": cfg.SSO.Roles}
	if o := cfg.SSO.OIDC; o != nil {
		sso["oidc"] = map[string]any{"issuer": o.Issuer, "client_id": o.ClientID, "redirect_url": o.RedirectURL, "groups_claim": o.GroupsClaim, "scopes": o.Scopes}
	}
	out["sso"] = sso
	mcp := map[string]any{"public_url": cfg.MCP.PublicURL, "refresh_interval": duration(cfg.MCP.RefreshInterval)}
	if o := cfg.MCP.OAuth; o != nil {
		mcp["oauth"] = map[string]any{"issuer": o.Issuer, "audience": o.Audience, "organization": o.Organization, "team_claim": o.TeamClaim, "scopes": o.Scopes}
	}
	out["mcp"] = mcp
	fields, stored, err := s.settingFields(r.Context(), cfg)
	if err != nil {
		return err
	}
	out["fields"], out["stored"], out["startup"], out["detectors"] = fields, stored, config.StartupPaths, redact.Builtins
	writeJSON(w, http.StatusOK, out)
	return nil
}

// settingField is one gateway setting as the Settings page edits it
// (docs/admin-ui-settings.md).
type settingField struct {
	Path    string `json:"path"`
	Value   any    `json:"value"`
	Default any    `json:"default"`
	// Source is where the value comes from: default, file or stored.
	Source string `json:"source"`
}

// defaultSettings are the settings of a config file that sets nothing.
var defaultSettings = func() map[string]any {
	c, err := config.Parse([]byte("{}"))
	if err != nil {
		panic(err)
	}
	return config.SettingsOf(c).Flatten()
}()

// settingFields reports every gateway setting with its value, default
// and source, and the stored settings spec (secrets as "set").
func (s *Server) settingFields(ctx context.Context, cfg *config.Config) ([]settingField, map[string]any, error) {
	stored := map[string]any{}
	if s.store != nil {
		row, err := s.store.GetConfigObject(ctx, config.KindSettings, config.SettingsName)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return nil, nil, err
		default:
			if stored, err = maskedSpec(row); err != nil {
				return nil, nil, err
			}
		}
	}
	raw, _ := json.Marshal(stored)
	inStore := config.FilePaths(raw)
	s.cfgMu.Lock()
	inFile := config.FilePaths(s.file.Raw)
	s.cfgMu.Unlock()
	values := config.SettingsOf(cfg).Flatten()
	fields := []settingField{}
	for _, p := range config.SettingPaths() {
		f := settingField{Path: p, Value: values[p], Default: defaultSettings[p], Source: "default"}
		switch {
		case inStore[p]:
			f.Source = "stored"
		case inFile[p]:
			f.Source = "file"
		}
		fields = append(fields, f)
	}
	return fields, stored, nil
}

// host returns only the scheme and host of a secret URL: paths and
// queries of webhook URLs often carry tokens.
func host(u config.Secret) string {
	if u == "" {
		return ""
	}
	parsed, err := url.Parse(u.Value())
	if err != nil || parsed.Host == "" {
		return "set"
	}
	return parsed.Scheme + "://" + parsed.Host
}

// duration formats a configured duration the way config files write
// them: 30d, 5m, 1h30m, 2s.
func duration(d config.Duration) string { return d.Format() }
