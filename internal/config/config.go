// Package config loads and validates the gateway's YAML configuration.
// See docs/config.md for the reference.
package config

import (
	"context"
	"crypto/rsa"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/rnikadi/genaiopengateway/internal/policy"
	"github.com/rnikadi/genaiopengateway/internal/redact"
	"github.com/rnikadi/genaiopengateway/internal/secretbox"
)

// Config is the root of the configuration file.
type Config struct {
	// Raw is the config file as read, for merging stored objects into.
	Raw []byte `yaml:"-"`

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
	A2A       A2A                 `yaml:"a2a"`
	SSO       SSO                 `yaml:"sso"`
	UI        UI                  `yaml:"ui"`
	Alerts    Alerts              `yaml:"alerts"`
	// Streams copy usage, audit and other records to outside systems as
	// they are written (docs/streams.md).
	Streams     map[string]Stream    `yaml:"streams"`
	Secrets     Secrets              `yaml:"secrets"`
	BodyLogging BodyLogging          `yaml:"body_logging"`
	Guardrails  map[string]Guardrail `yaml:"guardrails"`
	Cache       Cache                `yaml:"cache"`
	Routing     Routing              `yaml:"routing"`
	// ControlPlane makes this replica a data plane that reads tenancy
	// and stored configuration from a control plane instead of a store;
	// DataPlanes lets data planes read them from this one (ADR 0034).
	ControlPlane ControlPlane `yaml:"control_plane"`
	DataPlanes   DataPlanes   `yaml:"data_planes"`
}

// ControlPlane connects a data plane to its control plane
// (docs/control-plane.md).
type ControlPlane struct {
	// URL is the control plane's base URL.
	URL   string `yaml:"url"`
	Token Secret `yaml:"token"`
	// CacheFile keeps the last snapshot, so a data plane that restarts
	// while the control plane is down still serves.
	CacheFile string `yaml:"cache_file"`
}

// DataPlanes configures this replica as a control plane.
type DataPlanes struct {
	// Token is what data planes present; empty turns the control plane
	// endpoints off.
	Token Secret `yaml:"token"`
}

// minPlaneToken is the shortest token between planes.
const minPlaneToken = 32

// Cache configures the response cache (docs/cache.md). Policies turn it
// on.
type Cache struct {
	// MaxEntryBytes bounds a cached answer; larger ones are not stored.
	MaxEntryBytes int64 `yaml:"max_entry_bytes"`
	// MaxMemoryBytes bounds the in-memory store, used without Valkey.
	MaxMemoryBytes int64         `yaml:"max_memory_bytes"`
	Semantic       SemanticCache `yaml:"semantic"`
}

// SemanticCache configures matching by meaning.
type SemanticCache struct {
	// Model is a virtual model that serves embeddings.
	Model      string  `yaml:"model"`
	Threshold  float64 `yaml:"threshold"`
	MaxEntries int     `yaml:"max_entries"`
}

// Secrets holds the key that encrypts sensitive values in the store
// (ADR 0017). PreviousKeys still decrypt, for rotation.
type Secrets struct {
	EncryptionKey Secret   `yaml:"encryption_key"`
	PreviousKeys  []Secret `yaml:"previous_keys"`
	// KMS supplies the keys instead, wrapped by a key management service
	// and unwrapped at startup (docs/secrets.md).
	KMS *KMS `yaml:"kms"`
}

// KMS providers.
const (
	KMSAWS   = "aws"
	KMSGCP   = "gcp"
	KMSVault = "vault"
)

// KMS names the key that wraps the gateway's encryption keys, and the
// wrapped keys. The plaintext keys exist only in memory.
type KMS struct {
	Provider string `yaml:"provider"`
	// Key is an AWS KMS key ARN, a Google Cloud KMS key resource name
	// (projects/…/cryptoKeys/…), or a Vault Transit key name.
	Key        string   `yaml:"key"`
	WrappedKey string   `yaml:"wrapped_key"`
	Previous   []string `yaml:"previous_wrapped_keys"`
	// Endpoint overrides the service's URL, such as a VPC endpoint. For
	// Vault it is the server address; VAULT_ADDR is the default.
	Endpoint string `yaml:"endpoint"`
	// Mount is Vault's Transit mount, default transit. Role logs in to
	// Vault with the pod's Kubernetes service account through AuthMount
	// (default kubernetes); without it, VAULT_TOKEN is used.
	Mount     string `yaml:"mount"`
	Role      string `yaml:"role"`
	AuthMount string `yaml:"auth_mount"`
}

// Unwrapper decrypts a wrapped key with a KMS. The gateway registers one
// at startup; see RegisterUnwrapper.
type Unwrapper func(ctx context.Context, k KMS, wrapped string) ([]byte, error)

var (
	unwrapper   Unwrapper
	unwrapCache sync.Map // KMS provider, key and wrapped key to plaintext key
)

// RegisterUnwrapper sets how secrets.kms keys are unwrapped. Each
// wrapped key is unwrapped once per process: config reloads reuse it.
func RegisterUnwrapper(u Unwrapper) { unwrapper = u }

// resolveKMS fills EncryptionKey and PreviousKeys from secrets.kms.
func (c *Config) resolveKMS(add func(string, ...any)) {
	k := c.Secrets.KMS
	if k == nil {
		return
	}
	if c.Secrets.EncryptionKey != "" || len(c.Secrets.PreviousKeys) > 0 {
		add("secrets: use encryption_key or kms, not both")
		return
	}
	switch k.Provider {
	case KMSAWS, KMSGCP, KMSVault:
	default:
		add("secrets.kms.provider must be aws, gcp or vault, got %q", k.Provider)
		return
	}
	if k.Key == "" || k.WrappedKey == "" {
		add("secrets.kms needs key and wrapped_key")
		return
	}
	if unwrapper == nil {
		add("secrets.kms is not available in this build")
		return
	}
	for i, w := range append([]string{k.WrappedKey}, k.Previous...) {
		cacheKey := k.Provider + "\x00" + k.Key + "\x00" + w
		plain, ok := unwrapCache.Load(cacheKey)
		if !ok {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			b, err := unwrapper(ctx, *k, w)
			cancel()
			if err != nil {
				add("secrets.kms: unwrapping key %d: %v", i, err)
				return
			}
			plain = Secret(base64.StdEncoding.EncodeToString(b))
			unwrapCache.Store(cacheKey, plain)
		}
		if i == 0 {
			c.Secrets.EncryptionKey = plain.(Secret)
		} else {
			c.Secrets.PreviousKeys = append(c.Secrets.PreviousKeys, plain.(Secret))
		}
	}
}

