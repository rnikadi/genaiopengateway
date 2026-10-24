// Package bedrock adapts Amazon Bedrock: chat completions through the
// Converse API for every model, /v1/messages for Claude through
// InvokeModel, embeddings for Titan and Cohere, and images for Nova
// Canvas and Titan Image (docs/cloud-providers.md, docs/modalities.md).
package bedrock

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/provider/anthropic"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

// claudeVersion is the anthropic_version Bedrock expects in the body.
const claudeVersion = "bedrock-2023-05-31"

// maxBody caps non-streaming responses held in memory.
const maxBody = 64 << 20

// Provider is a Bedrock upstream.
type Provider struct {
	cfg    config.Provider
	client *http.Client
	base   string
	auth   *signer
	claude *anthropic.Provider
}

// New returns an adapter for cfg. Requests carry the Bedrock API key as
// a bearer token when one is set, and are otherwise signed with SigV4
// using the AWS default credential chain.
func New(cfg config.Provider, client *http.Client) *Provider {
	return newWithCredentials(cfg, client, nil)
}

func newWithCredentials(cfg config.Provider, client *http.Client, creds aws.CredentialsProvider) *Provider {
	p := &Provider{cfg: cfg, client: client, base: cfg.BaseURL,
		auth: &signer{apiKey: cfg.APIKey.Value(), region: cfg.Region, creds: creds, v4: v4.NewSigner()}}
	if p.base == "" {
		p.base = "https://bedrock-runtime." + cfg.Region + ".amazonaws.com"
	}
	p.claude = anthropic.NewUpstream(cfg, client, anthropic.Upstream{
		URL: func(model string, stream bool) string {
			if stream {
				return p.url(model, "invoke-with-response-stream")
			}
			return p.url(model, "invoke")
		},
		Version:     claudeVersion,
		BetasInBody: true,
		Authorize:   p.auth.authorize,
		Decode:      invokeStream,
	})
	return p
}

// url returns the URL of an action on a model. The model id is escaped
// as the AWS SDK does, since ids and ARNs contain colons and slashes.
func (p *Provider) url(model, action string) string {
	return p.base + "/model/" + strings.ReplaceAll(url.PathEscape(model), ":", "%3A") + "/" + action
}

func isClaude(model string) bool { return strings.Contains(model, "anthropic.") }

func isEmbedding(model string) bool {
	return strings.Contains(model, "amazon.titan-embed") || strings.Contains(model, "cohere.embed")
}

// Supports implements provider.Provider.
func (p *Provider) Supports(e provider.Endpoint) bool {
	return e == provider.ChatCompletions || e == provider.Messages || e == provider.Embeddings || e == provider.ImageGenerations
}

// SupportsModel implements provider.ModelSupporter: Messages is served
// for Claude models, embeddings for Titan and Cohere embedding models,
// images for Nova Canvas and Titan Image, and chat completions for
// every other model.
func (p *Provider) SupportsModel(e provider.Endpoint, model string) bool {
	switch e {
	case provider.ChatCompletions:
		return !isEmbedding(model) && !isImageModel(model)
	case provider.ImageGenerations:
		return isImageModel(model)
	case provider.Messages:
		return isClaude(model)
	case provider.Embeddings:
		return isEmbedding(model)
	}
	return false
}

// Send implements provider.Provider.
func (p *Provider) Send(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	switch req.Endpoint {
	case provider.ChatCompletions:
		return p.chat(ctx, req)
	case provider.Embeddings:
		return p.embed(ctx, req)
	case provider.ImageGenerations:
		return p.images(ctx, req)
	}
	resp, err := p.claude.Send(ctx, req)
	if err != nil || resp.StatusCode == http.StatusOK {
		return resp, err
	}
	// Bedrock's errors are not in the Anthropic shape the client expects.
	e := readError(resp)
	b, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": anthropicType(e.status), "message": e.message}})
	return errorBody(e.status, b, resp.Header), nil
}

