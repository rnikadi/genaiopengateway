package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/coder/websocket"
)

// sonicUpstream is a fake Bedrock that answers each input event of a
// bidirectional stream with an output event and a usageEvent, and sends
// a last usageEvent when the input ends.
type sonicUpstream struct {
	*httptest.Server
	mu     sync.Mutex
	events []string
	paths  []string
}

func sonicChunk(event string) []byte {
	payload, _ := json.Marshal(map[string]string{"bytes": base64.StdEncoding.EncodeToString([]byte(event))})
	var m eventstream.Message
	m.Headers.Set(":message-type", eventstream.StringValue("event"))
	m.Headers.Set(":event-type", eventstream.StringValue("chunk"))
	m.Payload = payload
	var buf bytes.Buffer
	_ = eventstream.NewEncoder().Encode(&buf, m)
	return buf.Bytes()
}

const sonicUsage = `{"event":{"usageEvent":{"details":{"delta":{"input":{"speechTokens":800,"textTokens":200},"output":{"speechTokens":500,"textTokens":0}}},"totalTokens":1500}}}`

func newSonicUpstream(t *testing.T) *sonicUpstream {
	u := &sonicUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).EnableFullDuplex()
		u.mu.Lock()
		u.paths = append(u.paths, r.URL.EscapedPath())
		u.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer br-upstream-key" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		dec := eventstream.NewDecoder()
		for {
			m, err := dec.Decode(r.Body, nil)
			if err != nil {
				// The input ended: report the last usage.
				_, _ = w.Write(sonicChunk(sonicUsage))
				return
			}
			var c struct{ Bytes string }
			_ = json.Unmarshal(m.Payload, &c)
			ev, _ := base64.StdEncoding.DecodeString(c.Bytes)
			u.mu.Lock()
			u.events = append(u.events, string(ev))
			u.mu.Unlock()
			_, _ = w.Write(sonicChunk(`{"event":{"textOutput":{"content":"ack"}}}`))
			_, _ = w.Write(sonicChunk(sonicUsage))
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(u.Close)
	return u
}

func sonicConfig(url string) string {
	return `providers:
  br: {type: bedrock, base_url: "` + url + `", region: us-east-1, api_key: br-upstream-key}
models:
  voice: {targets: [{provider: br, model: "amazon.nova-sonic-v1:0", price: {input_per_mtok: 0.06, output_per_mtok: 0.24, audio_input_per_mtok: 3.4, audio_output_per_mtok: 13.6}}]}
  chat: {targets: [{provider: br, model: "amazon.nova-pro-v1:0"}]}
guardrails:
  no-pii: {type: pii, detect: [email], action: redact, stages: [input]}
keys:
  - {name: dev, key: ` + devKey + `}
  - {name: guarded, key: ` + guardKey + `, guardrails: [no-pii]}
`
}

func dialSonic(t *testing.T, gw *httptest.Server, model, key string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(gw.URL, "http")+"/bedrock/model/"+model+"/invoke-with-bidirectional-stream",
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + key}}})
}

func TestNovaSonicRelay(t *testing.T) {
	up := newSonicUpstream(t)
	logs := &lockedBuffer{}
	gw := httptest.NewServer(gatewayLog(t, sonicConfig(up.URL), logs))
	t.Cleanup(gw.Close)

	c, _, err := dialSonic(t, gw, "voice", devKey)
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, `{"event":{"sessionStart":{"inferenceConfiguration":{"maxTokens":1024}}}}`)
	for _, want := range []string{"textOutput", "usageEvent"} {
		if m, err := readLive(t, c); err != nil || m["event"].(map[string]any)[want] == nil {
			t.Fatalf("want %s: %v %v", want, m, err)
		}
	}
	send(t, c, `{"event":{"promptStart":{"promptName":"p"}}}`)
	for range 2 {
		if _, err := readLive(t, c); err != nil {
			t.Fatal(err)
		}
	}
	_ = c.Close(websocket.StatusNormalClosure, "")

	// Three usage events: one per input event and one at the end, each
	// 200 text in at 0.06, 800 audio in at 3.4, 500 audio out at 13.6.
	want := 3 * (200*0.06 + 800*3.4 + 500*13.6) / 1e6
	deadline := time.Now().Add(5 * time.Second)
	for {
		l := logs.lastRequest(t)
		if l != nil && l["endpoint"] == "bidi_stream" {
			if l["status"] != 101.0 || l["model"] != "voice" || l["input_tokens"] != 3000.0 || math.Abs(l["cost_usd"].(float64)-want) > 1e-12 {
				t.Fatalf("log = %v", l)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no request log")
		}
		time.Sleep(10 * time.Millisecond)
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.events) != 2 || !strings.Contains(up.events[0], "sessionStart") || up.paths[0] != "/model/amazon.nova-sonic-v1%3A0/invoke-with-bidirectional-stream" {
		t.Fatalf("upstream saw %v at %v", up.events, up.paths)
	}
}

func TestNovaSonicRefusals(t *testing.T) {
	up := newSonicUpstream(t)
	gw := httptest.NewServer(gateway(t, sonicConfig(up.URL), nil))
	t.Cleanup(gw.Close)

	for _, tc := range []struct {
		model, key string
		status     int
	}{
		{"voice", "wrong-key", http.StatusUnauthorized},
		{"voice", guardKey, http.StatusBadRequest},
		{"chat", devKey, http.StatusNotFound},
		{"missing", devKey, http.StatusNotFound},
	} {
		if _, resp, err := dialSonic(t, gw, tc.model, tc.key); err == nil || resp == nil || resp.StatusCode != tc.status {
			t.Errorf("%s with %s: %v, want %d", tc.model, tc.key, err, tc.status)
		}
	}
	c, _, err := dialSonic(t, gw, "voice", devKey)
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, `not json`)
	_, err = readLive(t, c)
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != websocket.StatusPolicyViolation {
		t.Errorf("a first message that is not an event: %v", err)
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.paths) != 0 {
		t.Errorf("refused sessions reached the upstream: %v", up.paths)
	}
}
