// Package nativetest helps test adapters for providers with their own
// APIs against a fake upstream.
package nativetest

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/provider"
)

// Call is one request the fake upstream received.
type Call struct {
	Method, Path, Query string
	Header              http.Header
	Body                string
}

// Upstream is a fake upstream that answers with Handler and records
// every call.
type Upstream struct {
	Calls   []Call
	Handler func(w http.ResponseWriter, r *http.Request, body string)
}

// Start serves the fake and returns a provider config pointing at it.
func (u *Upstream) Start(t *testing.T) config.Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.Calls = append(u.Calls, Call{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), string(b)})
		u.Handler(w, r, string(b))
	}))
	t.Cleanup(srv.Close)
	return config.Provider{BaseURL: srv.URL, APIKey: "up-key", Timeout: config.Duration(5 * time.Second), Headers: map[string]string{"X-Extra": "1"}}
}

// Send sends a request and reads the whole answer.
func Send(t *testing.T, p provider.Provider, req *provider.Request) (*provider.Response, string) {
	t.Helper()
	resp, err := p.Send(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp, string(b)
}