// BodyLogging configures opt-in logging of request and response bodies
// (docs/body-logging.md). Policies choose which requests are logged.
type BodyLogging struct {
	Enabled      bool     `yaml:"enabled"`
	Retention    Duration `yaml:"retention"`
	MaxBodyBytes int      `yaml:"max_body_bytes"`
	Redact       Redact   `yaml:"redact"`
}

// Redact selects redactions; a nil Builtin means all of them.
type Redact struct {
	Builtin  []string `yaml:"builtin"`
	Patterns []string `yaml:"patterns"`
}

// Body logging defaults and bounds.
const (
	DefaultBodyRetention = 30 * 24 * time.Hour
	MaxBodyRetention     = 365 * 24 * time.Hour
	DefaultMaxBodyBytes  = 64 << 10
)

// Guardrail is one named check on model and MCP calls
// (docs/guardrails.md). Policies and virtual servers attach it by name.
type Guardrail struct {
	Type   string   `yaml:"type"`
	Action string   `yaml:"action"`
	Stages []string `yaml:"stages"`
	// Detect names the pii detectors; nil means all of them.
	Detect []string `yaml:"detect"`
	// Patterns are the RE2 expressions of a regex guardrail.
	Patterns []string `yaml:"patterns"`
	// Model, Prompt and UnsafeIf configure a model guardrail.
	Model    string `yaml:"model"`
	Prompt   string `yaml:"prompt"`
	UnsafeIf string `yaml:"unsafe_if"`
	// URL and Secret configure a webhook guardrail.
	URL     Secret   `yaml:"url"`
	Secret  Secret   `yaml:"secret"`
	Timeout Duration `yaml:"timeout"`
	OnError string   `yaml:"on_error"`
}

// Guardrail types, actions, stages and error modes.
const (
	GuardrailPII     = "pii"
	GuardrailRegex   = "regex"
	GuardrailModel   = "model"
	GuardrailWebhook = "webhook"

	ActionBlock  = "block"
	ActionRedact = "redact"
	ActionLog    = "log"

	StageInput      = "input"
	StageOutput     = "output"
	StageToolInput  = "tool_input"
	StageToolOutput = "tool_output"

	OnErrorAllow = "allow"
	OnErrorBlock = "block"
)

// Guardrail defaults and bounds.
const (
	DefaultGuardrailTimeout = 2 * time.Second
	MaxGuardrailTimeout     = 30 * time.Second
	DefaultUnsafeIf         = `(?i)^\s*unsafe`
)

// External reports whether a guardrail calls out of process.
func (g *Guardrail) External() bool {
	return g.Type == GuardrailModel || g.Type == GuardrailWebhook
}

// Alerts configures budget alerts and the webhooks that receive alerts
// (docs/alerts.md).
type Alerts struct {
	// BudgetThresholds are shares of a budget at which to alert.
	BudgetThresholds []float64 `yaml:"budget_thresholds"`
	// SpendAnomaly tunes the spend.anomaly alert (docs/forecast.md).
	SpendAnomaly SpendAnomaly `yaml:"spend_anomaly"`
	Webhooks     []Webhook    `yaml:"webhooks"`
}

// SpendAnomaly says when a day's spend counts as unusual: at least
// Factor times the daily average of the two weeks before, and at least
// MinUSD. A Factor of 0 turns the alert off.
type SpendAnomaly struct {
	Factor float64 `yaml:"factor"`
	MinUSD float64 `yaml:"min_usd"`
}

