package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/rnikadi/genaiopengateway/internal/config"
)

// realtimeUpstream is a fake OpenAI realtime server. It answers
// session.update with session.updated naming the model it was sent, and
// response.create with a response.done carrying usage.
type realtimeUpstream struct {
	*httptest.Server
	mu        sync.Mutex
	models    []string // model query of each session
	protocols []string // subprotocols offered
}

func newRealtimeUpstream(t *testing.T, refuse int) *realtimeUpstream {
	t.Helper()
	u := &realtimeUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if refuse != 0 {
			http.Error(w, `{"error":{"message":"refused"}}`, refuse)
			return
		}
		if r.URL.Path != "/realtime" || r.Header.Get("Authorization") != "Bearer upstream-key" {
			http.Error(w, "bad request", http.StatusUnauthorized)
			return
		}
		u.mu.Lock()
		u.models = append(u.models, r.URL.Query().Get("model"))
		u.protocols = append(u.protocols, r.Header.Values("Sec-WebSocket-Protocol")...)
		u.mu.Unlock()
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"realtime"}})
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"session.created"}`))
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var ev struct {
				Type    string `json:"type"`
				Session struct {
					Model string `json:"model"`
				} `json:"session"`
			}
			_ = json.Unmarshal(data, &ev)
			switch ev.Type {
			case "session.update":
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"session.updated","session":{"model":"`+ev.Session.Model+`"}}`))
			case "response.create":
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.done","response":{"status":"completed","usage":{"total_tokens":1500,"input_tokens":1000,"output_tokens":500,"input_token_details":{"cached_tokens":0,"text_tokens":200,"audio_tokens":800},"output_token_details":{"text_tokens":100,"audio_tokens":400}}}}`))
			}
		}
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *realtimeUpstream) sessions() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.models...)
}

// lockedBuffer holds the gateway's log while a session writes to it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) lastRequest(t *testing.T) map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return lastRequestLog(t, &b.buf)
}

// gatewayLog is gateway with its log written to w.
func gatewayLog(t *testing.T, yaml string, w io.Writer) http.Handler {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, slog.New(slog.NewJSONHandler(w, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	return s.Handler()
}

func realtimeConfig(urls ...string) string {
	cfg := "providers:\n"
	targets := ""
	for i, u := range urls {
		name := string(rune('a' + i))
		cfg += "  " + name + `: {base_url: "` + u + `", api_key: upstream-key}` + "\n"
		targets += `{provider: ` + name + `, model: gpt-realtime-` + name + `, price: {input_per_mtok: 4, output_per_mtok: 16, audio_input_per_mtok: 32, audio_output_per_mtok: 64}},`
	}
	return cfg + `models:
  voice: {targets: [` + strings.TrimSuffix(targets, ",") + `]}
guardrails:
  no-pii: {type: pii, detect: [email], action: redact, stages: [input]}
keys:
  - {name: dev, key: ` + devKey + `}
  - {name: guarded, key: ` + guardKey + `, guardrails: [no-pii]}
  - {name: cheap, key: cheap-key-0123456789, budget: {amount_usd: 0.08, period: daily}}
`
}

func dialGateway(t *testing.T, gw *httptest.Server, query string, opts *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(gw.URL, "http")+"/v1/realtime?"+query, opts)
}

func keyHeader(key string) *websocket.DialOptions {
	return &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + key}}}
}

// readType reads events until one of the given type arrives.
func readType(t *testing.T, c *websocket.Conn, typ string) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for %s: %v", typ, err)
		}
		var ev map[string]any
		_ = json.Unmarshal(data, &ev)
		if ev["type"] == typ {
			return ev
		}
	}
}

func send(t *testing.T, c *websocket.Conn, ev string) {
	t.Helper()
	if err := c.Write(context.Background(), websocket.MessageText, []byte(ev)); err != nil {
		t.Fatal(err)
	}
}

