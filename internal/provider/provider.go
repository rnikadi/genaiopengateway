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

// Endpoints the gateway serves.
const (
	ChatCompletions Endpoint = "chat.completions"
	Embeddings      Endpoint = "embeddings"
	Responses       Endpoint = "responses"
	Messages        Endpoint = "messages"
	// GenerateContent is Google's generateContent and
	// streamGenerateContent, served by passthrough.
	GenerateContent Endpoint = "generate_content"

	// Media endpoints (docs/modalities.md).
	ImageGenerations Endpoint = "images.generations"
	ImageEdits       Endpoint = "images.edits"
	Speech           Endpoint = "audio.speech"
	Transcriptions   Endpoint = "audio.transcriptions"
	Translations     Endpoint = "audio.translations"
	Moderations      Endpoint = "moderations"
	Rerank           Endpoint = "rerank"
	// Videos creates a video job; VideoRemix makes a new job from one
	// named by Request.JobID.
	Videos     Endpoint = "videos"
	VideoRemix Endpoint = "videos.remix"
)

// Multipart reports whether an endpoint takes multipart/form-data
// requests, with files.
func (e Endpoint) Multipart() bool {
	return e == ImageEdits || e == Transcriptions || e == Translations || e == Videos
}

// File is one file of a multipart request.
type File struct {
	Field       string
	Filename    string
	ContentType string
	Data        []byte
}

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
	// Files are the files of a multipart request. Body then holds its
	// other fields as a JSON object: strings, and lists for repeated
	// fields.
	Files []File
	// JobID is the upstream job a VideoRemix request starts from.
	JobID string
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

// Forwarder is implemented by adapters that serve calls on upstream
// objects, such as video jobs, which are not model requests. Path is
// below the provider's base URL and may carry a query. The response
// body must be closed.
type Forwarder interface {
	Forward(ctx context.Context, method, path string) (*http.Response, error)
}

// ModelSupporter is implemented by adapters whose endpoints depend on
// the upstream model, such as Vertex AI serving Gemini and Claude.
type ModelSupporter interface {
	SupportsModel(e Endpoint, model string) bool
}

// SupportsModel reports whether p serves an endpoint for model.
func SupportsModel(p Provider, e Endpoint, model string) bool {
	if ms, ok := p.(ModelSupporter); ok {
		return ms.SupportsModel(e, model)
	}
	return p.Supports(e)
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
