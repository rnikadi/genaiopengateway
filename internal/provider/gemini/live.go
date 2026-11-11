package gemini

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/coder/websocket"
)

// livePath is the Live API's WebSocket below the Gemini API host.
const livePath = "/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"

// isLive reports whether a model serves the Live API, such as
// gemini-live-2.5-flash-preview or
// gemini-2.5-flash-native-audio-preview-09-2025.
func isLive(model string) bool {
	return strings.Contains(model, "-live") || strings.Contains(model, "native-audio")
}

// DialLive implements provider.LiveDialer: the Live WebSocket at the
// base URL's host, with the provider's key in a header.
func (p *Provider) DialLive(ctx context.Context, header http.Header) (*websocket.Conn, *http.Response, error) {
	u, err := url.Parse(p.cfg.BaseURL + livePath)
	if err != nil {
		return nil, nil, err
	}
	u.Scheme = map[string]string{"https": "wss", "http": "ws"}[u.Scheme]
	h := header.Clone()
	if h == nil {
		h = http.Header{}
	}
	for k, v := range p.cfg.Headers {
		h.Set(k, v)
	}
	if key := p.cfg.APIKey.Value(); key != "" {
		h.Set("X-Goog-Api-Key", key)
	}
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout.Std())
	defer cancel()
	return websocket.Dial(ctx, u.String(), &websocket.DialOptions{HTTPHeader: h, HTTPClient: p.client})
}
