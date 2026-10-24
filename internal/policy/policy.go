// Package policy defines the access and spend rules that can be attached
// to an organization, a team or an API key.
package policy

import (
	"fmt"
	"slices"
	"time"
)

// Budget periods.
const (
	PeriodDaily   = "daily"
	PeriodMonthly = "monthly"
	PeriodTotal   = "total"
)

// Budget caps spend in USD over a period.
type Budget struct {
	AmountUSD float64 `yaml:"amount_usd" json:"amount_usd"`
	Period    string  `yaml:"period" json:"period"`
	// AlertAt overrides the gateway's alert thresholds, as shares of
	// AmountUSD. Nil uses the defaults; empty turns alerts off.
	AlertAt []float64 `yaml:"alert_at" json:"alert_at,omitzero"`
}

// Cache modes (docs/cache.md).
const (
	CacheOff      = "off"
	CacheExact    = "exact"
	CacheSemantic = "semantic"
)

// MaxCacheTTL bounds how long a cached answer is reused.
const MaxCacheTTL = 30 * 24 * time.Hour

// Cache turns the response cache on or off at one level.
type Cache struct {
	Mode string `yaml:"mode" json:"mode"`
	// TTL is how long an answer is reused, such as "1h".
	TTL string `yaml:"ttl" json:"ttl,omitempty"`
}

// Duration is the cache's TTL; zero when the cache is off.
func (c Cache) Duration() time.Duration {
	if c.Mode == CacheOff {
		return 0
	}
	d, _ := time.ParseDuration(c.TTL)
	return d
}

// Policy is the set of rules at one level of the tenancy hierarchy. Zero
// values mean "no restriction".
type Policy struct {
	AllowedModels     []string `yaml:"allowed_models" json:"allowed_models,omitempty"`
	AllowedMCPServers []string `yaml:"allowed_mcp_servers" json:"allowed_mcp_servers,omitempty"`
	RPM               int64    `yaml:"rpm" json:"rpm,omitempty"`
	TPM               int64    `yaml:"tpm" json:"tpm,omitempty"`
	Budget            *Budget  `yaml:"budget" json:"budget,omitempty"`
	// LogBodies turns body logging on or off at this level; nil inherits
	// from the level above (docs/body-logging.md).
	LogBodies *bool `yaml:"log_bodies" json:"log_bodies,omitempty"`
	// Guardrails names guardrails to run. Unlike other fields, the lists
	// of all levels add up (docs/guardrails.md).
	Guardrails []string `yaml:"guardrails" json:"guardrails,omitempty"`
	// Cache sets the response cache at this level; nil inherits from the
	// level above (docs/cache.md).
	Cache *Cache `yaml:"cache" json:"cache,omitempty"`
}

// AllowsModel reports whether the policy permits a virtual model.
func (p Policy) AllowsModel(model string) bool {
	return len(p.AllowedModels) == 0 || slices.Contains(p.AllowedModels, model)
}

// AllowsMCPServer reports whether the policy permits a virtual MCP server.
func (p Policy) AllowsMCPServer(name string) bool {
	return len(p.AllowedMCPServers) == 0 || slices.Contains(p.AllowedMCPServers, name)
}

// Validate checks that limits are non-negative and the budget is well formed.
func (p Policy) Validate() error {
	if p.RPM < 0 || p.TPM < 0 {
		return fmt.Errorf("rpm and tpm must not be negative")
	}
	if b := p.Budget; b != nil {
		if b.AmountUSD < 0 {
			return fmt.Errorf("budget amount must not be negative")
		}
		switch b.Period {
		case PeriodDaily, PeriodMonthly, PeriodTotal:
		default:
			return fmt.Errorf("budget period must be daily, monthly or total, got %q", b.Period)
		}
		for _, t := range b.AlertAt {
			if t <= 0 || t > 1 {
				return fmt.Errorf("budget alert_at values must be above 0 and at most 1, got %v", t)
			}
		}
	}
	if c := p.Cache; c != nil {
		switch c.Mode {
		case CacheOff:
		case CacheExact, CacheSemantic:
			d, err := time.ParseDuration(c.TTL)
			if err != nil || d < time.Second || d > MaxCacheTTL {
				return fmt.Errorf("cache ttl must be a duration between 1s and 720h, got %q", c.TTL)
			}
		default:
			return fmt.Errorf("cache mode must be off, exact or semantic, got %q", c.Mode)
		}
	}
	return nil
}
