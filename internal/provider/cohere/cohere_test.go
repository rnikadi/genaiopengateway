package cohere

import (
	"net/http"
	"strings"
	"testing"

	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/provider/native/nativetest"
)

func TestPaths(t *testing.T) {
	up := &nativetest.Upstream{Handler: func(w http.ResponseWriter, r *http.Request, _ string) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/compatibility/v1/chat/completions":
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
		case "/v2/rerank":
			_, _ = w.Write([]byte(`{"results":[{"index":1,"relevance_score":0.9}],"meta":{"billed_units":{"search_units":1}}}`))
		default:
			w.WriteHeader(404)
		}
	}}
	p := New(up.Start(t), http.DefaultClient)
	resp, _ := nativetest.Send(t, p, &provider.Request{Endpoint: provider.ChatCompletions, Model: "command-a-03-2025", Body: []byte(`{"model":"command-a-03-2025","messages":[]}`)})
	if resp.StatusCode != 200 || resp.Usage.InputTokens != 3 || up.Calls[0].Header.Get("Authorization") != "Bearer up-key" {
		t.Fatalf("chat %d %+v", resp.StatusCode, resp.Usage)
	}
	resp, body := nativetest.Send(t, p, &provider.Request{Endpoint: provider.Rerank, Model: "rerank-v3.5", Body: []byte(`{"model":"rerank-v3.5","query":"q","documents":["a","b"]}`)})
	if resp.StatusCode != 200 || !strings.Contains(body, `"index":1`) || up.Calls[1].Path != "/v2/rerank" {
		t.Fatalf("rerank %d %s", resp.StatusCode, body)
	}
	if !p.SupportsModel(provider.Rerank, "rerank-v3.5") || !p.SupportsModel(provider.Embeddings, "embed-v4.0") || !p.SupportsModel(provider.ChatCompletions, "command-a-03-2025") || p.SupportsModel(provider.ChatCompletions, "embed-v4.0") {
		t.Error("SupportsModel")
	}
}