// Alert events.
const (
	EventBudgetThreshold = "budget.threshold"
	EventMCPToolPending  = "mcp.tool_pending"
	EventKeyExpiring     = "key.expiring"
	EventBudgetForecast  = "budget.forecast"
	EventSpendAnomaly    = "spend.anomaly"
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

// Stream types and record types (docs/streams.md).
const (
	StreamWebhook = "webhook"
	StreamFile    = "file"

	RecordUsage           = "usage"
	RecordAudit           = "audit"
	RecordMCPCalls        = "mcp_calls"
	RecordGuardrailEvents = "guardrail_events"
	RecordDenials         = "denials"

	DefaultStreamBatchSize     = 500
	DefaultStreamFlushInterval = 5 * time.Second
	MaxStreamBatchSize         = 10_000
)

// RecordTypes lists what a stream can carry.
var RecordTypes = []string{RecordUsage, RecordAudit, RecordMCPCalls, RecordGuardrailEvents, RecordDenials}

// Stream sends records to a webhook as signed NDJSON batches, or appends
// them to a JSON Lines file for a log shipper.
type Stream struct {
	Type    string            `yaml:"type"`
	URL     Secret            `yaml:"url"`
	Secret  Secret            `yaml:"secret"`
	Headers map[string]Secret `yaml:"headers"`
	Path    string            `yaml:"path"`
	Records []string          `yaml:"records"`
	// BatchSize and FlushInterval bound how many records a delivery
	// holds and how long a record waits for one.
	BatchSize     int      `yaml:"batch_size"`
	FlushInterval Duration `yaml:"flush_interval"`
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
	OIDC *OIDC `yaml:"oidc"`
	// SAML signs operators in with a SAML 2.0 identity provider instead
	// of OIDC (docs/saml.md).
	SAML       *SAML         `yaml:"saml"`
	SessionTTL Duration      `yaml:"session_ttl"`
	Roles      []RoleMapping `yaml:"roles"`
	// CustomRoles are named sets of permissions that role mappings can
	// grant besides the built-in roles (docs/roles.md).
	CustomRoles map[string]CustomRole `yaml:"custom_roles"`
	// SCIM lets the identity provider provision users and groups
	// (docs/scim.md).
	SCIM SCIM `yaml:"scim"`
	// Portal limits what developers do for themselves in the developer
	// portal (docs/developer-portal.md).
	Portal Portal `yaml:"portal"`
}

// Portal limits developers' own keys.
type Portal struct {
	// MaxKeysPerUser is how many unrevoked keys one person may own.
	MaxKeysPerUser int `yaml:"max_keys_per_user"`
	// MaxKeyTTL is the longest expiry a developer may give a key.
	MaxKeyTTL Duration `yaml:"max_key_ttl"`
}

// Enabled reports whether operators sign in with an identity provider.
func (s *SSO) Enabled() bool { return s.OIDC != nil || s.SAML != nil }

// SAML is a SAML 2.0 identity provider operators sign in with. The
// gateway is the service provider; its metadata is served at
// /ui/auth/saml/metadata.
type SAML struct {
	// IDPMetadataURL or IDPMetadata (the XML itself) describe the
	// identity provider: its sign-in URL and signing certificates.
	IDPMetadataURL string `yaml:"idp_metadata_url"`
	IDPMetadata    string `yaml:"idp_metadata"`
	// ACSURL is the gateway's URL ending in /ui/auth/saml/acs, where the
	// identity provider posts its response.
	ACSURL string `yaml:"acs_url"`
	// EntityID names the gateway to the identity provider; the default is
	// its metadata URL.
	EntityID string `yaml:"entity_id"`
	// EmailAttribute, NameAttribute and GroupsAttribute name the
	// assertion's attributes. Without an email attribute, the NameID is
	// the email.
	EmailAttribute  string `yaml:"email_attribute"`
	NameAttribute   string `yaml:"name_attribute"`
	GroupsAttribute string `yaml:"groups_attribute"`
	// SPCertificate and SPPrivateKey are the gateway's own key pair, as
	// PEM (gateway saml-keygen makes one). With them the gateway signs
	// its requests, takes encrypted assertions and supports single
	// logout.
	SPCertificate string `yaml:"sp_certificate"`
	SPPrivateKey  Secret `yaml:"sp_private_key"`
	// RequireEncryptedAssertions refuses a response whose assertion is
	// not encrypted to the gateway's key.
	RequireEncryptedAssertions bool `yaml:"require_encrypted_assertions"`
}

// SPKeyPair returns the gateway's SAML key pair, or nil without one.
func (m *SAML) SPKeyPair() (*tls.Certificate, error) {
	if m.SPCertificate == "" && m.SPPrivateKey == "" {
		return nil, nil
	}
	if m.SPCertificate == "" || m.SPPrivateKey == "" {
		return nil, errors.New("set both sp_certificate and sp_private_key")
	}
	pair, err := tls.X509KeyPair(NormalizePEM(m.SPCertificate), NormalizePEM(m.SPPrivateKey.Value()))
	if err != nil {
		return nil, err
	}
	if _, ok := pair.PrivateKey.(*rsa.PrivateKey); !ok {
		return nil, errors.New("the key must be RSA, which assertion encryption needs")
	}
	return &pair, nil
}

var pemBlock = regexp.MustCompile(`-----BEGIN ([A-Z0-9 ]+)-----([A-Za-z0-9+/=\s]*)-----END ([A-Z0-9 ]+)-----`)

// NormalizePEM rewraps PEM blocks whose line breaks were lost, as when a
// key is pasted into a one-line field or an environment variable.
func NormalizePEM(s string) []byte {
	return []byte(pemBlock.ReplaceAllStringFunc(s, func(b string) string {
		m := pemBlock.FindStringSubmatch(b)
		body := strings.Join(strings.Fields(m[2]), "")
		var out strings.Builder
		out.WriteString("-----BEGIN " + m[1] + "-----\n")
		for len(body) > 64 {
			out.WriteString(body[:64] + "\n")
			body = body[64:]
		}
		if body != "" {
			out.WriteString(body + "\n")
		}
		out.WriteString("-----END " + m[3] + "-----\n")
		return out.String()
	}))
}

// SCIM turns on the SCIM 2.0 endpoints under /scim/v2 when Token is set.
type SCIM struct {
	Token Secret `yaml:"token"`
}

// Enabled reports whether the SCIM endpoints are served.
func (s SCIM) Enabled() bool { return s.Token.Value() != "" }

// CustomRole is a named set of permissions.
type CustomRole struct {
	Description string   `yaml:"description" json:"description,omitempty"`
	Permissions []string `yaml:"permissions" json:"permissions"`
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
	// RoleDeveloper uses the developer portal: own keys within the
	// team's policy, and the playground (docs/developer-portal.md).
	RoleDeveloper = "developer"
)

// Permissions an operator role can hold, each over the organizations or
// the team it is granted for (docs/roles.md). Owners hold them all, and
// also manage the gateway itself.
const (
	PermOrgRead          = "org.read"
	PermTeamsRead        = "teams.read"
	PermTeamsWrite       = "teams.write"
	PermKeysRead         = "keys.read"
	PermKeysWrite        = "keys.write"
	PermCredentialsWrite = "credentials.write"
	PermUsageRead        = "usage.read"
	PermAuditRead        = "audit.read"
	PermAlertsRead       = "alerts.read"
	PermBodiesRead       = "bodies.read"
	PermBatchesWrite     = "batches.write"
	PermCacheClear       = "cache.clear"
	PermPlayground       = "playground.use"
	PermKeysSelf         = "keys.self"
)

// Permissions lists every permission, in the order the docs give them.
var Permissions = []string{
	PermOrgRead, PermTeamsRead, PermTeamsWrite, PermKeysRead, PermKeysWrite, PermCredentialsWrite,
	PermUsageRead, PermAuditRead, PermAlertsRead, PermBodiesRead, PermBatchesWrite, PermCacheClear, PermPlayground, PermKeysSelf,
}

// developerPermissions are what the developer role holds.
var developerPermissions = []string{PermKeysSelf, PermPlayground}

// viewerPermissions are what the viewer role holds: everything an
// organization's admin reads, except logged bodies.
var viewerPermissions = []string{PermOrgRead, PermTeamsRead, PermKeysRead, PermUsageRead, PermAuditRead, PermAlertsRead}

// RolePermissions returns what a role may do, or nil for an unknown role.
func (s *SSO) RolePermissions(role string) []string {
	switch role {
	case RoleOwner, RoleAdmin:
		return Permissions
	case RoleViewer:
		return viewerPermissions
	case RoleDeveloper:
		return developerPermissions
	}
	if r, ok := s.CustomRoles[role]; ok {
		return r.Permissions
	}
	return nil
}

// RoleMapping grants a role to the members of a group, or to one email,
// gateway-wide, for one organization (named or by id), or for one team
// of that organization.
type RoleMapping struct {
	Group string `yaml:"group"`
	Email string `yaml:"email"`
	Role  string `yaml:"role"`
	Org   string `yaml:"org"`
	Team  string `yaml:"team"`
}

// UI configures the admin UI. It is on whenever the admin API is.
type UI struct {
	Enabled *bool `yaml:"enabled"`
}

// AdminEnabled reports whether the admin API, and with it the UI, is on:
// it needs a store and a way to sign in.
func (c *Config) AdminEnabled() bool {
	return c.Store.Driver != "" && (c.Admin.Token.Value() != "" || c.SSO.Enabled())
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
	// MaxRealtimeSession is how long a realtime session may stay open.
	MaxRealtimeSession Duration `yaml:"max_realtime_session"`
	// MaxBatchFileBytes bounds a batch input file, which the gateway
	// reads into memory to check (docs/batch.md).
	MaxBatchFileBytes int64 `yaml:"max_batch_file_bytes"`
}

// Provider types.
const (
	ProviderOpenAI    = "openai"
	ProviderAnthropic = "anthropic"
	ProviderGemini    = "gemini"
	ProviderVertex    = "vertex"
	ProviderBedrock   = "bedrock"
	// Speech providers with their own APIs (docs/providers.md).
	ProviderElevenLabs = "elevenlabs"
	ProviderDeepgram   = "deepgram"
	ProviderAssemblyAI = "assemblyai"
	// Image providers with their own APIs.
	ProviderStability = "stability"
	ProviderFal       = "fal"
	// ProviderCohere serves Cohere chat, embeddings and rerank.
	ProviderCohere = "cohere"

	// DefaultGeminiBaseURL is the Gemini API.
	DefaultGeminiBaseURL = "https://generativelanguage.googleapis.com"
)

// defaultBaseURLs are the endpoints of provider types that have one
// public API, used when base_url is empty.
var defaultBaseURLs = map[string]string{
	ProviderGemini:     DefaultGeminiBaseURL,
	ProviderElevenLabs: "https://api.elevenlabs.io",
	ProviderDeepgram:   "https://api.deepgram.com",
	ProviderAssemblyAI: "https://api.assemblyai.com",
	ProviderStability:  "https://api.stability.ai",
	ProviderFal:        "https://fal.run",
	ProviderCohere:     "https://api.cohere.com",
}

// ProviderTypes lists the provider types.
var ProviderTypes = []string{ProviderOpenAI, ProviderAnthropic, ProviderGemini, ProviderVertex, ProviderBedrock, ProviderElevenLabs, ProviderDeepgram, ProviderAssemblyAI, ProviderStability, ProviderFal, ProviderCohere}

// Provider is an upstream that serves models.
type Provider struct {
	Type    string            `yaml:"type"`
	BaseURL string            `yaml:"base_url"`
	APIKey  Secret            `yaml:"api_key"`
	Timeout Duration          `yaml:"timeout"`
	Headers map[string]string `yaml:"headers"`
	// Project, Location and CredentialsFile configure a vertex provider.
	// Without CredentialsFile, Application Default Credentials are used.
	Project         string `yaml:"project"`
	Location        string `yaml:"location"`
	CredentialsFile string `yaml:"credentials_file"`
	// Region configures a bedrock provider. Without APIKey, requests are
	// signed with credentials from the AWS default chain.
	Region string `yaml:"region"`
	// S3OutputURI is where a bedrock provider's Nova Reel jobs write
	// their videos, such as s3://bucket/videos, in the provider's region.
	S3OutputURI string `yaml:"s3_output_uri"`
	// Residency labels where the provider processes data, such as eu or
	// us. Organizations, teams and keys can be pinned to labels
	// (docs/routing.md#data-residency).
	Residency string `yaml:"residency"`
}

// Routing strategies of a virtual model (docs/routing.md).
const (
	StrategyOrdered      = "ordered"
	StrategyWeighted     = "weighted"
	StrategyRoundRobin   = "round_robin"
	StrategyLeastLatency = "least_latency"
	StrategyLowestCost   = "lowest_cost"
)

// Strategies lists the routing strategies.
var Strategies = []string{StrategyOrdered, StrategyWeighted, StrategyRoundRobin, StrategyLeastLatency, StrategyLowestCost}

// Model is a virtual model. Strategy chooses the order its targets are
// tried in; the default tries them in the order listed.
type Model struct {
	Targets  []Target `yaml:"targets" json:"targets"`
	Strategy string   `yaml:"strategy" json:"strategy,omitempty"`
	// Sticky "user" sends a caller's requests to the same first target
	// while it is healthy, keyed by the request's user field, so provider
	// prompt caches stay warm. Only with weighted and round_robin.
	Sticky  string  `yaml:"sticky" json:"sticky,omitempty"`
	Retries Retries `yaml:"retries" json:"retries,omitempty"`
}

// Retries are extra tries on the same target before moving on, after a
// 429, a 5xx or a connection failure, waiting a jittered exponential
// backoff or the upstream's Retry-After when it is shorter than MaxBackoff.
type Retries struct {
	Attempts   int      `yaml:"attempts" json:"attempts,omitempty"`
	Backoff    Duration `yaml:"backoff" json:"backoff,omitempty"`
	MaxBackoff Duration `yaml:"max_backoff" json:"max_backoff,omitempty"`
}

// Target maps a virtual model onto one provider's model.
type Target struct {
	Provider string `yaml:"provider" json:"provider"`
	Model    string `yaml:"model" json:"model"`
	Price    Price  `yaml:"price" json:"price"`
	// Weight is the target's share of traffic under the weighted
	// strategy; zero counts as 1.
	Weight int `yaml:"weight" json:"weight,omitempty"`
	// Timeout bounds the wait for response headers, after which the next
	// target is tried. Zero leaves only the provider's timeout.
	Timeout Duration `yaml:"timeout" json:"timeout,omitempty"`
}

// Routing holds gateway-wide routing settings (docs/routing.md).
type Routing struct {
	CircuitBreaker CircuitBreaker `yaml:"circuit_breaker"`
}

// CircuitBreaker takes a target out of rotation after Failures failures
// in a row, for Cooldown, then lets one probe request through to decide
// whether it is back. Failures of 0 turns it off.
type CircuitBreaker struct {
	Failures int      `yaml:"failures"`
	Cooldown Duration `yaml:"cooldown"`
}

// String identifies the target as provider/model.
func (t Target) String() string { return t.Provider + "/" + t.Model }

// Price is the cost of a target in USD per million tokens.
type Price struct {
	InputPerMTok       float64 `yaml:"input_per_mtok" json:"input_per_mtok"`
	OutputPerMTok      float64 `yaml:"output_per_mtok" json:"output_per_mtok"`
	CachedInputPerMTok float64 `yaml:"cached_input_per_mtok" json:"cached_input_per_mtok"`
	// Media prices (docs/modalities.md); they add to the token prices.
	PerImage       float64 `yaml:"per_image" json:"per_image,omitempty"`
	PerMChars      float64 `yaml:"per_mchars" json:"per_mchars,omitempty"`
	PerAudioMinute float64 `yaml:"per_audio_minute" json:"per_audio_minute,omitempty"`
	PerVideoSecond float64 `yaml:"per_video_second" json:"per_video_second,omitempty"`
	PerRequest     float64 `yaml:"per_request" json:"per_request,omitempty"`
	// Audio token prices, for realtime; audio tokens use the token
	// prices above when they are unset.
	AudioInputPerMTok  float64 `yaml:"audio_input_per_mtok" json:"audio_input_per_mtok,omitempty"`
	AudioOutputPerMTok float64 `yaml:"audio_output_per_mtok" json:"audio_output_per_mtok,omitempty"`
	// BatchDiscount is the fraction taken off batch requests, such as 0.5
	// for half price (docs/batch.md).
	BatchDiscount float64 `yaml:"batch_discount" json:"batch_discount,omitempty"`
}

func (p Price) negative() bool {
	return min(p.InputPerMTok, p.OutputPerMTok, p.CachedInputPerMTok, p.PerImage, p.PerMChars, p.PerAudioMinute, p.PerVideoSecond, p.PerRequest, p.AudioInputPerMTok, p.AudioOutputPerMTok, p.BatchDiscount) < 0
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
	// MaxUserSessions caps upstream sessions held for user_auth servers
	// per replica; the least recently used is closed first.
	MaxUserSessions int `yaml:"max_user_sessions"`
}

// A2A configures agents served through the gateway with the Agent2Agent
// protocol (docs/a2a.md, ADR 0033).
type A2A struct {
	// PublicURL is the gateway's external URL, which agent cards name.
	// Default: mcp.public_url, or else the URL of each card request.
	PublicURL string              `yaml:"public_url"`
	Agents    map[string]A2AAgent `yaml:"agents"`
}

// A2AAgent is an upstream A2A agent reached over JSON-RPC.
type A2AAgent struct {
	// URL is the agent's JSON-RPC endpoint.
	URL string `yaml:"url"`
	// CardURL is where the agent's card is. Default: the URL's origin
	// with /.well-known/agent-card.json.
	CardURL     string            `yaml:"card_url"`
	Description string            `yaml:"description"`
	Headers     map[string]Secret `yaml:"headers"`
	// Timeout bounds a call that does not stream, and the wait for a
	// stream to start. Default: 5m.
	Timeout    Duration `yaml:"timeout"`
	Guardrails []string `yaml:"guardrails"`
}

// DefaultA2ATimeout bounds an agent call without a timeout of its own.
const DefaultA2ATimeout = 5 * time.Minute

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
	URL string `yaml:"url"`
	// Command runs a stdio MCP server as a child process instead of
	// calling URL (docs/mcp.md#hosted-stdio-servers). It sees only Env,
	// PATH and HOME, and runs in Dir. Set only in the config file.
	Command []string          `yaml:"command"`
	Env     map[string]Secret `yaml:"env"`
	Dir     string            `yaml:"dir"`

	Headers  map[string]Secret `yaml:"headers"`
	Timeout  Duration          `yaml:"timeout"`
	PinTools PinMode           `yaml:"pin_tools"`
	// UserAuth makes the gateway call this server with each user's own
	// OAuth token (docs/mcp-user-credentials.md).
	UserAuth *UserAuth `yaml:"user_auth"`
}

