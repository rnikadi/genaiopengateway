// Package stability adapts the Stability AI API: Stable Image Core,
// Ultra and Stable Diffusion 3.5, served as OpenAI image generation.
package stability

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/provider/native"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

// ratios are the aspect ratios Stability makes.
var ratios = []string{"1:1", "16:9", "21:9", "2:3", "3:2", "4:5", "5:4", "9:16", "9:21"}

// Provider is a Stability AI upstream.
type Provider struct{ c native.Client }

// New returns an adapter for cfg.
func New(cfg config.Provider, client *http.Client) *Provider {
	return &Provider{c: native.Client{Cfg: cfg, HTTP: client, Auth: func(h http.Header) {
		if key := cfg.APIKey.Value(); key != "" {
			h.Set("Authorization", "Bearer "+key)
		}
		h.Set("Accept", "application/json")
	}}}
}

// Supports implements provider.Provider.
func (p *Provider) Supports(e provider.Endpoint) bool { return e == provider.ImageGenerations }

// service returns the generate path and the model field for a model:
// stable-image-core and stable-image-ultra are services of their own,
// and sd3* models share the sd3 service.
func service(model string) (path, sd3Model string) {
	switch {
	case strings.HasPrefix(model, "sd3"):
		return "/v2beta/stable-image/generate/sd3", model
	case model == "stable-image-ultra" || model == "ultra":
		return "/v2beta/stable-image/generate/ultra", ""
	}
	return "/v2beta/stable-image/generate/core", ""
}

// Send implements provider.Provider. Stability makes one image per
// call, so n images are n calls, up to native.MaxImages.
func (p *Provider) Send(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	if req.Endpoint != provider.ImageGenerations {
		return native.Error(http.StatusNotFound, "invalid_request_error", "Stability AI does not serve "+string(req.Endpoint)), nil
	}
	ir, bad := native.ReadImage(req.Body)
	if bad != nil {
		return bad, nil
	}
	if ir.N > native.MaxImages {
		return native.BadRequest("n must be at most %d", native.MaxImages), nil
	}
	format := ir.OutputFormat
	switch format {
	case "":
		format = "png"
	case "png", "jpeg", "webp":
	default:
		return native.BadRequest("output_format must be png, jpeg or webp"), nil
	}
	path, sd3Model := service(req.Model)
	data := []map[string]string{}
	for i := range ir.N {
		var b bytes.Buffer
		w := multipart.NewWriter(&b)
		_ = w.WriteField("prompt", ir.Prompt)
		_ = w.WriteField("output_format", format)
		if r := native.NearestRatio(ir.Size, ratios); r != "" {
			_ = w.WriteField("aspect_ratio", r)
		}
		if sd3Model != "" {
			_ = w.WriteField("model", sd3Model)
		}
		if ir.NegativePrompt != "" {
			_ = w.WriteField("negative_prompt", ir.NegativePrompt)
		}
		if ir.Seed != nil {
			_ = w.WriteField("seed", strconv.FormatInt(*ir.Seed+int64(i), 10))
		}
		_ = w.Close()
		resp, buf, err := p.c.Do(ctx, http.MethodPost, path, b.Bytes(), w.FormDataContentType())
		if err != nil {
			return nil, err
		}
		if bad := native.Failed(resp, buf); bad != nil {
			return bad, nil
		}
		var out struct {
			Image        string `json:"image"`
			FinishReason string `json:"finish_reason"`
		}
		if json.Unmarshal(buf, &out) != nil || out.Image == "" {
			return native.BadGateway("the upstream answer is not a Stability image"), nil
		}
		if out.FinishReason == "CONTENT_FILTERED" {
			// A filtered image is a blurred placeholder; leave it out,
			// as for other providers.
			continue
		}
		data = append(data, map[string]string{"b64_json": out.Image})
	}
	t := &usage.Tokens{Images: int64(len(data)), Reported: true}
	return native.JSON(map[string]any{"created": time.Now().Unix(), "data": data, "output_format": format}, t), nil
}
