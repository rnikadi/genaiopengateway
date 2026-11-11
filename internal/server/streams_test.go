package server

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestStreams stores a stream through the admin API and checks that
// audit entries and usage records reach it.
func TestStreams(t *testing.T) {
	var mu sync.Mutex
	got := map[string]int{}
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sc := bufio.NewScanner(r.Body)
		mu.Lock()
		defer mu.Unlock()
		for sc.Scan() {
			var e struct {
				Type string         `json:"type"`
				Data map[string]any `json:"data"`
			}
			if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
				t.Error(err)
			}
			got[e.Type]++
			if e.Type == "usage" && e.Data["request_id"] == "" {
				t.Errorf("usage record without request_id: %v", e.Data)
			}
		}
	}))
	defer sink.Close()

	e := newAdminEnv(t, `secrets: {encryption_key: "`+testEncryptionKey+`"}`)
	e.call("PUT", "/admin/v1/config/streams/siem", `{"type": "webhook", "url": "`+sink.URL+`", "records": ["usage", "audit"], "flush_interval": "20ms"}`, http.StatusCreated, nil)
	// The store applies a stored object on its next refresh; this
	// replica applies it at once.
	var org struct{ ID string }
	e.call("POST", "/admin/v1/orgs", `{"name": "streamed"}`, http.StatusCreated, &org)
	var team struct{ ID string }
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name": "t"}`, http.StatusCreated, &team)
	var key struct{ Key string }
	e.call("POST", "/admin/v1/teams/"+team.ID+"/keys", `{"name": "k"}`, http.StatusCreated, &key)
	if rec := do(e.h, "POST", "/v1/chat/completions", key.Key, `{"model":"chat","messages":[]}`); rec.Code != http.StatusOK {
		t.Fatalf("chat: %d %s", rec.Code, rec.Body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		audit, usage := got["audit"], got["usage"]
		mu.Unlock()
		if audit >= 3 && usage >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stream got %d audit and %d usage records", audit, usage)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(scrape(t, e.h), `gog_stream_records_total{result="delivered",stream="siem"}`) {
		t.Error("no delivered metric")
	}
}
