package bedrock

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/provider"
)

func isImageModel(model string) bool {
	return strings.Contains(model, "amazon.nova-canvas") || strings.Contains(model, "amazon.titan-image")
}

// images serves OpenAI image generation with Nova Canvas or Titan Image,
// which share one request shape.
func (p *Provider) images(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	var ir struct {
		Prompt string `json:"prompt"`
		N      int    `json:"n"`
		Size   string `json:"size"`
	}
	if json.Unmarshal(req.Body, &ir) != nil || ir.Prompt == "" {
		return errorResponse(http.StatusBadRequest, "invalid_request_error", "prompt is required"), nil
	}
	cfg := map[string]any{"numberOfImages": max(ir.N, 1)}
	if w, h, ok := strings.Cut(ir.Size, "x"); ok {
		wi, err1 := strconv.Atoi(w)
		hi, err2 := strconv.Atoi(h)
		if err1 != nil || err2 != nil {
			return errorResponse(http.StatusBadRequest, "invalid_request_error", "size must be auto or WIDTHxHEIGHT"), nil
		}
		cfg["width"], cfg["height"] = wi, hi
	}
	body, _ := json.Marshal(map[string]any{
		"taskType":              "TEXT_IMAGE",
		"textToImageParams":     map[string]string{"text": ir.Prompt},
		"imageGenerationConfig": cfg,
	})
	resp, err := p.post(ctx, req.Model, "invoke", body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return openAIError(resp), nil
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	var ar struct {
		Images []string `json:"images"`
		Error  string   `json:"error"`
	}
	if err := json.Unmarshal(buf, &ar); err != nil {
		return errorResponse(http.StatusBadGateway, "api_error", "the upstream answer is not an image response"), nil
	}
	if ar.Error != "" && len(ar.Images) == 0 {
		return errorResponse(http.StatusBadRequest, "invalid_request_error", ar.Error), nil
	}
	data := make([]map[string]string, 0, len(ar.Images))
	for _, img := range ar.Images {
		data = append(data, map[string]string{"b64_json": img})
	}
	out, _ := json.Marshal(map[string]any{"created": time.Now().Unix(), "data": data, "output_format": "png"})
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.Header = http.Header{"Content-Type": {"application/json"}}
	resp.Usage.Images = int64(len(data))
	return resp, nil
}
