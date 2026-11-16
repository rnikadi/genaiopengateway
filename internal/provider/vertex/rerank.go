package vertex

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

// maxRankRecords is how many records the ranking API takes.
const maxRankRecords = 1000

// maxBody caps an upstream answer held in memory.
const maxBody = 64 << 20

// isRanker reports whether a model is one of the ranking API's, such
// as semantic-ranker-default@latest.
func isRanker(model string) bool { return strings.HasPrefix(model, "semantic-ranker-") }

// rankURL is where the ranking API is for the provider's location. It
// is part of Vertex AI Search (Discovery Engine), which has a global
// endpoint and the us and eu multi-regions; other locations use the
// global one.
func (p *Provider) rankURL() string {
	loc, host := "global", "discoveryengine.googleapis.com"
	if p.cfg.Location == "us" || p.cfg.Location == "eu" {
		loc, host = p.cfg.Location, p.cfg.Location+"-discoveryengine.googleapis.com"
	}
	base := "https://" + host
	if p.cfg.BaseURL != "" {
		base = p.cfg.BaseURL
	}
	return base + "/v1/projects/" + url.PathEscape(p.cfg.Project) + "/locations/" + loc + "/rankingConfigs/default_ranking_config:rank"
}

// rerank serves /v1/rerank through the ranking API: each document is a
// record whose id is its place in the request.
func (p *Provider) rerank(ctx context.Context, req *provider.Request, model string) (*provider.Response, error) {
	rr, problem := provider.ParseRerank(req.Body)
	if rr == nil {
		return errorResponse(http.StatusBadRequest, "invalid_request_error", problem), nil
	}
	if len(rr.Texts) > maxRankRecords {
		return errorResponse(http.StatusBadRequest, "invalid_request_error", "this model takes at most 1000 documents per request"), nil
	}
	records := make([]map[string]string, len(rr.Texts))
	for i, text := range rr.Texts {
		records[i] = map[string]string{"id": strconv.Itoa(i), "content": text}
	}
	body := map[string]any{"model": model, "query": rr.Query, "records": records, "ignoreRecordDetailsInResponse": true}
	if rr.TopN > 0 {
		body["topN"] = rr.TopN
	}
	raw, _ := json.Marshal(body)
	// The ranking API bills and counts quota against this project.
	status, hdr, buf, err := p.call(ctx, http.MethodPost, p.rankURL(), raw, http.Header{"X-Goog-User-Project": {p.cfg.Project}})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return googleError(status, hdr, buf), nil
	}
	var out struct {
		Records []struct {
			ID    string   `json:"id"`
			Score *float64 `json:"score"`
		} `json:"records"`
	}
	if json.Unmarshal(buf, &out) != nil || out.Records == nil {
		return errorResponse(http.StatusBadGateway, "api_error", "upstream returned an unreadable response"), nil
	}
	results := make([]provider.RerankResult, 0, len(out.Records))
	for _, r := range out.Records {
		i, err := strconv.Atoi(r.ID)
		if err != nil || r.Score == nil {
			return errorResponse(http.StatusBadGateway, "api_error", "upstream returned a record without an id or a score"), nil
		}
		results = append(results, provider.RerankResult{Index: i, Score: *r.Score})
	}
	return rr.Response("", results), nil
}

// call sends a JSON request to a Google API with the provider's
// credentials and reads the answer. A credential failure is a 502,
// which allows fallback.
func (p *Provider) call(ctx context.Context, method, u string, body []byte, extra http.Header) (int, http.Header, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout.Std())
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	hreq, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return 0, nil, nil, err
	}
	if body != nil {
		hreq.Header.Set("Content-Type", "application/json")
	}
	for k, v := range extra {
		hreq.Header[k] = v
	}
	for k, v := range p.cfg.Headers {
		hreq.Header.Set(k, v)
	}
	if err := p.auth.authorize(hreq); err != nil {
		b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": "upstream credentials: " + err.Error(), "status": "API_ERROR"}})
		return http.StatusBadGateway, http.Header{}, b, nil
	}
	resp, err := p.client.Do(hreq)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return 0, nil, nil, err
	}
	return resp.StatusCode, resp.Header, buf, nil
}

// googleError is a Google API's error in the shape clients read.
func googleError(status int, hdr http.Header, buf []byte) *provider.Response {
	var ge struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	_ = json.Unmarshal(buf, &ge)
	msg, typ := ge.Error.Message, strings.ToLower(ge.Error.Status)
	if msg == "" {
		msg = "the upstream answered HTTP " + strconv.Itoa(status)
	}
	if typ == "" {
		typ = "api_error"
	}
	out := errorResponse(status, typ, msg)
	if ra := hdr.Get("Retry-After"); ra != "" {
		out.Header.Set("Retry-After", ra)
	}
	return out
}

// errorResponse builds an error response in the shape rerank clients
// read.
func errorResponse(status int, typ, msg string) *provider.Response {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": typ}})
	return &provider.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(b)), Usage: &usage.Tokens{}}
}