// UserAuth registers the gateway as an OAuth client of an upstream MCP
// server. Endpoints left empty are discovered from the server's metadata.
type UserAuth struct {
	ClientID         string   `yaml:"client_id"`
	ClientSecret     Secret   `yaml:"client_secret"`
	Scopes           []string `yaml:"scopes"`
	AuthorizationURL string   `yaml:"authorization_url"`
	TokenURL         string   `yaml:"token_url"`
	RevocationURL    string   `yaml:"revocation_url"`
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
	// Guardrails apply to every tool call through the virtual server.
	Guardrails []string `yaml:"guardrails"`
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
	DefaultBreakerFailures = 5
	DefaultBreakerCooldown = 30 * time.Second
	DefaultRetryBackoff    = 250 * time.Millisecond
	DefaultRetryMaxBackoff = 5 * time.Second
	MaxRetryAttempts       = 5
	DefaultMaxRequestBytes = 32 << 20
	// DefaultMaxRealtimeSession bounds a realtime session.
	DefaultMaxRealtimeSession = Duration(30 * time.Minute)
	DefaultMaxBatchFileBytes  = 100 << 20
	DefaultCacheEntryBytes    = 1 << 20
	DefaultCacheMemoryBytes   = 256 << 20
	DefaultCacheThreshold     = 0.95
	DefaultCacheVectors       = 10000
	DefaultProviderTimeout    = 5 * time.Minute
	DefaultRefreshInterval    = 5 * time.Second
	DefaultMCPRefresh         = time.Minute
	DefaultMCPTimeout         = time.Minute
	DefaultMaxUserSessions    = 1000
	DefaultSessionTTL         = 8 * time.Hour
	DefaultPortalMaxKeys      = 5
	DefaultPortalKeyTTL       = 90 * 24 * time.Hour
	MinAdminTokenLength       = 32
	MinStaticKeyLength        = 16
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
	c, err := ParseWith(raw, nil)
	return c, err
}

