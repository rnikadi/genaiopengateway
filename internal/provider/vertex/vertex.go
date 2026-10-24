// Package vertex adapts Vertex AI: Gemini models through the gemini
// adapter and Claude models through the anthropic adapter, with Google
// credentials (docs/cloud-providers.md).
package vertex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/provider/anthropic"
	"github.com/rnikadi/genaiopengateway/internal/provider/gemini"
)

// Publishers the adapter serves.
const (
	google_    = "google"
	anthropic_ = "anthropic"
)

// claudeVersion is the anthropic_version Vertex AI expects in the body.
const claudeVersion = "vertex-2023-10-16"

const scope = "https://www.googleapis.com/auth/cloud-platform"

// Provider is a Vertex AI upstream.
type Provider struct {
	gemini *gemini.Provider
	claude *anthropic.Provider
}

// New returns an adapter for cfg, using Application Default Credentials
// or cfg.CredentialsFile.
func New(cfg config.Provider, client *http.Client) *Provider {
	return newWithTokens(cfg, client, &tokens{file: cfg.CredentialsFile})
}

func newWithTokens(cfg config.Provider, client *http.Client, t *tokens) *Provider {
	base := cfg.BaseURL
	if base == "" {
		base = "https://" + Host(cfg.Location)
	}
	prefix := base + "/v1/projects/" + url.PathEscape(cfg.Project) + "/locations/" + url.PathEscape(cfg.Location) + "/publishers/"
	modelURL := func(model, method string) string {
		pub, name := Split(model)
		return prefix + pub + "/models/" + url.PathEscape(name) + ":" + method
	}
	return &Provider{
		gemini: gemini.NewUpstream(cfg, client, gemini.Upstream{URL: modelURL, Authorize: t.authorize}),
		claude: anthropic.NewUpstream(cfg, client, anthropic.Upstream{
			URL: func(model string, stream bool) string {
				if stream {
					return modelURL(model, "streamRawPredict")
				}
				return modelURL(model, "rawPredict")
			},
			Version:   claudeVersion,
			Authorize: t.authorize,
		}),
	}
}

// Host returns the Vertex AI host for a location: the global endpoint,
// a multi-region endpoint, or a regional one.
func Host(location string) string {
	switch location {
	case "global":
		return "aiplatform.googleapis.com"
	case "us", "eu":
		return "aiplatform." + location + ".rep.googleapis.com"
	}
	return location + "-aiplatform.googleapis.com"
}

// Split returns the publisher and model of a target model written as
// publisher/model; a model without a publisher is Google's.
func Split(model string) (publisher, name string) {
	if p, n, ok := strings.Cut(model, "/"); ok {
		return p, n
	}
	return google_, model
}

// Supports implements provider.Provider.
func (p *Provider) Supports(e provider.Endpoint) bool {
	return e == provider.ChatCompletions || e == provider.Messages || e == provider.GenerateContent ||
		e == provider.ImageGenerations || e == provider.Speech
}

// SupportsModel implements provider.ModelSupporter: Messages is served
// for Claude models, and Google's models serve what the gemini adapter
// serves for them: chat, Imagen images or text to speech.
func (p *Provider) SupportsModel(e provider.Endpoint, model string) bool {
	switch pub, name := Split(model); pub {
	case google_:
		return p.gemini.SupportsModel(e, name)
	case anthropic_:
		return p.claude.Supports(e)
	}
	return false
}

// Send implements provider.Provider.
func (p *Provider) Send(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	switch pub, _ := Split(req.Model); pub {
	case google_:
		return p.gemini.Send(ctx, req)
	case anthropic_:
		return p.claude.Send(ctx, req)
	default:
		return nil, fmt.Errorf("vertex: publisher %q is not supported", pub)
	}
}

// tokens gets OAuth tokens for Google APIs. Credentials are found on
// first use, so a gateway without them starts and fails over at request
// time; a failed lookup is retried on the next request.
type tokens struct {
	file string
	mu   sync.Mutex
	ts   oauth2.TokenSource
}

func (t *tokens) authorize(r *http.Request) error {
	ts, err := t.source(r.Context())
	if err != nil {
		return err
	}
	tok, err := ts.Token()
	if err != nil {
		return err
	}
	tok.SetAuthHeader(r)
	return nil
}

func (t *tokens) source(ctx context.Context) (oauth2.TokenSource, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ts != nil {
		return t.ts, nil
	}
	// The token source outlives this request.
	ctx = context.WithoutCancel(ctx)
	var creds *google.Credentials
	var err error
	if t.file == "" {
		creds, err = google.FindDefaultCredentials(ctx, scope)
	} else {
		creds, err = fromFile(ctx, t.file)
	}
	if err != nil {
		return nil, err
	}
	t.ts = creds.TokenSource
	return t.ts, nil
}

// fromFile loads a service account key or a workload identity
// federation configuration.
func fromFile(ctx context.Context, path string) (*google.Credentials, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("credentials_file: %w", err)
	}
	var f struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(b, &f) != nil {
		return nil, fmt.Errorf("credentials_file is not JSON")
	}
	switch t := google.CredentialsType(f.Type); t {
	case google.ServiceAccount, google.ExternalAccount, google.ImpersonatedServiceAccount:
		return google.CredentialsFromJSONWithType(ctx, b, t, scope)
	}
	return nil, fmt.Errorf("credentials_file type %q is not supported", f.Type)
}
