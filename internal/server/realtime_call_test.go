package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/rnikadi/genaiopengateway/internal/config"
)

// callUpstream is a fake OpenAI that makes realtime calls over WebRTC:
// POST /realtime/calls answers an offer, GET /realtime?call_id= is the
// call's events, and POST /realtime/calls/{id}/hangup ends it.
type callUpstream struct {
	*httptest.Server
	mu       sync.Mutex
	sessions []map[string]any // the session of each call made
	offers   []string
	hungUp   []string
	events   chan string // events the call sends, in order
	noEvents bool        // refuse the events WebSocket
	noID     bool        // name no call id
}

func newCallUpstream(t *testing.T) *callUpstream {
	t.Helper()
	u := &callUpstream{events: make(chan string, 16)}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer upstream-key" {
			http.Error(w, "bad key", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/realtime/calls":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			var sess map[string]any
			_ = json.Unmarshal([]byte(r.FormValue("session")), &sess)
			u.mu.Lock()
			u.sessions = append(u.sessions, sess)
			u.offers = append(u.offers, r.FormValue("sdp"))
			u.mu.Unlock()
			if !u.noID {
				w.Header().Set("Location", "/v1/realtime/calls/rtc_123")
			}
			w.Header().Set("Content-Type", "application/sdp")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "v=0 answer")
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/hangup"):
			u.mu.Lock()
			u.hungUp = append(u.hungUp, strings.Split(r.URL.Path, "/")[3])
			u.mu.Unlock()
			close(u.events)
		case r.URL.Path == "/realtime" && r.URL.Query().Get("call_id") == "rtc_123" && !u.noEvents:
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer c.CloseNow()
			_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"session.created","session":{"model":"gpt-realtime-a-2026-01-01"}}`))
			for ev := range u.events {
				if c.Write(r.Context(), websocket.MessageText, []byte(ev)) != nil {
					return
				}
			}
			_ = c.Close(websocket.StatusNormalClosure, "")
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *callUpstream) hangups() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.hungUp...)
}

const callDone = `{"type":"response.done","response":{"status":"completed","usage":{"total_tokens":1500,"input_tokens":1000,"output_tokens":500,"input_token_details":{"cached_tokens":0,"text_tokens":200,"audio_tokens":800},"output_token_details":{"text_tokens":100,"audio_tokens":400}}}}`

// offerSDP posts an offer as application/sdp.
func offerSDP(t *testing.T, gw *httptest.Server, key, query string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/realtime/calls?"+query, strings.NewReader("v=0 offer"))
	req.Header.Set("Content-Type", "application/sdp")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func TestRealtimeCall(t *testing.T) {
	up := newCallUpstream(t)
	logs := &lockedBuffer{}
	gw := httptest.NewServer(gatewayLog(t, realtimeConfig(up.URL), logs))
	t.Cleanup(gw.Close)

	// The offer as a form, with a session that names the virtual model.
	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	_ = mw.WriteField("sdp", "v=0 offer")
	_ = mw.WriteField("session", `{"type":"realtime","model":"voice","instructions":"Be brief."}`)
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/realtime/calls", &form)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+devKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	answer, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" || !strings.Contains(resp.Header.Get("Access-Control-Expose-Headers"), "Location") {
		t.Fatalf("a page of another origin could not read the answer: %v", resp.Header)
	}
	// A browser asks first, without a key.
	pre, _ := http.NewRequest(http.MethodOptions, gw.URL+"/v1/realtime/calls", nil)
	pre.Header.Set("Origin", "https://app.example")
	pre.Header.Set("Access-Control-Request-Method", "POST")
	if presp, err := http.DefaultClient.Do(pre); err != nil || presp.StatusCode != http.StatusNoContent || !strings.Contains(presp.Header.Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Fatalf("preflight: %v %v", presp, err)
	}
	if resp.StatusCode != http.StatusCreated || string(answer) != "v=0 answer" || resp.Header.Get("Location") != "/v1/realtime/calls/rtc_123" || resp.Header.Get("Content-Type") != "application/sdp" {
		t.Fatalf("call: %d %q %v", resp.StatusCode, answer, resp.Header)
	}
	up.mu.Lock()
	sess, offer := up.sessions[0], up.offers[0]
	up.mu.Unlock()
	if sess["model"] != "gpt-realtime-a" || sess["instructions"] != "Be brief." || offer != "v=0 offer" {
		t.Fatalf("the provider got session %v and offer %q", sess, offer)
	}

	// Another key cannot hang the call up; a response is charged; then
	// the call ends by itself and is recorded once, with its cost.
	hang := func(key string) int {
		req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/realtime/calls/rtc_123/hangup", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if code := hang(guardKey); code != http.StatusNotFound {
		t.Fatalf("hangup with another key: %d", code)
	}
	up.events <- callDone
	// A dated name of the same model is not a change of model.
	up.events <- `{"type":"session.updated","session":{"model":"gpt-realtime-a-2026-01-01"}}`
	close(up.events)
	want := (200*4 + 800*32 + 100*16 + 400*64) / 1e6
	var line map[string]any
	waitFor(t, "the call's request line", func() bool {
		line = logs.lastRequest(t)
		return line != nil && line["endpoint"] == "realtime"
	})
	if cost, _ := line["cost_usd"].(float64); cost < want*0.999 || cost > want*1.001 || line["status"] != 201.0 || line["target"] != "a/gpt-realtime-a" || line["error_code"] != "" {
		t.Fatalf("request line: %v, want cost %v", line, want)
	}
	if h := up.hangups(); len(h) != 0 {
		t.Fatalf("a call that ended by itself was hung up: %v", h)
	}
	if code := hang(devKey); code != http.StatusNotFound {
		t.Fatalf("hangup after the call ended: %d", code)
	}
}

// The gateway hangs up when a budget is spent, when the caller asks,
// when the model changes and when the session is too long.
func TestRealtimeCallEndedByGateway(t *testing.T) {
	for name, tc := range map[string]struct {
		key, limits string
		act         func(t *testing.T, up *callUpstream, gw *httptest.Server)
		want        string
	}{
		"budget": {key: "cheap-key-0123456789", want: "budget_exceeded", act: func(t *testing.T, up *callUpstream, _ *httptest.Server) {
			// Each response costs about 0.054, so the second spends 0.08.
			up.events <- callDone
			up.events <- callDone
		}},
		"caller": {key: devKey, want: "", act: func(t *testing.T, _ *callUpstream, gw *httptest.Server) {
			req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/realtime/calls/rtc_123/hangup", nil)
			req.Header.Set("Authorization", "Bearer "+devKey)
			resp, err := http.DefaultClient.Do(req)
			if err != nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("hangup: %v %v", resp, err)
			}
			_ = resp.Body.Close()
		}},
		"model": {key: devKey, want: "model_changed", act: func(t *testing.T, up *callUpstream, _ *httptest.Server) {
			up.events <- `{"type":"session.updated","session":{"model":"gpt-costly"}}`
		}},
		"length": {key: devKey, limits: "limits: {max_realtime_session: 100ms}\n", want: "session_expired", act: func(*testing.T, *callUpstream, *httptest.Server) {}},
	} {
		t.Run(name, func(t *testing.T) {
			up := newCallUpstream(t)
			logs := &lockedBuffer{}
			gw := httptest.NewServer(gatewayLog(t, tc.limits+realtimeConfig(up.URL), logs))
			t.Cleanup(gw.Close)
			if resp := offerSDP(t, gw, tc.key, "model=voice"); resp.StatusCode != http.StatusCreated {
				t.Fatalf("call: %d", resp.StatusCode)
			}
			tc.act(t, up, gw)
			waitFor(t, "the hangup", func() bool { return len(up.hangups()) == 1 })
			if h := up.hangups(); h[0] != "rtc_123" {
				t.Fatalf("hung up %v", h)
			}
			var line map[string]any
			waitFor(t, "the call's request line", func() bool {
				line = logs.lastRequest(t)
				return line != nil && line["endpoint"] == "realtime"
			})
			if line["error_code"] != tc.want || line["status"] != 201.0 {
				t.Fatalf("request line: %v, want error code %q", line, tc.want)
			}
		})
	}
}

func TestRealtimeCallRefused(t *testing.T) {
	up := newCallUpstream(t)
	gw := httptest.NewServer(gatewayLog(t, realtimeConfig(up.URL), io.Discard))
	t.Cleanup(gw.Close)
	for name, tc := range map[string]struct {
		key, query string
		want       int
	}{
		"no key":        {"", "model=voice", http.StatusUnauthorized},
		"no model":      {devKey, "", http.StatusBadRequest},
		"unknown model": {devKey, "model=nope", http.StatusNotFound},
		"guardrails":    {guardKey, "model=voice", http.StatusBadRequest},
	} {
		if resp := offerSDP(t, gw, tc.key, tc.query); resp.StatusCode != tc.want {
			t.Errorf("%s: %d, want %d", name, resp.StatusCode, tc.want)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/realtime/calls?model=voice", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+devKey)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a JSON body: %v %v", resp, err)
	}
	up.mu.Lock()
	made := len(up.sessions)
	up.mu.Unlock()
	if made != 0 {
		t.Fatalf("%d refused calls reached the provider", made)
	}

	// A call the gateway cannot watch is ended and not handed over.
	up.noEvents = true
	if resp := offerSDP(t, gw, devKey, "model=voice"); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("call without events: %d", resp.StatusCode)
	}
	if h := up.hangups(); len(h) != 1 {
		t.Fatalf("hangups after a call without events: %v", h)
	}
}

// Shutting down ends the calls, which no one would meter afterwards.
func TestRealtimeCallShutdown(t *testing.T) {
	up := newCallUpstream(t)
	cfg, err := config.Parse([]byte(realtimeConfig(up.URL)))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(s.Handler())
	t.Cleanup(gw.Close)
	if resp := offerSDP(t, gw, devKey, "model=voice"); resp.StatusCode != http.StatusCreated {
		t.Fatalf("call: %d", resp.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.Close(ctx)
	if h := up.hangups(); len(h) != 1 {
		t.Fatalf("hangups after shutdown: %v", h)
	}
}
