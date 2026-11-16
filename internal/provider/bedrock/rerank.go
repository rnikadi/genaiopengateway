package bedrock

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/rnikadi/genaiopengateway/internal/provider"
)

// maxRerankDocs is how many documents the rerank models take.
const maxRerankDocs = 1000

// isRerank reports whether a model reranks: Cohere Rerank and Amazon
// Rerank, such as cohere.rerank-v3-5:0 and amazon.rerank-v1:0.
func isRerank(model string) bool { return strings.Contains(model, ".rerank") }

// rerank serves /v1/rerank through InvokeModel. Both models take a
// query and documents as strings, and answer with an index and a score
// for each document they kept.
func (p *Provider) rerank(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	rr, problem := provider.ParseRerank(req.Body)
	if rr == nil {
		return errorResponse(http.StatusBadRequest, "invalid_request_error", problem), nil
	}
	if len(rr.Texts) > maxRerankDocs {
		return errorResponse(http.StatusBadRequest, "invalid_request_error", "this model takes at most 1000 documents per request"), nil
	}
	body := map[string]any{"query": rr.Query, "documents": rr.Texts}
	if rr.TopN > 0 {
		body["top_n"] = min(rr.TopN, len(rr.Texts))
	}
	if strings.Contains(req.Model, "cohere.") {
		// Cohere's models on Bedrock want the version of their API named.
		body["api_version"] = 2
	}
	raw, _ := json.Marshal(body)
	resp, err := p.post(ctx, req.Model, "invoke", raw)
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
	var out struct {
		Results []struct {
			Index *int     `json:"index"`
			Score *float64 `json:"relevance_score"`
		} `json:"results"`
	}
	if json.Unmarshal(buf, &out) != nil || out.Results == nil {
		return errorResponse(http.StatusBadGateway, "api_error", "upstream returned an unreadable response"), nil
	}
	results := make([]provider.RerankResult, 0, len(out.Results))
	for _, r := range out.Results {
		if r.Index == nil || r.Score == nil {
			return errorResponse(http.StatusBadGateway, "api_error", "upstream returned a result without an index or a score"), nil
		}
		results = append(results, provider.RerankResult{Index: *r.Index, Score: *r.Score})
	}
	return rr.Response(resp.Header.Get("X-Amzn-Requestid"), results), nil
}
