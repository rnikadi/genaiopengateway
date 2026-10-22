package router

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

// fake is a provider that answers with a fixed status or error.
type fake struct {
	status   int
	err      error
	supports bool
	got      []string
}

func (f *fake) Supports(provider.Endpoint) bool { return f.supports }

func (f *fake) Send(_ context.Context, r *provider.Request) (*provider.Response, error) {
	f.got = append(f.got, string(r.Body))
	if f.err != nil {
		return nil, f.err
	}
	return &provider.Response{StatusCode: f.status, Body: io.NopCloser(strings.NewReader("{}")), Usage: &usage.Tokens{}}, nil
}

func newRouter(fakes map[string]*fake, targets ...string) *Router {
	r := &Router{models: map[string]config.Model{}, providers: map[string]provider.Provider{}}
	var m config.Model
	for _, name := range targets {
		r.providers[name] = fakes[name]
		m.Targets = append(m.Targets, config.Target{Provider: name, Model: name + "-model"})
	}
	r.models["virtual"] = m
	return r
}

func body() map[string]json.RawMessage {
	return map[string]json.RawMessage{"model": json.RawMessage(`"virtual"`), "x": json.RawMessage(`1`)}
}

func TestFallbackOrder(t *testing.T) {
	cases := []struct {
		name       string
		a, b       *fake
		wantTarget string
		wantFailed int
		wantErr    error
	}{
		{"first ok", &fake{status: 200, supports: true}, &fake{status: 200, supports: true}, "a", 0, nil},
		{"429 falls back", &fake{status: 429, supports: true}, &fake{status: 200, supports: true}, "b", 1, nil},
		{"500 falls back", &fake{status: 500, supports: true}, &fake{status: 200, supports: true}, "b", 1, nil},
		{"network error falls back", &fake{err: errors.New("dial"), supports: true}, &fake{status: 200, supports: true}, "b", 1, nil},
		{"400 does not fall back", &fake{status: 400, supports: true}, &fake{status: 200, supports: true}, "a", 0, nil},
		{"501 does not fall back", &fake{status: 501, supports: true}, &fake{status: 200, supports: true}, "a", 0, nil},
		{"last target error returned", &fake{status: 500, supports: true}, &fake{status: 503, supports: true}, "b", 1, nil},
		{"unsupported target skipped", &fake{status: 200, supports: false}, &fake{status: 200, supports: true}, "b", 0, nil},
		{"all network errors", &fake{err: errors.New("x"), supports: true}, &fake{err: errors.New("y"), supports: true}, "", 2, ErrAllTargetsFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRouter(map[string]*fake{"a": c.a, "b": c.b}, "a", "b")
			res, err := r.Route(context.Background(), provider.ChatCompletions, "virtual", body(), provider.Request{})
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("err = %v, want %v", err, c.wantErr)
			}
			if len(res.Failed) != c.wantFailed {
				t.Fatalf("failed attempts = %d, want %d", len(res.Failed), c.wantFailed)
			}
			if c.wantTarget != "" && res.Target.Provider != c.wantTarget {
				t.Fatalf("target = %s, want %s", res.Target.Provider, c.wantTarget)
			}
		})
	}
}

func TestModelRewrittenPerTarget(t *testing.T) {
	a, b := &fake{status: 500, supports: true}, &fake{status: 200, supports: true}
	r := newRouter(map[string]*fake{"a": a, "b": b}, "a", "b")
	if _, err := r.Route(context.Background(), provider.ChatCompletions, "virtual", body(), provider.Request{}); err != nil {
		t.Fatal(err)
	}
	if a.got[0] != `{"model":"a-model","x":1}` || b.got[0] != `{"model":"b-model","x":1}` {
		t.Fatalf("bodies: %s / %s", a.got[0], b.got[0])
	}
}

func TestUnknownAndUnsupported(t *testing.T) {
	r := newRouter(map[string]*fake{"a": {supports: false}}, "a")
	if _, err := r.Route(context.Background(), provider.Embeddings, "nope", body(), provider.Request{}); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("err = %v", err)
	}
	if _, err := r.Route(context.Background(), provider.Embeddings, "virtual", body(), provider.Request{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v", err)
	}
}
