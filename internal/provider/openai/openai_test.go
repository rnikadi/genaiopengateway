package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

func newProvider(t *testing.T, h http.HandlerFunc) *Provider {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(config.Provider{
		BaseURL: srv.URL + "/v1",
		APIKey:  "upstream-secret",
		Timeout: config.Duration(5 * time.Second),
		Headers: map[string]string{"X-Extra": "1"},
	}, srv.Client())
}

func TestNonStreamingReadsUsageAndSendsCredentials(t *testing.T) {
	p := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" || r.Header.Get("Authorization") != "Bearer upstream-secret" || r.Header.Get("X-Extra") != "1" {
			t.Errorf("unexpected request %s %v", r.URL.Path, r.Header)
		}
		_, _ = io.WriteString(w, `{"data":[],"usage":{"prompt_tokens":12,"total_tokens":12}}`)
	})
	resp, err := p.Send(context.Background(), &provider.Request{Endpoint: provider.Embeddings, Body: []byte(`{"model":"m"}`)})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || len(body) == 0 {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
	if *resp.Usage != (usage.Tokens{InputTokens: 12, Reported: true}) {
		t.Fatalf("usage = %+v", *resp.Usage)
	}
}

func TestStreamingAddsIncludeUsageAndTapsFinalChunk(t *testing.T) {
	p := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			StreamOptions struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !body.StreamOptions.IncludeUsage {
			t.Error("include_usage not requested")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n")
	})
	resp, err := p.Send(context.Background(), &provider.Request{
		Endpoint: provider.ChatCompletions, Stream: true,
		Body: []byte(`{"model":"m","stream":true,"messages":[]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.Usage.InputTokens != 3 || resp.Usage.OutputTokens != 1 {
		t.Fatalf("usage = %+v", *resp.Usage)
	}
}

func TestErrorStatusIsReturnedNotErrored(t *testing.T) {
	p := newProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down"}}`)
	})
	resp, err := p.Send(context.Background(), &provider.Request{Endpoint: provider.ChatCompletions, Body: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestWithStreamUsage(t *testing.T) {
	cases := map[string]string{
		`{"model":"m"}`: `{"model":"m","stream_options":{"include_usage":true}}`,
		`{"stream_options":{"include_usage":false}}`: `{"stream_options":{"include_usage":false}}`,
		`{"stream_options":{"other":1}}`:             `{"stream_options":{"include_usage":true,"other":1}}`,
		`not json`:                                   `not json`,
	}
	for in, want := range cases {
		if got := string(withStreamUsage([]byte(in))); got != want {
			t.Errorf("withStreamUsage(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestSupports(t *testing.T) {
	p := New(config.Provider{}, http.DefaultClient)
	if !p.Supports(provider.Responses) || p.Supports(provider.Messages) {
		t.Fatal("wrong endpoint support")
	}
}