func TestRealtimeRelay(t *testing.T) {
	down, up := newRealtimeUpstream(t, http.StatusServiceUnavailable), newRealtimeUpstream(t, 0)
	logs := &lockedBuffer{}
	gw := httptest.NewServer(gatewayLog(t, realtimeConfig(down.URL, up.URL), logs))
	t.Cleanup(gw.Close)

	c, _, err := dialGateway(t, gw, "model=voice", keyHeader(devKey))
	if err != nil {
		t.Fatal(err)
	}
	readType(t, c, "session.created")
	// The client cannot leave the virtual model.
	send(t, c, `{"type":"session.update","session":{"type":"realtime","model":"gpt-other"}}`)
	if ev := readType(t, c, "session.updated"); ev["session"].(map[string]any)["model"] != "gpt-realtime-b" {
		t.Fatalf("session.updated = %v", ev)
	}
	send(t, c, `{"type":"response.create"}`)
	readType(t, c, "response.done")
	_ = c.Close(websocket.StatusNormalClosure, "")

	if m := up.sessions(); len(m) != 1 || m[0] != "gpt-realtime-b" {
		t.Fatalf("upstream sessions = %v", m)
	}
	// One request line per session, after it closes, with the response's
	// cost: 200 text in at 4, 800 audio in at 32, 100 text out at 16 and
	// 400 audio out at 64, per million.
	want := (200*4 + 800*32 + 100*16 + 400*64) / 1e6
	deadline := time.Now().Add(5 * time.Second)
	for {
		l := logs.lastRequest(t)
		if l != nil && l["endpoint"] == "realtime" {
			if l["status"] != 101.0 || l["target"] != "b/gpt-realtime-b" || l["fallbacks"] != 1.0 || l["input_tokens"] != 1000.0 || math.Abs(l["cost_usd"].(float64)-want) > 1e-9 {
				t.Fatalf("log = %v", l)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no realtime request log")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRealtimeBrowserKey(t *testing.T) {
	up := newRealtimeUpstream(t, 0)
	gw := httptest.NewServer(gateway(t, realtimeConfig(up.URL), nil))
	t.Cleanup(gw.Close)
	c, _, err := dialGateway(t, gw, "model=voice", &websocket.DialOptions{Subprotocols: []string{"realtime", keyProtocol + devKey}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	readType(t, c, "session.created")
	if c.Subprotocol() != "realtime" {
		t.Fatalf("subprotocol = %q", c.Subprotocol())
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	for _, p := range up.protocols {
		if strings.Contains(p, devKey) {
			t.Fatalf("the gateway key reached the upstream: %v", up.protocols)
		}
	}
}

func TestRealtimeBudgetClosesSession(t *testing.T) {
	up := newRealtimeUpstream(t, 0)
	gw := httptest.NewServer(gateway(t, realtimeConfig(up.URL), nil))
	t.Cleanup(gw.Close)
	c, _, err := dialGateway(t, gw, "model=voice", keyHeader("cheap-key-0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	send(t, c, `{"type":"response.create"}`)
	readType(t, c, "response.done")
	send(t, c, `{"type":"response.create"}`)
	// The response that spends the budget is delivered, then the session
	// ends with an error event.
	readType(t, c, "response.done")
	if ev := readType(t, c, "error"); ev["error"].(map[string]any)["code"] != "budget_exceeded" {
		t.Fatalf("error = %v", ev)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := c.Read(ctx); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("close = %v", err)
	}
	// A new session is refused while the budget is spent.
	if _, resp, err := dialGateway(t, gw, "model=voice", keyHeader("cheap-key-0123456789")); err == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("new session = %v, %v", resp, err)
	}
}

func TestRealtimeSessionLimit(t *testing.T) {
	up := newRealtimeUpstream(t, 0)
	gw := httptest.NewServer(gateway(t, realtimeConfig(up.URL)+"limits: {max_realtime_session: 200ms}\n", nil))
	t.Cleanup(gw.Close)
	c, _, err := dialGateway(t, gw, "model=voice", keyHeader(devKey))
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if ev := readType(t, c, "error"); ev["error"].(map[string]any)["code"] != "session_expired" {
		t.Fatalf("error = %v", ev)
	}
}

func TestRealtimeRefusals(t *testing.T) {
	up := newRealtimeUpstream(t, 0)
	refusing := newRealtimeUpstream(t, http.StatusBadRequest)
	gw := httptest.NewServer(gateway(t, realtimeConfig(up.URL)+"", nil))
	t.Cleanup(gw.Close)
	gw2 := httptest.NewServer(gateway(t, realtimeConfig(refusing.URL), nil))
	t.Cleanup(gw2.Close)
	for _, c := range []struct {
		name   string
		gw     *httptest.Server
		query  string
		opts   *websocket.DialOptions
		status int
	}{
		{"no key", gw, "model=voice", nil, http.StatusUnauthorized},
		{"no model", gw, "", keyHeader(devKey), http.StatusBadRequest},
		{"unknown model", gw, "model=nope", keyHeader(devKey), http.StatusNotFound},
		{"guardrails apply", gw, "model=voice", keyHeader(guardKey), http.StatusBadRequest},
		{"upstream refuses", gw2, "model=voice", keyHeader(devKey), http.StatusBadRequest},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, resp, err := dialGateway(t, c.gw, c.query, c.opts)
			if err == nil || resp == nil || resp.StatusCode != c.status {
				t.Fatalf("dial = %v, %v; want %d", resp, err, c.status)
			}
		})
	}
	if m := up.sessions(); len(m) != 0 {
		t.Fatalf("refused sessions reached the upstream: %v", m)
	}
}
