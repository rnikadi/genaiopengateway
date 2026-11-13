package native

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"github.com/rnikadi/genaiopengateway/internal/provider"
)

// ImageRequest is an OpenAI image generation request.
type ImageRequest struct {
	Prompt         string `json:"prompt"`
	N              int    `json:"n"`
	Size           string `json:"size"`
	ResponseFormat string `json:"response_format"`
	OutputFormat   string `json:"output_format"`
	NegativePrompt string `json:"negative_prompt"`
	Seed           *int64 `json:"seed"`
}

// MaxImages caps n for providers that make one image per call.
const MaxImages = 4

// ReadImage parses an image request, with n at least 1.
func ReadImage(body []byte) (ImageRequest, *provider.Response) {
	var ir ImageRequest
	if json.Unmarshal(body, &ir) != nil || ir.Prompt == "" {
		return ir, BadRequest("prompt is required")
	}
	ir.N = max(ir.N, 1)
	return ir, nil
}

// Size reads an OpenAI size such as 1536x1024, or reports false for
// auto and sizes it cannot read.
func Size(size string) (w, h int, ok bool) {
	a, b, found := strings.Cut(size, "x")
	w, err1 := strconv.Atoi(a)
	h, err2 := strconv.Atoi(b)
	if !found || err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

// NearestRatio returns the ratio from ratios, written as "w:h", nearest
// to an OpenAI size, or "" when the size is auto or unreadable.
func NearestRatio(size string, ratios []string) string {
	w, h, ok := Size(size)
	if !ok {
		return ""
	}
	best, diff := "", math.Inf(1)
	for _, r := range ratios {
		a, b, _ := strings.Cut(r, ":")
		af, _ := strconv.ParseFloat(a, 64)
		bf, _ := strconv.ParseFloat(b, 64)
		if d := math.Abs(math.Log(float64(w)/float64(h)) - math.Log(af/bf)); d < diff {
			best, diff = r, d
		}
	}
	return best
}
