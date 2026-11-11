// Package catalog holds provider presets and model prices for the setup
// wizard and the configuration forms (docs/catalog.md). It is a starting
// point: everything it fills in is ordinary config that operators can
// change, and prices are refreshed with each release.
package catalog

import (
	_ "embed"
	"fmt"
	"slices"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/rnikadi/genaiopengateway/internal/config"
)

//go:embed catalog.yaml
var raw []byte

// Kinds of model, by the endpoint they serve.
var Kinds = []string{"chat", "embeddings", "image", "speech", "transcription", "moderation", "rerank", "video", "realtime", "live", "bidi_stream"}

// Catalog is a versioned list of provider presets.
type Catalog struct {
	Version   string     `yaml:"version" json:"version"`
	Providers []Provider `yaml:"providers" json:"providers"`
}

// Provider is a preset for one upstream: what a provider entry in the
// config needs, where to get a key, and the models it commonly serves.
type Provider struct {
	ID         string `yaml:"id" json:"id"`
	Name       string `yaml:"name" json:"name"`
	Type       string `yaml:"type" json:"type"`
	BaseURL    string `yaml:"base_url" json:"base_url,omitempty"`
	Region     string `yaml:"region" json:"region,omitempty"`
	Location   string `yaml:"location" json:"location,omitempty"`
	KeyEnv     string `yaml:"key_env" json:"key_env,omitempty"`
	KeyURL     string `yaml:"key_url" json:"key_url,omitempty"`
	PricingURL string `yaml:"pricing_url" json:"pricing_url,omitempty"`
	// KeyOptional providers work without an API key (local servers, or
	// cloud credentials from the environment); NoKey ones never take one.
	KeyOptional bool `yaml:"key_optional" json:"key_optional,omitempty"`
	NoKey       bool `yaml:"no_key" json:"no_key,omitempty"`
	// CustomModels providers serve models the catalog cannot list, such
	// as deployments or locally pulled models.
	CustomModels bool    `yaml:"custom_models" json:"custom_models,omitempty"`
	Note         string  `yaml:"note" json:"note,omitempty"`
	Models       []Model `yaml:"models" json:"models"`
}

// Model is one upstream model with its kind and list price.
type Model struct {
	ID    string       `yaml:"id" json:"id"`
	Kind  string       `yaml:"kind" json:"kind"`
	Price config.Price `yaml:"price" json:"price"`
}

var (
	once   sync.Once
	loaded *Catalog
	err    error
)

// Get returns the embedded catalog.
func Get() (*Catalog, error) {
	once.Do(func() { loaded, err = parse(raw) })
	return loaded, err
}

func parse(b []byte) (*Catalog, error) {
	var c Catalog
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	ids := map[string]bool{}
	for _, p := range c.Providers {
		if p.ID == "" || ids[p.ID] {
			return nil, fmt.Errorf("catalog: provider id %q is empty or repeated", p.ID)
		}
		ids[p.ID] = true
		for _, m := range p.Models {
			if !slices.Contains(Kinds, m.Kind) {
				return nil, fmt.Errorf("catalog: %s/%s: unknown kind %q", p.ID, m.ID, m.Kind)
			}
		}
	}
	return &c, nil
}

// Find returns the preset with this id.
func (c *Catalog) Find(id string) (Provider, bool) {
	for _, p := range c.Providers {
		if p.ID == id {
			return p, true
		}
	}
	return Provider{}, false
}

// Price looks up a model's list price at a provider type and model id,
// for filling in a target's price when the operator leaves it empty.
func (c *Catalog) Price(providerType, model string) (config.Price, bool) {
	for _, p := range c.Providers {
		if p.Type != providerType {
			continue
		}
		for _, m := range p.Models {
			if m.ID == model {
				return m.Price, true
			}
		}
	}
	return config.Price{}, false
}
