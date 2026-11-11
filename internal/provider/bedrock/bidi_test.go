package bedrock

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/provider"
)

// chunkFrame encodes one event as a bidirectional stream chunk.
func chunkFrame(t *testing.T, event string) []byte {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{"bytes": base64.StdEncoding.EncodeToString([]byte(event))})
	var m eventstream.Message
	m.Headers.Set(":message-type", eventstream.StringValue("event"))
	m.Headers.Set(":event-type", eventstream.StringValue("chunk"))
	m.Payload = payload
	var buf bytes.Buffer
	if err := eventstream.NewEncoder().Encode(&buf, m); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// chunkEvent returns the event a chunk message carries.
func chunkEvent(m eventstream.Message) string {
	var c struct{ Bytes string }
	_ = json.Unmarshal(m.Payload, &c)
	b, _ := base64.StdEncoding.DecodeString(c.Bytes)
	return string(b)
}

// h2Server serves HTTP/2 over TLS, as Bedrock does, so requests are
// full duplex.
func h2Server(h http.Handler) *httptest.Server {
	srv := httptest.NewUnstartedServer(h)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	return srv
}

func TestNovaSonicStream(t *testing.T) {
	for _, mode := range []string{"sigv4", "api key"} {
		t.Run(mode, func(t *testing.T) {
			var mu sync.Mutex
			var got []string
			ended := false
			badSignature := ""
			srv := h2Server(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.ProtoMajor != 2 || r.URL.EscapedPath() != "/model/amazon.nova-sonic-v1%3A0/invoke-with-bidirectional-stream" {
					w.WriteHeader(404)
					return
				}
				var ss *v4.StreamSigner
				if mode == "sigv4" {
					if r.Header.Get("X-Amz-Content-Sha256") != streamingEvents {
						w.WriteHeader(403)
						return
					}
					_, sig, _ := strings.Cut(r.Header.Get("Authorization"), "Signature=")
					seed, _ := hex.DecodeString(sig)
					c, _ := staticCreds.Retrieve(r.Context())
					ss = v4.NewStreamSigner(c, "bedrock", "us-east-1", seed)
				} else if r.Header.Get("Authorization") != "Bearer br-key" {
					w.WriteHeader(403)
					return
				}
				w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				dec := eventstream.NewDecoder()
				for {
					m, err := dec.Decode(r.Body, nil)
					if err != nil {
						return
					}
					if ss != nil {
						// Check the envelope's signature chain, then open it.
						date := m.Headers.Get(":date")
						var h eventstream.Headers
						h.Set(":date", date)
						var hb bytes.Buffer
						_ = eventstream.EncodeHeaders(&hb, h)
						want, _ := ss.GetSignature(r.Context(), hb.Bytes(), m.Payload, date.Get().(time.Time))
						if sig, _ := m.Headers.Get(":chunk-signature").Get().([]byte); !bytes.Equal(sig, want) {
							mu.Lock()
							badSignature = "message signature mismatch"
							mu.Unlock()
						}
						if len(m.Payload) == 0 {
							mu.Lock()
							ended = true
							mu.Unlock()
							_, _ = w.Write(chunkFrame(t, `{"event":{"completionEnd":{}}}`))
							w.(http.Flusher).Flush()
							return
						}
						if m, err = dec.Decode(bytes.NewReader(m.Payload), nil); err != nil {
							return
						}
					}
					ev := chunkEvent(m)
					mu.Lock()
					got = append(got, ev)
					mu.Unlock()
					_, _ = w.Write(chunkFrame(t, `{"event":{"textOutput":{"content":"ack"}}}`))
					w.(http.Flusher).Flush()
				}
			}))
			defer srv.Close()
			cfg := config.Provider{Type: config.ProviderBedrock, BaseURL: srv.URL, Region: "us-east-1", Timeout: config.Duration(5 * time.Second)}
			if mode == "api key" {
				cfg.APIKey = "br-key"
			}
			p := newWithCredentials(cfg, srv.Client(), staticCreds)
			if !p.SupportsModel(provider.BidiStream, "amazon.nova-sonic-v1:0") || p.SupportsModel(provider.ChatCompletions, "amazon.nova-sonic-v1:0") {
				t.Fatal("SupportsModel")
			}
			s, status, err := p.OpenBidi(context.Background(), "amazon.nova-sonic-v1:0", []byte(`{"event":{"sessionStart":{}}}`))
			if err != nil || status != 200 {
				t.Fatal(status, err)
			}
			defer s.Close()
			for _, ev := range []string{"", `{"event":{"promptStart":{}}}`} {
				if ev != "" {
					if err := s.Send([]byte(ev)); err != nil {
						t.Fatal(err)
					}
				}
				out, err := s.Recv()
				if err != nil || string(out) != `{"event":{"textOutput":{"content":"ack"}}}` {
					t.Fatalf("recv %q %v", out, err)
				}
			}
			if err := s.CloseSend(); err != nil {
				t.Fatal(err)
			}
			if mode == "sigv4" {
				if out, err := s.Recv(); err != nil || !strings.Contains(string(out), "completionEnd") {
					t.Fatalf("after the end: %q %v", out, err)
				}
			}
			if _, err := s.Recv(); !errors.Is(err, io.EOF) {
				t.Fatalf("end: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if strings.Join(got, "|") != `{"event":{"sessionStart":{}}}|{"event":{"promptStart":{}}}` || badSignature != "" || ended != (mode == "sigv4") {
				t.Fatalf("upstream saw %v, %q, ended %v", got, badSignature, ended)
			}
		})
	}
}

func TestNovaSonicRefused(t *testing.T) {
	srv := h2Server(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Amzn-Errortype", "ThrottlingException:http://internal")
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"message":"slow down"}`))
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()
	cfg := config.Provider{Type: config.ProviderBedrock, BaseURL: srv.URL, Region: "us-east-1", APIKey: "k", Timeout: config.Duration(5 * time.Second)}
	_, status, err := newWithCredentials(cfg, srv.Client(), staticCreds).OpenBidi(context.Background(), "amazon.nova-sonic-v1:0", []byte(`{}`))
	if status != http.StatusTooManyRequests || err == nil || !strings.Contains(err.Error(), "slow down") {
		t.Fatal(status, err)
	}
}