// post sends a JSON body to an action on a model.
func (p *Provider) post(ctx context.Context, model, action string, body []byte) (*provider.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout.Std())
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url(model, action), bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	for k, v := range p.cfg.Headers {
		hreq.Header.Set(k, v)
	}
	if err := p.auth.authorize(hreq); err != nil {
		cancel()
		return &provider.Response{StatusCode: http.StatusBadGateway, Header: http.Header{},
			Body: io.NopCloser(strings.NewReader(`{"message":` + quote("upstream credentials: "+err.Error()) + `}`)), Usage: &usage.Tokens{}}, nil
	}
	resp, err := p.client.Do(hreq)
	if err != nil {
		cancel()
		return nil, err
	}
	return &provider.Response{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       &cancelOnClose{ReadCloser: resp.Body, cancel: cancel},
		Usage:      &usage.Tokens{},
	}, nil
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// signer authorizes requests with a Bedrock API key or SigV4.
type signer struct {
	apiKey, region string
	v4             *v4.Signer
	mu             sync.Mutex
	creds          aws.CredentialsProvider
}

func (s *signer) authorize(r *http.Request) error {
	// Streams answer in the AWS event stream encoding.
	if strings.HasSuffix(r.URL.Path, "-stream") {
		r.Header.Set("Accept", "application/vnd.amazon.eventstream")
	} else {
		r.Header.Set("Accept", "application/json")
	}
	if s.apiKey != "" {
		r.Header.Set("Authorization", "Bearer "+s.apiKey)
		return nil
	}
	creds, err := s.credentials(r.Context())
	if err != nil {
		return err
	}
	c, err := creds.Retrieve(r.Context())
	if err != nil {
		return err
	}
	sum := sha256.New()
	if r.GetBody != nil {
		body, err := r.GetBody()
		if err != nil {
			return err
		}
		_, _ = io.Copy(sum, body)
	}
	return s.v4.SignHTTP(r.Context(), c, r, hex.EncodeToString(sum.Sum(nil)), "bedrock", s.region, time.Now())
}

// credentials loads the AWS default credential chain on first use, so a
// gateway without credentials starts and fails over at request time; a
// failed load is retried on the next request.
func (s *signer) credentials(ctx context.Context) (aws.CredentialsProvider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.creds != nil {
		return s.creds, nil
	}
	cfg, err := awsconfig.LoadDefaultConfig(context.WithoutCancel(ctx), awsconfig.WithRegion(s.region))
	if err != nil {
		return nil, err
	}
	if cfg.Credentials == nil {
		return nil, errors.New("no AWS credentials found")
	}
	s.creds = cfg.Credentials
	return s.creds, nil
}

// bedrockError is an error response from Bedrock.
type bedrockError struct {
	status       int
	typ, message string
}

// errorStatus maps Bedrock exceptions to the statuses fallback expects:
// throttling and quotas are 429, capacity problems are 5xx.
var errorStatus = map[string]int{
	"ThrottlingException":           http.StatusTooManyRequests,
	"ServiceQuotaExceededException": http.StatusTooManyRequests,
	"ModelNotReadyException":        http.StatusServiceUnavailable,
	"ServiceUnavailableException":   http.StatusServiceUnavailable,
	"InternalServerException":       http.StatusInternalServerError,
	"ModelTimeoutException":         http.StatusGatewayTimeout,
	"ValidationException":           http.StatusBadRequest,
	"AccessDeniedException":         http.StatusForbidden,
	"ResourceNotFoundException":     http.StatusNotFound,
}

func readError(resp *provider.Response) bedrockError {
	defer resp.Body.Close()
	buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	typ, _, _ := strings.Cut(resp.Header.Get("X-Amzn-Errortype"), ":")
	var body struct {
		Message string `json:"message"`
		Type    string `json:"__type"`
	}
	msg := strings.TrimSpace(string(buf))
	if json.Unmarshal(buf, &body) == nil {
		if body.Message != "" {
			msg = body.Message
		}
		if typ == "" && body.Type != "" {
			_, typ, _ = strings.Cut(body.Type, "#")
		}
	}
	return exception(resp.StatusCode, typ, msg)
}

