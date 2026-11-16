package bedrock

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/provider"
)

func TestRerankOnBedrock(t *testing.T) {
	var path string
	var got map[string]any
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, got = r.URL.EscapedPath(), nil
		_ = json.NewDecoder(r.Body).Decode(&got)
		if fail {
			w.Header().Set("X-Amzn-Errortype", "ThrottlingException")
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"message":"slow down"}`)
			return
		}
		_, _ = io.WriteString(w, `{"results":[{"index":0,"relevance_score":0.2},{"index":2,"relevance_score":0.8}]}`)
	}))
	defer srv.Close()
	cfg := config.Provider{Type: config.ProviderBedrock, BaseURL: srv.URL, Region: "us-west-2", Timeout: config.Duration(5 * time.Second)}
	p := newWithCredentials(cfg, srv.Client(), staticCreds)
	for model, want := range map[string]bool{"cohere.rerank-v3-5:0": true, "amazon.rerank-v1:0": true, "amazon.nova-pro-v1:0": false} {
		if p.SupportsModel(provider.Rerank, model) != want || p.SupportsModel(provider.ChatCompletions, model) == want {
			t.Fatalf("SupportsModel %s", model)
		}
	}
	send := func(model, body string) (int, string) {
		t.Helper()
		resp, err := p.Send(context.Background(), &provider.Request{Endpoint: provider.Rerank, Model: model, Body: []byte(body)})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	const body = `{"model":"rank","query":"capital of Laos","documents":["Oslo",{"text":"Bangkok"},{"title":"Vientiane"}],"top_n":5,"return_documents":true}`
	status, out := send("cohere.rerank-v3-5:0", body)
	if status != 200 || path != "/model/cohere.rerank-v3-5%3A0/invoke" {
		t.Fatalf("%d %s at %s", status, out, path)
	}
	docs := got["documents"].([]any)
	// top_n is capped at the number of documents, and an object without
	// text is sent as its JSON.
	if got["api_version"] != 2.0 || got["query"] != "capital of Laos" || got["top_n"] != 3.0 || docs[1] != "Bangkok" || docs[2] != `{"title":"Vientiane"}` || got["model"] != nil {
		t.Fatalf("body: %v", got)
	}
	var res struct {
		Results []struct {
			Index    int     `json:"index"`
			Score    float64 `json:"relevance_score"`
			Document struct {
				Text string `json:"text"`
			} `json:"document"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || len(res.Results) != 2 || res.Results[0].Index != 2 || res.Results[0].Score != 0.8 || res.Results[1].Document.Text != "Oslo" {
		t.Fatalf("answer: %s", out)
	}
	// Amazon's model takes no API version.
	if status, out := send("amazon.rerank-v1:0", `{"query":"q","documents":["a","b","c"]}`); status != 200 || got["api_version"] != nil || got["top_n"] != nil || strings.Contains(out, "document") {
		t.Fatalf("amazon: %d %s %v", status, out, got)
	}
	if status, out := send("amazon.rerank-v1:0", `{"query":"q","documents":[]}`); status != 400 {
		t.Fatalf("no documents: %d %s", status, out)
	}
	fail = true
	if status, out := send("amazon.rerank-v1:0", `{"query":"q","documents":["a"]}`); status != 429 || !strings.Contains(out, "slow down") {
		t.Fatalf("upstream error: %d %s", status, out)
	}
}
