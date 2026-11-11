// Package fal adapts fal.ai's synchronous model API, served as OpenAI
// image generation for its text to image models, such as
// fal-ai/flux/dev.
package fal

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/provider/native"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

// Provider is a fal.ai upstream.
type Provider struct{ c native.Client }

// New returns an adapter for cfg.
func New(cfg config.Provider, client *http.Client) *Provider {
	return &Provider{c: native.Client{Cfg: cfg, HTTP: client, Auth: func(h http.Header) {
		if key := cfg.APIKey.Value(); key != "" {
			h.Set("Authorization", "Key "+key)
		}
	}}}
}

// Supports implements provider.Provider.
func (p *Provider) Supports(e provider.Endpoint) bool { return e == provider.ImageGenerations }

// Send implements provider.Provider. The model is the fal endpoint id,
// which is the path below the base URL.
func (p *Provider) Send(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	if req.Endpoint != provider.ImageGenerations {
		return native.Error(http.StatusNotFound, "invalid_request_error", "fal does not serve "+string(req.Endpoint)), nil
	}
	if req.Model == "" || strings.Contains(req.Model, "..") || strings.ContainsAny(req.Model, "?#") {
		return native.BadRequest("the model must be a fal endpoint id, such as fal-ai/flux/dev"), nil
	}
	ir, bad := native.ReadImage(req.Body)
	if bad != nil {
		return bad, nil
	}
	if ir.N > native.MaxImages {
		return native.BadRequest("n must be at most %d", native.MaxImages), nil
	}
	in := map[string]any{"prompt": ir.Prompt, "num_images": ir.N}
	if w, h, ok := native.Size(ir.Size); ok {
		in["image_size"] = map[string]int{"width": w, "height": h}
	}
	if ir.Seed != nil {
		in["seed"] = *ir.Seed
	}
	if ir.NegativePrompt != "" {
		in["negative_prompt"] = ir.NegativePrompt
	}
	switch ir.OutputFormat {
	case "":
	case "png", "jpeg":
		in["output_format"] = ir.OutputFormat
	default:
		return native.BadRequest("output_format must be png or jpeg"), nil
	}
	b, _ := json.Marshal(in)
	resp, buf, err := p.c.Do(ctx, http.MethodPost, "/"+strings.TrimPrefix(req.Model, "/"), b, "application/json")
	if err != nil {
		return nil, err
	}
	if bad := native.Failed(resp, buf); bad != nil {
		return bad, nil
	}
	var out struct {
		Images []struct {
			URL string `json:"url"`
		} `json:"images"`
		NSFW []bool `json:"has_nsfw_concepts"`
	}
	if json.Unmarshal(buf, &out) != nil {
		return native.BadGateway("the upstream answer is not a fal image result"), nil
	}
	data := []map[string]string{}
	for i, img := range out.Images {
		if i < len(out.NSFW) && out.NSFW[i] {
			continue
		}
		if img.URL != "" {
			data = append(data, map[string]string{"url": img.URL})
		}
	}
	t := &usage.Tokens{Images: int64(len(data)), Reported: true}
	return native.JSON(map[string]any{"created": time.Now().Unix(), "data": data}, t), nil
}