func exception(status int, typ, msg string) bedrockError {
	if s, ok := errorStatus[typ]; ok {
		status = s
	}
	if typ == "" {
		typ = "api_error"
	}
	return bedrockError{status: status, typ: typ, message: msg}
}

func anthropicType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable:
		return "overloaded_error"
	}
	return "api_error"
}

// openAIError converts a Bedrock error response to the OpenAI shape.
func openAIError(resp *provider.Response) *provider.Response {
	e := readError(resp)
	return errorResponse(e.status, e.typ, e.message)
}

// errorResponse builds an OpenAI-shaped error response.
func errorResponse(status int, typ, msg string) *provider.Response {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": typ}})
	return errorBody(status, b, nil)
}

func errorBody(status int, b []byte, upstream http.Header) *provider.Response {
	h := http.Header{"Content-Type": {"application/json"}}
	if ra := upstream.Get("Retry-After"); ra != "" {
		h.Set("Retry-After", ra)
	}
	return &provider.Response{StatusCode: status, Header: h, Body: io.NopCloser(bytes.NewReader(b)), Usage: &usage.Tokens{}}
}

// --- event streams ---

// readFrames calls fn with each message of an AWS event stream until
// the stream ends.
func readFrames(r io.Reader, fn func(eventstream.Message) error) error {
	dec := eventstream.NewDecoder()
	buf := make([]byte, 0, 64<<10)
	for {
		m, err := dec.Decode(r, buf)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := fn(m); err != nil {
			return err
		}
	}
}

func header(m eventstream.Message, name string) string {
	v := m.Headers.Get(name)
	if v == nil {
		return ""
	}
	if s, ok := v.(eventstream.StringValue); ok {
		return string(s)
	}
	return v.String()
}

// frameError returns the error an exception or error message carries.
func frameError(m eventstream.Message) (bedrockError, bool) {
	switch header(m, ":message-type") {
	case "exception":
		var body struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(m.Payload, &body)
		return exception(http.StatusInternalServerError, header(m, ":exception-type"), body.Message), true
	case "error":
		return exception(http.StatusInternalServerError, header(m, ":error-code"), header(m, ":error-message")), true
	}
	return bedrockError{}, false
}

// invokeStream turns an InvokeModelWithResponseStream response for
// Claude into Anthropic server-sent events: each chunk carries one
// Anthropic event, base64 encoded.
func invokeStream(upstream io.ReadCloser) io.ReadCloser {
	return pipe(upstream, func(w io.Writer) error {
		return readFrames(upstream, func(m eventstream.Message) error {
			if e, ok := frameError(m); ok {
				b, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": anthropicType(e.status), "message": e.message}})
				_, err := fmt.Fprintf(w, "event: error\ndata: %s\n\n", b)
				return err
			}
			if header(m, ":event-type") != "chunk" {
				return nil
			}
			var chunk struct {
				Bytes string `json:"bytes"`
			}
			if json.Unmarshal(m.Payload, &chunk) != nil {
				return nil
			}
			data, err := base64.StdEncoding.DecodeString(chunk.Bytes)
			if err != nil {
				return nil
			}
			var ev struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(data, &ev) != nil || ev.Type == "" {
				return nil
			}
			_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
			return err
		})
	})
}

// pipe runs produce in a goroutine writing to the returned reader.
// Closing the reader stops it and waits for it to finish.
func pipe(upstream io.ReadCloser, produce func(io.Writer) error) io.ReadCloser {
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer upstream.Close()
		pw.CloseWithError(produce(pw))
	}()
	return &piped{PipeReader: pr, upstream: upstream, done: done}
}

type piped struct {
	*io.PipeReader
	upstream io.Closer
	done     chan struct{}
}

func (p *piped) Close() error {
	_ = p.PipeReader.Close()
	_ = p.upstream.Close()
	<-p.done
	return nil
}

// cancelOnClose releases the request context when the body is closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}
