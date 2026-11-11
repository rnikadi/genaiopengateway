package server

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// liveUpstream is a fake Gemini Live API: it acknowledges the setup and
// answers each client message with a turn that reports usage, in binary
// frames as Google does.
type liveUpstream struct {
	*httptest.Server
	mu     sync.Mutex
	setups []map[string]any
	keys   []string
}

func newLiveUpstream(t *testing.T) *liveUpstream {
	u := &liveUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent" {
			http.NotFound(w, r)
			return
		}
		u.mu.Lock()
		u.keys = append(u.keys, r.Header.Get("X-Goog-Api-Key"))
		u.mu.Unlock()
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		var setup map[string]any
		_ = json.Unmarshal(data, &setup)
		u.mu.Lock()
		u.setups = append(u.setups, setup)
		u.mu.Unlock()
		_ = c.Write(ctx, websocket.MessageBinary, []byte(`{"setupComplete":{}}`))
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
			_ = c.Write(ctx, websocket.MessageBinary, []byte(`{"serverContent":{"turnComplete":true},"usageMetadata":{"promptTokenCount":1000,"responseTokenCount":500,`+
				`"promptTokensDetails":[{"modality":"TEXT","tokenCount":200},{"modality":"AUDIO","tokenCount":800}],"responseTokensDetails":[{"modality":"AUDIO","tokenCount":500}]}}`))
		}
	}))
	t.Cleanup(u.Close)
	return u
}

func liveConfig(url string) string {
	return `providers:
  gem: {type: gemini, base_url: "` + url + `", api_key: goog-upstream-key}
models:
  live: {targets: [{provider: gem, model: gemini-live-2.5-flash-preview, price: {input_per_mtok: 0.5, output_per_mtok: 2, audio_input_per_mtok: 3, audio_output_per_mtok: 12}}]}
  chat: {targets: [{provider: gem, model: gemini-2.5-flash}]}
guardrails:
  no-pii: {type: pii, detect: [email], action: redact, stages: [input]}
keys:
  - {name: dev, key: ` + devKey + `}
  - {name: guarded, key: ` + guardKey + `, guardrails: [no-pii]}
`
}

func dialLive(t *testing.T, gw *httptest.Server, query string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(gw.URL, "http")+"/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent?"+query, nil)
}

func readLive(t *testing.T, c *websocket.Conn) (map[string]any, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	return m, nil
}

func TestLiveRelay(t *testing.T) {
	up := newLiveUpstream(t)
	logs := &lockedBuffer{}
	gw := httptest.NewServer(gatewayLog(t, liveConfig(up.URL), logs))
	t.Cleanup(gw.Close)

	c, _, err := dialLive(t, gw, "key="+devKey)
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, `{"setup":{"model":"models/live","generationConfig":{"responseModalities":["AUDIO"]}}}`)
	if m, err := readLive(t, c); err != nil || m["setupComplete"] == nil {
		t.Fatalf("setup: %v %v", m, err)
	}
	// A second setup could name another model, so it is dropped.
	send(t, c, `{"setup":{"model":"models/gemini-2.5-pro"}}`)
	send(t, c, `{"realtimeInput":{"text":"hello"}}`)
	if m, err := readLive(t, c); err != nil || m["usageMetadata"] == nil {
		t.Fatalf("turn: %v %v", m, err)
	}
	_ = c.Close(websocket.StatusNormalClosure, "")

	up.mu.Lock()
	setup := up.setups[0]["setup"].(map[string]any)
	if setup["model"] != "models/gemini-live-2.5-flash-preview" || setup["generationConfig"] == nil || len(up.setups) != 1 || up.keys[0] != "goog-upstream-key" {
		t.Fatalf("upstream saw %v with %v", up.setups, up.keys)
	}
	up.mu.Unlock()

	// 200 text in at 0.5, 800 audio in at 3, 500 audio out at 12.
	want := (200*0.5 + 800*3 + 500*12) / 1e6
	deadline := time.Now().Add(5 * time.Second)
	for {
		l := logs.lastRequest(t)
		if l != nil && l["endpoint"] == "live" {
			if l["status"] != 101.0 || l["model"] != "live" || l["input_tokens"] != 1000.0 || math.Abs(l["cost_usd"].(float64)-want) > 1e-12 {
				t.Fatalf("log = %v", l)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no live request log")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLiveRefusals(t *testing.T) {
	up := newLiveUpstream(t)
	gw := httptest.NewServer(gateway(t, liveConfig(up.URL), nil))
	t.Cleanup(gw.Close)

	if _, resp, err := dialLive(t, gw, "key=wrong-key"); err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a wrong key: %v", err)
	}
	if _, resp, err := dialLive(t, gw, "key="+guardKey); err == nil || resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a key with guardrails: %v", err)
	}
	for setup, want := range map[string]string{
		`{"setup":{"model":"models/chat"}}`:    "does not support",
		`{"setup":{"model":"models/missing"}}`: "not found",
		`{"realtimeInput":{"text":"hi"}}`:      "setup",
	} {
		c, _, err := dialLive(t, gw, "key="+devKey)
		if err != nil {
			t.Fatal(err)
		}
		send(t, c, setup)
		_, err = readLive(t, c)
		var ce websocket.CloseError
		if !errors.As(err, &ce) || ce.Code != websocket.StatusPolicyViolation || !strings.Contains(ce.Reason, want) {
			t.Errorf("%s: %v, want a close naming %q", setup, err, want)
		}
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.setups) != 0 {
		t.Errorf("refused sessions reached the upstream: %v", up.setups)
	}
}