// decode applies defaults, decodes the file's root mapping over them and
// validates the result.
func decode(root *yaml.Node) (*Config, error) {
	c := Config{
		Listen:  DefaultListen,
		Limits:  Limits{MaxRequestBytes: DefaultMaxRequestBytes, MaxRealtimeSession: DefaultMaxRealtimeSession, MaxBatchFileBytes: DefaultMaxBatchFileBytes},
		Limiter: Limiter{Driver: "memory", FailOpenRateLimits: true},
		Cache: Cache{MaxEntryBytes: DefaultCacheEntryBytes, MaxMemoryBytes: DefaultCacheMemoryBytes,
			Semantic: SemanticCache{Threshold: DefaultCacheThreshold, MaxEntries: DefaultCacheVectors}},
		Store:       Store{RefreshInterval: Duration(DefaultRefreshInterval)},
		MCP:         MCP{RefreshInterval: Duration(DefaultMCPRefresh), MaxUserSessions: DefaultMaxUserSessions},
		SSO:         SSO{SessionTTL: Duration(DefaultSessionTTL), Portal: Portal{MaxKeysPerUser: DefaultPortalMaxKeys, MaxKeyTTL: Duration(DefaultPortalKeyTTL)}},
		Alerts:      Alerts{BudgetThresholds: DefaultBudgetThresholds, SpendAnomaly: SpendAnomaly{Factor: 3, MinUSD: 5}},
		BodyLogging: BodyLogging{Retention: Duration(DefaultBodyRetention), MaxBodyBytes: DefaultMaxBodyBytes},
		Routing:     Routing{CircuitBreaker: CircuitBreaker{Failures: DefaultBreakerFailures, Cooldown: Duration(DefaultBreakerCooldown)}},
	}
	if err := root.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
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
	c.resolveKMS(add)

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
	if c.Limits.MaxRealtimeSession <= 0 {
		add("limits.max_realtime_session must be positive")
	}
	if c.Limits.MaxBatchFileBytes <= 0 {
		add("limits.max_batch_file_bytes must be positive")
	}
	if c.Cache.MaxEntryBytes <= 0 || c.Cache.MaxMemoryBytes <= 0 || c.Cache.Semantic.MaxEntries <= 0 {
		add("cache.max_entry_bytes, cache.max_memory_bytes and cache.semantic.max_entries must be positive")
	}
	if t := c.Cache.Semantic.Threshold; t <= 0 || t > 1 {
		add("cache.semantic.threshold must be above 0 and at most 1")
	}
	if m := c.Cache.Semantic.Model; m != "" {
		if _, ok := c.Models[m]; !ok {
			add("cache.semantic.model %q is not configured", m)
		}
	}

	for name, p := range c.Providers {
		switch p.Type {
		case "":
			p.Type = ProviderOpenAI
		case ProviderOpenAI, ProviderAnthropic:
		case ProviderGemini, ProviderElevenLabs, ProviderDeepgram, ProviderAssemblyAI, ProviderStability, ProviderFal, ProviderCohere:
			if p.BaseURL == "" {
				p.BaseURL = defaultBaseURLs[p.Type]
			}
		case ProviderVertex:
			if p.Project == "" || p.Location == "" {
				add("provider %q: vertex needs project and location", name)
			} else if !vertexLocation.MatchString(p.Location) || !vertexProject.MatchString(p.Project) {
				add("provider %q: project or location has characters a Google Cloud name cannot have", name)
			}
		case ProviderBedrock:
			if !awsRegion.MatchString(p.Region) {
				add("provider %q: bedrock needs a region such as us-east-1", name)
			}
			if u := p.S3OutputURI; u != "" && (!strings.HasPrefix(u, "s3://") || len(strings.TrimPrefix(u, "s3://")) < 3) {
				add("provider %q: s3_output_uri must be an s3:// URI, such as s3://bucket/videos", name)
			}
		default:
			add("provider %q: type must be one of %s, got %q", name, strings.Join(ProviderTypes, ", "), p.Type)
		}
		if p.BaseURL == "" {
			if p.Type != ProviderVertex && p.Type != ProviderBedrock {
				add("provider %q: base_url is required", name)
			}
		} else if u, err := url.Parse(p.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			add("provider %q: base_url must be an http or https URL", name)
		}
		p.BaseURL = strings.TrimRight(p.BaseURL, "/")
		if p.Timeout <= 0 {
			p.Timeout = Duration(DefaultProviderTimeout)
		}
		c.Providers[name] = p
	}

	if c.Routing.CircuitBreaker.Failures < 0 || c.Routing.CircuitBreaker.Cooldown < 0 {
		add("routing.circuit_breaker: failures and cooldown must not be negative")
	}
	if c.Routing.CircuitBreaker.Failures > 0 && c.Routing.CircuitBreaker.Cooldown == 0 {
		c.Routing.CircuitBreaker.Cooldown = Duration(DefaultBreakerCooldown)
	}
	for name, m := range c.Models {
		if len(m.Targets) == 0 {
			add("model %q: at least one target is required", name)
		}
		switch {
		case m.Strategy == "":
			m.Strategy = StrategyOrdered
		case !slices.Contains(Strategies, m.Strategy):
			add("model %q: strategy must be one of %s, got %q", name, strings.Join(Strategies, ", "), m.Strategy)
		}
		switch m.Sticky {
		case "":
		case "user":
			if m.Strategy != StrategyWeighted && m.Strategy != StrategyRoundRobin {
				add("model %q: sticky routing needs the weighted or round_robin strategy", name)
			}
		default:
			add("model %q: sticky must be user or empty, got %q", name, m.Sticky)
		}
		if m.Retries.Attempts < 0 || m.Retries.Attempts > MaxRetryAttempts {
			add("model %q: retries.attempts must be between 0 and %d", name, MaxRetryAttempts)
		}
		if m.Retries.Backoff < 0 || m.Retries.MaxBackoff < 0 {
			add("model %q: retry backoffs must not be negative", name)
		}
		if m.Retries.Backoff == 0 {
			m.Retries.Backoff = Duration(DefaultRetryBackoff)
		}
		if m.Retries.MaxBackoff == 0 {
			m.Retries.MaxBackoff = Duration(DefaultRetryMaxBackoff)
		}
		c.Models[name] = m
		for i, t := range m.Targets {
			if _, ok := c.Providers[t.Provider]; !ok {
				add("model %q target %d: unknown provider %q", name, i, t.Provider)
			}
			if t.Model == "" {
				add("model %q target %d: model is required", name, i)
			}
			if t.Price.negative() {
				add("model %q target %d: prices must not be negative", name, i)
			}
			if t.Price.BatchDiscount > 1 {
				add("model %q target %d: batch_discount must be between 0 and 1", name, i)
			}
			if t.Weight < 0 || t.Timeout < 0 {
				add("model %q target %d: weight and timeout must not be negative", name, i)
			}
		}
	}

	c.validateMCP(add)
	c.validateA2A(add)
	c.validatePlanes(add)
	c.validateSSO(add)
	c.validateAlerts(add)
	c.validateStreams(add)
	c.validateBodyLogging(add)
	c.validateGuardrails(add)

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
		if k.Cache != nil && k.Cache.Mode == policy.CacheSemantic && c.Cache.Semantic.Model == "" {
			add("key %q: the semantic cache needs cache.semantic.model", k.Name)
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
		for _, v := range k.AllowedAgents {
			if _, ok := c.A2A.Agents[v]; !ok {
				add("key %q: allowed agent %q is not configured", k.Name, v)
			}
		}
		for _, g := range k.Guardrails {
			if _, ok := c.Guardrails[g]; !ok {
				add("key %q: guardrail %q is not configured", k.Name, g)
			}
		}
	}
	return errors.Join(errs...)
}

