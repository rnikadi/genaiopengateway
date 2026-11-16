package provider

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sort"

	"github.com/rnikadi/genaiopengateway/internal/usage"
)

// RerankRequest is a request to /v1/rerank, in the shape Cohere and
// Jina share, read for adapters whose upstream has another shape.
type RerankRequest struct {
	Query string
	// Texts are the documents' texts, and Docs the documents as sent.
	Texts           []string
	Docs            []json.RawMessage
	TopN            int
	ReturnDocuments bool
}

// RerankResult is the score of one document, by its place in the
// request.
type RerankResult struct {
	Index int
	Score float64
}

// ParseRerank reads a rerank request. A document is a string, or an
// object: its text field when it has one, otherwise its JSON. It
// returns a message for the client when the request is not valid.
func ParseRerank(body []byte) (*RerankRequest, string) {
	var in struct {
		Query           *string           `json:"query"`
		Documents       []json.RawMessage `json:"documents"`
		TopN            *int              `json:"top_n"`
		ReturnDocuments bool              `json:"return_documents"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, "request body is not a valid rerank request"
	}
	if in.Query == nil || *in.Query == "" {
		return nil, "query is required"
	}
	if len(in.Documents) == 0 {
		return nil, "documents must have at least one document"
	}
	r := &RerankRequest{Query: *in.Query, Docs: in.Documents, ReturnDocuments: in.ReturnDocuments}
	if in.TopN != nil {
		if *in.TopN < 1 {
			return nil, "top_n must be at least 1"
		}
		r.TopN = *in.TopN
	}
	for _, d := range in.Documents {
		var s string
		if json.Unmarshal(d, &s) == nil {
			r.Texts = append(r.Texts, s)
			continue
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(d, &obj) != nil {
			return nil, "each document must be a string or an object"
		}
		if json.Unmarshal(obj["text"], &s) == nil && s != "" {
			r.Texts = append(r.Texts, s)
		} else {
			r.Texts = append(r.Texts, string(d))
		}
	}
	return r, ""
}

// Response returns the answer to the request for an upstream's scores:
// the best first, at most top_n of them, with the documents when they
// were asked for. Scores of documents the request does not have are
// left out.
func (r *RerankRequest) Response(id string, results []RerankResult) *Response {
	kept := make([]RerankResult, 0, len(results))
	for _, x := range results {
		if x.Index >= 0 && x.Index < len(r.Texts) {
			kept = append(kept, x)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].Score > kept[j].Score })
	if r.TopN > 0 && len(kept) > r.TopN {
		kept = kept[:r.TopN]
	}
	out := make([]map[string]any, len(kept))
	for i, x := range kept {
		out[i] = map[string]any{"index": x.Index, "relevance_score": x.Score}
		if r.ReturnDocuments {
			out[i]["document"] = map[string]any{"text": r.Texts[x.Index]}
		}
	}
	b, _ := json.Marshal(map[string]any{"id": id, "results": out, "meta": map[string]any{"billed_units": map[string]any{"search_units": 1}}})
	return &Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(b)),
		Usage:      &usage.Tokens{},
	}
}
