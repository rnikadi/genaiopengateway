package alerts

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
)

func TestSign(t *testing.T) {
	// HMAC-SHA256 of "1700000000.{}" with the key "secret".
	got := Sign("secret", time.Unix(1700000000, 0), []byte("{}"))
	if want := "t=1700000000,v1=b8569b78799ff9e3cbff0fc2d63a33a2b57f3282abd07c37ae5e8e7d79a5f163"; got != want {
		t.Fatalf("Sign = %s, want %s", got, want)
	}
}

func TestBodyFormats(t *testing.T) {
	e := &Event{ID: "alert_1", Event: config.EventBudgetThreshold, Org: &Ref{ID: "org_1", Name: "acme"}, Data: map[string]float64{"threshold": 0.8}, Text: "*acme* has spent $8 of its $10 monthly budget (80%)."}
	raw, _ := Body(config.FormatJSON, e)
	var j map[string]any
	_ = json.Unmarshal(raw, &j)
	if j["event"] != "budget.threshold" || j["org"].(map[string]any)["name"] != "acme" || j["text"] != nil {
		t.Fatalf("json body = %s", raw)
	}
	raw, _ = Body(config.FormatSlack, e)
	var s struct {
		Text   string `json:"text"`
		Blocks []any  `json:"blocks"`
	}
	if err := json.Unmarshal(raw, &s); err != nil || s.Text != e.Text || len(s.Blocks) != 1 {
		t.Fatalf("slack body = %s", raw)
	}
}

type receiver struct {
	mu       sync.Mutex
	statuses []int // answered in order; then 200
	got      []*http.Request
	bodies   [][]byte
	done     chan struct{}
}

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	rc.mu.Lock()
	rc.got, rc.bodies = append(rc.got, r), append(rc.bodies, b)
	status := http.StatusOK
	if len(rc.statuses) > 0 {
		status, rc.statuses = rc.statuses[0], rc.statuses[1:]
	}
	rc.mu.Unlock()
	w.WriteHeader(status)
	if rc.done != nil {
		rc.done <- struct{}{}
	}
}

func newNotifier(t *testing.T) *Notifier {
	n := New(slog.New(slog.DiscardHandler), nil, "test", nil)
	n.SetRetryDelays([]time.Duration{time.Millisecond, time.Millisecond, time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	go n.Run(ctx)
	t.Cleanup(cancel)
	return n
}

func waitFor(t *testing.T, ch chan struct{}, n int) {
	t.Helper()
	for range n {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for a delivery")
		}
	}
}

func TestDeliveryRetriesAndSigns(t *testing.T) {
	rc := &receiver{statuses: []int{500, 429}, done: make(chan struct{}, 10)}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	n := newNotifier(t)
	hook := config.Webhook{Name: "ops", URL: config.Secret(srv.URL), Secret: "s3cret", Format: config.FormatJSON}
	n.Send([]config.Webhook{hook}, &Event{ID: "alert_1", Event: "budget.threshold", Data: map[string]int{}})
	waitFor(t, rc.done, 3)

	rc.mu.Lock()
	defer rc.mu.Unlock()
	if len(rc.got) != 3 {
		t.Fatalf("attempts = %d, want 3", len(rc.got))
	}
	r, body := rc.got[2], rc.bodies[2]
	if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("User-Agent") != "GenAIOpenGateway/test" {
		t.Fatalf("headers = %v", r.Header)
	}
	sig := r.Header.Get(SignatureHeader)
	ts, _, _ := strings.Cut(strings.TrimPrefix(sig, "t="), ",")
	unix, _ := strconv.ParseInt(ts, 10, 64)
	if want := Sign("s3cret", time.Unix(unix, 0), body); sig != want {
		t.Fatalf("signature %s does not verify (want %s)", sig, want)
	}
}

func TestDeliveryGivesUp(t *testing.T) {
	rc := &receiver{statuses: []int{400}, done: make(chan struct{}, 10)}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	n := newNotifier(t)
	n.Send([]config.Webhook{{Name: "bad", URL: config.Secret(srv.URL)}}, &Event{ID: "a", Event: "key.expiring"})
	waitFor(t, rc.done, 1)
	time.Sleep(50 * time.Millisecond)
	rc.mu.Lock()
	attempts := len(rc.got)
	rc.mu.Unlock()
	if attempts != 1 {
		t.Fatalf("a 400 was retried: %d attempts", attempts)
	}
}

func TestDeliveryDoesNotFollowRedirects(t *testing.T) {
	target := &receiver{}
	inner := httptest.NewServer(target)
	defer inner.Close()
	redirect := httptest.NewServer(http.RedirectHandler(inner.URL, http.StatusFound))
	defer redirect.Close()
	n := New(slog.New(slog.DiscardHandler), nil, "test", nil)
	status, err := n.Deliver(context.Background(), config.Webhook{Name: "r", URL: config.Secret(redirect.URL)}, &Event{ID: "a"})
	if err == nil || status != http.StatusFound || len(target.got) != 0 {
		t.Fatalf("redirect: status %d, err %v, followed %d", status, err, len(target.got))
	}
}

func TestErrorsDoNotLeakTheURL(t *testing.T) {
	n := New(slog.New(slog.DiscardHandler), nil, "test", nil)
	_, err := n.Deliver(context.Background(), config.Webhook{URL: "http://127.0.0.1:1/services/T000/B000/XXXXsecret"}, &Event{ID: "a"})
	if err == nil || strings.Contains(err.Error(), "XXXXsecret") {
		t.Fatalf("err = %v", err)
	}
}