func (c *Config) validateBodyLogging(add func(string, ...any)) {
	for i, k := range append([]Secret{c.Secrets.EncryptionKey}, c.Secrets.PreviousKeys...) {
		if (i == 0 && k == "") || validKey(k.Value()) {
			continue
		}
		add("secrets: an encryption key must be 32 random bytes, base64 encoded (openssl rand -base64 32)")
		break
	}
	if len(c.Secrets.PreviousKeys) > 0 && c.Secrets.EncryptionKey == "" {
		add("secrets.previous_keys needs secrets.encryption_key")
	}
	b := &c.BodyLogging
	if !b.Enabled {
		return
	}
	if c.Store.Driver == "" {
		add("body_logging needs a store")
	}
	if c.Secrets.EncryptionKey == "" {
		add("body_logging needs secrets.encryption_key, to encrypt bodies at rest")
	}
	if r := b.Retention.Std(); r <= 0 || r > MaxBodyRetention {
		add("body_logging.retention must be between 1s and 365d")
	}
	if b.MaxBodyBytes < 1024 || b.MaxBodyBytes > 1<<20 {
		add("body_logging.max_body_bytes must be between 1024 and 1048576")
	}
	if _, err := redact.New(redact.Config{Builtin: b.Redact.Builtin, Patterns: b.Redact.Patterns}); err != nil {
		add("body_logging.redact: %v", err)
	}
}

