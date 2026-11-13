package catalog

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rnikadi/genaiopengateway/internal/config"
)

// TestPresetsMakeValidConfig checks every preset, with every model, is a
// config the gateway accepts.
func TestPresetsMakeValidConfig(t *testing.T) {
	c, err := Get()
	if err != nil {
		t.Fatal(err)
	}
	if c.Version == "" || len(c.Providers) < 10 {
		t.Fatalf("catalog version %q with %d providers", c.Version, len(c.Providers))
	}
	for _, p := range c.Providers {
		var b strings.Builder
		fmt.Fprintf(&b, "providers:\n  p: {type: %q", p.Type)
		if p.BaseURL != "" {
			fmt.Fprintf(&b, ", base_url: %q", p.BaseURL)
		}
		if p.Region != "" {
			fmt.Fprintf(&b, ", region: %q", p.Region)
		}
		if p.Type == config.ProviderVertex {
			fmt.Fprintf(&b, ", project: my-project, location: %q", p.Location)
		}
		b.WriteString("}\nmodels:\n")
		for i, m := range p.Models {
			if m.Price.IsZero() && p.Type != config.ProviderOpenAI {
				t.Errorf("%s/%s has no price", p.ID, m.ID)
			}
			fmt.Fprintf(&b, "  m%d: {targets: [{provider: p, model: %q, price: {input_per_mtok: %g, output_per_mtok: %g}}]}\n",
				i, m.ID, m.Price.InputPerMTok, m.Price.OutputPerMTok)
		}
		if _, err := config.Parse([]byte(b.String())); err != nil {
			t.Errorf("preset %s: %v\n%s", p.ID, err, b.String())
		}
	}
}

func TestPriceLookup(t *testing.T) {
	c, _ := Get()
	if p, ok := c.Price("openai", "gpt-4.1-mini"); !ok || p.InputPerMTok != 0.4 {
		t.Fatalf("gpt-4.1-mini = %+v, %v", p, ok)
	}
	if _, ok := c.Price("openai", "no-such-model"); ok {
		t.Fatal("found a model that is not listed")
	}
	if _, ok := c.Find("anthropic"); !ok {
		t.Fatal("anthropic preset missing")
	}
}
