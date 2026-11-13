package stream

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/alerts"
	"github.com/rnikadi/genaiopengateway/internal/config"
)

type counts struct {
	mu sync.Mutex
	n  map[string]int
}

func (c *counts) ObserveStream(stream, result string, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n[stream+"/"+result] += n
}

func (c *counts) get(k string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[k]
}

type rec struct {
	ID string `json:"id"`
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	for range 200 {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met")
}

func TestWebhookStream(t *testing.T) {
	var mu sync.Mutex
	var lines []Envelope
	var sigs []string
	fail := 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if fail > 0 {
			fail--
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("Content-Type") != "application/x-ndjson" || r.Header.Get("X-Tenant") != "acme" {
			t.Errorf("headers %v", r.Header)
		}
		sigs = append(sigs, r.Header.Get(alerts.SignatureHeader))
		sc := bufio.NewScanner(strings.NewReader(string(body)))
		for sc.Scan() {
			var e Envelope
			if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
				t.Error(err)
			}
			lines = append(lines, e)
		}
	}))
	defer srv.Close()

	c := &counts{n: map[string]int{}}
	ctx, cancel := context.WithCancel(context.Background())
	m := New(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), http.DefaultTransport, c)
	m.SetRetryDelays([]time.Duration{time.Millisecond})
	m.Apply(map[string]config.Stream{"siem": {
		Type: config.StreamWebhook, URL: config.Secret(srv.URL), Secret: "s3cret", Headers: map[string]config.Secret{"X-Tenant": "acme"},
		Records: []string{config.RecordUsage, config.RecordAudit}, BatchSize: 2, FlushInterval: config.Duration(20 * time.Millisecond),
	}})
	Publish(m, config.RecordUsage, []rec{{"u1"}, {"u2"}, {"u3"}})
	Publish(m, config.RecordAudit, []rec{{"a1"}})
	Publish(m, config.RecordMCPCalls, []rec{{"not carried"}})
	eventually(t, func() bool { return c.get("siem/delivered") == 4 })
	cancel()
	m.Wait()

	mu.Lock()
	defer mu.Unlock()
	var got []string
	for _, e := range lines {
		var r rec
		_ = json.Unmarshal(e.Data, &r)
		got = append(got, e.Type+":"+r.ID)
	}
	if strings.Join(got, ",") != "usage:u1,usage:u2,usage:u3,audit:a1" {
		t.Fatalf("lines = %v", got)
	}
	for _, s := range sigs {
		if !strings.HasPrefix(s, "t=") || !strings.Contains(s, ",v1=") {
			t.Fatalf("signature %q", s)
		}
	}
}

func TestFileStreamAndReconfigure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	c := &counts{n: map[string]int{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := New(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), http.DefaultTransport, c)
	spec := config.Stream{Type: config.StreamFile, Path: path, Records: []string{config.RecordUsage}, BatchSize: 100, FlushInterval: config.Duration(10 * time.Millisecond)}
	m.Apply(map[string]config.Stream{"file": spec})
	Publish(m, config.RecordUsage, []rec{{"1"}, {"2"}})
	eventually(t, func() bool { return c.get("file/delivered") == 2 })
	// Removing the stream flushes what it holds and stops it.
	Publish(m, config.RecordUsage, []rec{{"3"}})
	m.Apply(nil)
	m.Wait()
	Publish(m, config.RecordUsage, []rec{{"4"}})
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "\n"); n != 3 {
		t.Fatalf("%d lines in the file, want 3:\n%s", n, b)
	}
}

func TestDeliveryGivesUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	defer srv.Close()
	c := &counts{n: map[string]int{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := New(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), http.DefaultTransport, c)
	m.SetRetryDelays([]time.Duration{time.Millisecond, time.Millisecond})
	m.Apply(map[string]config.Stream{"down": {Type: config.StreamWebhook, URL: config.Secret(srv.URL), Records: []string{config.RecordAudit}, BatchSize: 10, FlushInterval: config.Duration(5 * time.Millisecond)}})
	Publish(m, config.RecordAudit, []rec{{"a"}})
	eventually(t, func() bool { return c.get("down/dropped") == 1 })
}