func (c *Config) validateGuardrails(add func(string, ...any)) {
	for name, g := range c.Guardrails {
		add := func(format string, args ...any) { add("guardrail %q: "+format, append([]any{name}, args...)...) }
		switch g.Type {
		case GuardrailPII:
			if _, err := redact.New(redact.Config{Builtin: g.Detect}); err != nil {
				add("%v", err)
			}
		case GuardrailRegex:
			if len(g.Patterns) == 0 {
				add("patterns must list at least one pattern")
			}
			if _, err := redact.New(redact.Config{Builtin: []string{}, Patterns: g.Patterns}); err != nil {
				add("%v", err)
			}
		case GuardrailModel:
			if _, ok := c.Models[g.Model]; !ok {
				add("model %q is not configured", g.Model)
			}
			if g.UnsafeIf == "" {
				g.UnsafeIf = DefaultUnsafeIf
			}
			if _, err := regexp.Compile(g.UnsafeIf); err != nil {
				add("unsafe_if: %v", err)
			}
		case GuardrailWebhook:
			if !webhookURL(g.URL.Value()) {
				add("url must be https, or http to localhost")
			}
		default:
			add("type must be pii, regex, model or webhook, got %q", g.Type)
		}
		if g.Type == GuardrailWebhook && g.Action == "" {
			g.Action = ActionBlock // the webhook's reply decides: block or redact
		}
		switch g.Action {
		case ActionBlock, ActionLog:
		case ActionRedact:
			if g.Type == GuardrailModel {
				add("a model guardrail cannot redact; use block or log")
			}
		default:
			add("action must be block, redact or log, got %q", g.Action)
		}
		if len(g.Stages) == 0 {
			add("stages must list at least one stage")
		}
		for _, st := range g.Stages {
			switch st {
			case StageInput, StageOutput, StageToolInput, StageToolOutput:
			default:
				add("unknown stage %q", st)
			}
		}
		if g.External() {
			if g.Timeout == 0 {
				g.Timeout = Duration(DefaultGuardrailTimeout)
			}
			if g.Timeout < 0 || g.Timeout.Std() > MaxGuardrailTimeout {
				add("timeout must be at most 30s")
			}
			switch g.OnError {
			case "":
				g.OnError = OnErrorAllow
			case OnErrorAllow, OnErrorBlock:
			default:
				add("on_error must be allow or block, got %q", g.OnError)
			}
		}
		c.Guardrails[name] = g
	}
	for vs, v := range c.MCP.VirtualServers {
		for _, g := range v.Guardrails {
			if _, ok := c.Guardrails[g]; !ok {
				add("mcp virtual server %q: guardrail %q is not configured", vs, g)
			}
		}
	}
}

