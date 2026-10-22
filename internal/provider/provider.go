// Package provider defines the adapter interface between the gateway and
// upstream model providers.
package provider

import (
	"context"
	"io"
	"net/http"

	"github.com/rnikadi/genaiopengateway/internal/usage"
)

// Endpoint is a client-facing API the gateway serves.
type Endpoint string

// Endpoints served in v0.1.
const (
	ChatCompletions Endpoint = "chat.completions"
	Embeddings      Endpoint = "embeddings"
	Responses       Endpoint = "responses"
	Messages        Endpoint = "messages"
)

// Request is a client request already resolved to one target. Body is
// the client's JSON with "model" rewritten to the upstream model.
type Request struct {
	Endpoint Endpoint
	Model    string
	Body     []byte
	Stream   bool
	// Header carries client headers an adapter may forward, such as
	// anthropic-version on passthrough.
	Header http.Header
}

// Response is an upstream response in the client's format. Body must be
// closed. Once Body has been read to EOF, Usage holds what the upstream
// reported.
type Response struct {
	StatusCode int
	Header     http.Header
	Body       io.ReadCloser
	Usage      *usage.Tokens
}

// Provider is an adapter for one configured upstream.
type Provider interface {
	// Supports reports whether the adapter serves an endpoint.
	Supports(Endpoint) bool
	// Send performs the request. It returns an error only when no
	// response was received; upstream error statuses are returned as
	// responses.
	Send(ctx context.Context, req *Request) (*Response, error)
}

// Retryable reports whether a status should trigger fallback to the next
// target: rate limiting or a server error other than Not Implemented.
func Retryable(status int) bool {
	return status == http.StatusTooManyRequests || (status >= 500 && status != http.StatusNotImplemented)
}

// Drain discards and closes a response body so the connection is reused.
func Drain(r *Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
	_ = r.Body.Close()
}
