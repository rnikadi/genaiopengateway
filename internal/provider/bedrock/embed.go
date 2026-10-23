package bedrock

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

// Limits on embedding requests: Titan embeds one text per call, so the
// gateway makes the calls in parallel, a few at a time; Cohere takes up
// to 96 texts in one call.
const (
	maxEmbedInputs  = 2048
	maxCohereInputs = 96
	titanParallel   = 8
)

type embedRequest struct {
	Model          string          `json:"model"`
	Input          json.RawMessage `json:"input"`
	Dimensions     *int            `json:"dimensions"`
	EncodingFormat string          `json:"encoding_format"`
}

// embedResult is one upstream call's outcome.
type embedResult struct {
	vectors [][]float64
	tokens  int64
	failed  *provider.Response
}

func (p *Provider) embed(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	var er embedRequest
	if err := json.Unmarshal(req.Body, &er); err != nil {
		return errorResponse(http.StatusBadRequest, "invalid_request_error", "request body is not a valid embeddings request"), nil
	}
	if er.EncodingFormat != "" && er.EncodingFormat != "float" {
		return errorResponse(http.StatusBadRequest, "invalid_request_error", "encoding_format must be float for this model"), nil
	}
	var inputs []string
	var one string
	if json.Unmarshal(er.Input, &one) == nil {
		inputs = []string{one}
	} else if json.Unmarshal(er.Input, &inputs) != nil {
		return errorResponse(http.StatusBadRequest, "invalid_request_error", "input must be a string or an array of strings for this model"), nil
	}
	if len(inputs) == 0 || len(inputs) > maxEmbedInputs {
		return errorResponse(http.StatusBadRequest, "invalid_request_error", "input must have between 1 and 2048 texts"), nil
	}

	var results []embedResult
	var err error
	if strings.Contains(req.Model, "cohere.embed") {
		if len(inputs) > maxCohereInputs {
			return errorResponse(http.StatusBadRequest, "invalid_request_error", "this model takes at most 96 texts per request"), nil
		}
		body := map[string]any{"texts": inputs, "input_type": "search_document"}
		if er.Dimensions != nil {
			body["output_dimension"] = *er.Dimensions
		}
		var r embedResult
		r, err = p.invokeEmbed(ctx, req.Model, body)
		results = []embedResult{r}
	} else {
		results = make([]embedResult, len(inputs))
		errs := make([]error, len(inputs))
		sem := make(chan struct{}, titanParallel)
		var wg sync.WaitGroup
		for i, in := range inputs {
			body := map[string]any{"inputText": in}
			if er.Dimensions != nil {
				body["dimensions"] = *er.Dimensions
			}
			wg.Go(func() {
				sem <- struct{}{}
				defer func() { <-sem }()
				results[i], errs[i] = p.invokeEmbed(ctx, req.Model, body)
			})
		}
		wg.Wait()
		for _, e := range errs {
			if e != nil {
				err = e
			}
		}
	}
	if err != nil {
		return nil, err
	}

	var data []map[string]any
	var tokens int64
	for _, r := range results {
		if r.failed != nil {
			return r.failed, nil
		}
		for _, v := range r.vectors {
			data = append(data, map[string]any{"object": "embedding", "index": len(data), "embedding": v})
		}
		tokens += r.tokens
	}
	if len(data) != len(inputs) {
		return errorResponse(http.StatusBadGateway, "api_error", "upstream returned "+strconv.Itoa(len(data))+" embeddings for "+strconv.Itoa(len(inputs))+" inputs"), nil
	}
	b, _ := json.Marshal(map[string]any{"object": "list", "data": data, "model": req.Model,
		"usage": map[string]any{"prompt_tokens": tokens, "total_tokens": tokens}})
	return &provider.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(b)),
		Usage:      &usage.Tokens{InputTokens: tokens, Reported: true},
	}, nil
}

// invokeEmbed makes one InvokeModel call to an embedding model. An
// upstream error is returned as a failed result in the OpenAI shape.
func (p *Provider) invokeEmbed(ctx context.Context, model string, body map[string]any) (embedResult, error) {
	raw, _ := json.Marshal(body)
	resp, err := p.post(ctx, model, "invoke", raw)
	if err != nil {
		return embedResult{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return embedResult{failed: openAIError(resp)}, nil
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return embedResult{}, err
	}
	var out struct {
		// Titan
		Embedding           []float64 `json:"embedding"`
		InputTextTokenCount int64     `json:"inputTextTokenCount"`
		// Cohere: a list of vectors, or vectors by type when embedding
		// types were asked for (Embed v4).
		Embeddings json.RawMessage `json:"embeddings"`
	}
	if json.Unmarshal(buf, &out) != nil {
		return embedResult{failed: errorResponse(http.StatusBadGateway, "api_error", "upstream returned an unreadable response")}, nil
	}
	r := embedResult{tokens: out.InputTextTokenCount}
	if n, err := strconv.ParseInt(resp.Header.Get("X-Amzn-Bedrock-Input-Token-Count"), 10, 64); err == nil {
		r.tokens = n
	}
	switch {
	case out.Embedding != nil:
		r.vectors = [][]float64{out.Embedding}
	case len(out.Embeddings) > 0:
		if json.Unmarshal(out.Embeddings, &r.vectors) != nil {
			var typed struct {
				Float [][]float64 `json:"float"`
			}
			_ = json.Unmarshal(out.Embeddings, &typed)
			r.vectors = typed.Float
		}
	}
	return r, nil
}