func validKey(s string) bool {
	_, err := secretbox.ParseKey(s)
	return err == nil
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
	if f := a.SpendAnomaly.Factor; f != 0 && f < 1.5 {
		add("alerts.spend_anomaly.factor must be 0 (off) or at least 1.5, got %v", f)
	}
	if a.SpendAnomaly.MinUSD < 0 {
		add("alerts.spend_anomaly.min_usd must not be negative")
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
			case EventBudgetThreshold, EventMCPToolPending, EventKeyExpiring, EventBudgetForecast, EventSpendAnomaly:
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
	if m := s.SAML; m != nil {
		if s.OIDC != nil {
			add("sso.saml and sso.oidc cannot both be set: choose one identity provider")
		}
		if (m.IDPMetadataURL == "") == (m.IDPMetadata == "") {
			add("sso.saml: set exactly one of idp_metadata_url and idp_metadata")
		}
		if m.IDPMetadataURL != "" && !httpURL(m.IDPMetadataURL) {
			add("sso.saml.idp_metadata_url must be an http or https URL")
		}
		u, err := url.Parse(m.ACSURL)
		if err != nil || !httpURL(m.ACSURL) || u.Path != "/ui/auth/saml/acs" {
			add("sso.saml.acs_url must be the gateway's URL ending in /ui/auth/saml/acs")
		} else if m.EntityID == "" {
			m.EntityID = strings.TrimSuffix(m.ACSURL, "/acs") + "/metadata"
		}
		if m.GroupsAttribute == "" {
			m.GroupsAttribute = "groups"
		}
		pair, err := m.SPKeyPair()
		if err != nil {
			add("sso.saml.sp_certificate and sp_private_key: %v", err)
		} else if pair == nil && m.RequireEncryptedAssertions {
			add("sso.saml.require_encrypted_assertions needs sp_certificate and sp_private_key, which the identity provider encrypts to")
		}
		if len(s.Roles) == 0 {
			add("sso.roles is required with sso.saml: without roles nobody can sign in")
		}
		if c.Store.Driver == "" {
			add("sso.saml needs a store, where sessions are kept")
		}
	}
	if p := s.Portal; p.MaxKeysPerUser < 1 || p.MaxKeysPerUser > 100 {
		add("sso.portal.max_keys_per_user must be between 1 and 100")
	} else if p.MaxKeyTTL < Duration(time.Hour) || p.MaxKeyTTL > Duration(366*24*time.Hour) {
		add("sso.portal.max_key_ttl must be between 1h and 366d")
	}
	if s.SessionTTL < Duration(time.Minute) {
		add("sso.session_ttl must be at least 1m")
	}
	if s.SCIM.Enabled() {
		if len(s.SCIM.Token.Value()) < 32 {
			add("sso.scim.token must be at least 32 characters")
		}
		if c.Store.Driver == "" {
			add("sso.scim needs a store, where provisioned users are kept")
		}
	}
	for i, r := range s.Roles {
		if (r.Group == "") == (r.Email == "") {
			add("sso.roles[%d]: set exactly one of group and email", i)
		}
		switch {
		case r.Role == RoleOwner && r.Org != "":
			add("sso.roles[%d]: owner is gateway-wide and takes no org", i)
		case (r.Role == RoleAdmin || r.Role == RoleDeveloper) && r.Org == "":
			add("sso.roles[%d]: %s needs an org", i, r.Role)
		case r.Team != "" && r.Org == "":
			add("sso.roles[%d]: team needs an org", i)
		case s.RolePermissions(r.Role) == nil:
			add("sso.roles[%d]: role must be owner, admin, viewer, developer or one of sso.custom_roles, got %q", i, r.Role)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(s.CustomRoles)) {
		r := s.CustomRoles[name]
		switch {
		case name == RoleOwner || name == RoleAdmin || name == RoleViewer || name == RoleDeveloper:
			add("sso.custom_roles.%s: the name is a built-in role", name)
		case !mcpName.MatchString(name):
			add("sso.custom_roles.%s: names are lowercase letters, digits, - and _", name)
		case len(r.Permissions) == 0:
			add("sso.custom_roles.%s: permissions are required", name)
		}
		for _, p := range r.Permissions {
			if !slices.Contains(Permissions, p) {
				add("sso.custom_roles.%s: unknown permission %q; permissions are %s", name, p, strings.Join(Permissions, ", "))
			}
		}
	}
}

var (
	// mcpName is a server or virtual server name: safe in a URL path and
	// as a metric label.
	mcpName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	// mcpPrefix fits the MCP tool name alphabet.
	mcpPrefix = regexp.MustCompile(`^[A-Za-z0-9.-][A-Za-z0-9_.-]{0,63}$`)
	// vertexProject and vertexLocation go into Vertex AI URLs.
	vertexProject  = regexp.MustCompile(`^[a-z][a-z0-9:.-]{1,62}$`)
	vertexLocation = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)
	awsRegion      = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`)
)

func (c *Config) validateMCP(add func(string, ...any)) {
	m := &c.MCP
	if m.RefreshInterval < Duration(time.Second) {
		add("mcp.refresh_interval must be at least 1s")
	}
	m.PublicURL = strings.TrimRight(m.PublicURL, "/")
	if m.MaxUserSessions < 1 {
		add("mcp.max_user_sessions must be at least 1")
	}
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
		switch {
		case len(s.Command) > 0:
			if s.URL != "" {
				add("mcp server %q: set url or command, not both", name)
			}
			if s.Command[0] == "" {
				add("mcp server %q: command must name a program", name)
			}
			if len(s.Headers) > 0 || s.UserAuth != nil {
				add("mcp server %q: headers and user_auth are for url servers; a command server takes env", name)
			}
		case !httpURL(s.URL):
			add("mcp server %q: url must be an http or https URL", name)
		}
		if len(s.Command) == 0 && (len(s.Env) > 0 || s.Dir != "") {
			add("mcp server %q: env and dir are for command servers", name)
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
		if ua := s.UserAuth; ua != nil {
			if ua.ClientID == "" {
				add("mcp server %q: user_auth.client_id is required", name)
			}
			for field, u := range map[string]string{"authorization_url": ua.AuthorizationURL, "token_url": ua.TokenURL, "revocation_url": ua.RevocationURL} {
				if u != "" && !httpURL(u) {
					add("mcp server %q: user_auth.%s must be an http or https URL", name, field)
				}
			}
			if m.OAuth == nil {
				add("mcp server %q: user_auth needs mcp.oauth, which identifies each user", name)
			}
			if c.Secrets.EncryptionKey == "" {
				add("mcp server %q: user_auth needs secrets.encryption_key, to encrypt user tokens", name)
			}
			if c.Store.Driver == "" {
				add("mcp server %q: user_auth needs a store, where user tokens are kept", name)
			}
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

func (c *Config) validateStreams(add func(string, ...any)) {
	for name, st := range c.Streams {
		if !ValidObjectName(name) {
			add("stream %q: names are lowercase letters, digits, '.', '_' and '-'", name)
		}
		switch st.Type {
		case StreamWebhook:
			if !webhookURL(st.URL.Value()) {
				add("stream %q: url must be https, or http to localhost", name)
			}
		case StreamFile:
			if !filepath.IsAbs(st.Path) {
				add("stream %q: path must be an absolute file path", name)
			}
		default:
			add("stream %q: type must be webhook or file, got %q", name, st.Type)
		}
		if len(st.Records) == 0 {
			add("stream %q: records must list at least one of %s", name, strings.Join(RecordTypes, ", "))
		}
		for _, r := range st.Records {
			if !slices.Contains(RecordTypes, r) {
				add("stream %q: unknown record type %q; use %s", name, r, strings.Join(RecordTypes, ", "))
			}
		}
		if st.BatchSize < 0 || st.BatchSize > MaxStreamBatchSize || st.FlushInterval < 0 {
			add("stream %q: batch_size must be between 1 and %d, and flush_interval not negative", name, MaxStreamBatchSize)
		}
		if st.BatchSize == 0 {
			st.BatchSize = DefaultStreamBatchSize
		}
		if st.FlushInterval == 0 {
			st.FlushInterval = Duration(DefaultStreamFlushInterval)
		}
		c.Streams[name] = st
	}
}

func (c *Config) validateA2A(add func(string, ...any)) {
	a := &c.A2A
	a.PublicURL = strings.TrimRight(a.PublicURL, "/")
	if a.PublicURL != "" && !httpURL(a.PublicURL) {
		add("a2a.public_url must be an http or https URL")
	}
	for name, ag := range a.Agents {
		if !mcpName.MatchString(name) {
			add("a2a agent %q: names use lowercase letters, digits, - and _", name)
		}
		if !httpURL(ag.URL) {
			add("a2a agent %q: url must be an http or https URL", name)
		}
		if ag.CardURL != "" && !httpURL(ag.CardURL) {
			add("a2a agent %q: card_url must be an http or https URL", name)
		}
		if ag.Timeout < 0 {
			add("a2a agent %q: timeout must not be negative", name)
		}
		if ag.Timeout == 0 {
			ag.Timeout = Duration(DefaultA2ATimeout)
		}
		for _, g := range ag.Guardrails {
			if _, ok := c.Guardrails[g]; !ok {
				add("a2a agent %q: guardrail %q is not configured", name, g)
			}
		}
		a.Agents[name] = ag
	}
}

func (c *Config) validatePlanes(add func(string, ...any)) {
	if cp := c.ControlPlane; cp.URL != "" {
		c.ControlPlane.URL = strings.TrimRight(cp.URL, "/")
		if !httpURL(cp.URL) {
			add("control_plane.url must be an http or https URL")
		}
		if len(cp.Token.Value()) < minPlaneToken {
			add("control_plane.token must be at least %d characters", minPlaneToken)
		}
		if c.Store.Driver != "" {
			add("a data plane reads from its control plane, so control_plane and store cannot both be set")
		}
	} else if cp.Token != "" || cp.CacheFile != "" {
		add("control_plane needs a url")
	}
	if t := c.DataPlanes.Token; t != "" {
		if len(t.Value()) < minPlaneToken {
			add("data_planes.token must be at least %d characters", minPlaneToken)
		}
		if c.Store.Driver == "" {
			add("data_planes needs a store, which the snapshot is read from")
		}
	}
}
