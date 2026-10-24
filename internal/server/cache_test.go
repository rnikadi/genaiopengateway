package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/store"
)

// cacheUpstream answers chat, streamed or not, and embeddings, whose
// vectors come from vectors by input text. It counts calls by path.
type cacheUpstream struct {
	*httptest.Server
	mu    sync.Mutex
	calls map[string]int
	n     int
}

var vectors = map[string][]float32{
	"What is the capital of France?":   {1, 0, 0},
	"what's France's capital city?":    {0.99, 0.05, 0},
	"How tall is the Eiffel Tower?":    {0, 1, 0},
	"Tell me about the Eiffel Tower.":  {0.1, 0.9, 0.3},
	"Which city is France's capital?":  {0.98, 0.1, 0.05},
	"Name the capital city of France.": {0.97, 0.12, 0.05},
}

func newCacheUpstream(t *testing.T) *cacheUpstream {
	t.Helper()
	u := &cacheUpstream{calls: map[string]int{}}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream bool   `json:"stream"`
			Input  string `json:"input"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		u.mu.Lock()
		u.calls[r.URL.Path]++
		u.n++
		n := u.n
		u.mu.Unlock()
		switch {
		case r.URL.Path == "/embeddings":
			w.Header().Set("Content-Type", "application/json")
			v, _ := json.Marshal(vectors[body.Input])
			fmt.Fprintf(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":%s}],"usage":{"prompt_tokens":4,"total_tokens":4}}`, v)
		case strings.Contains(string(raw), "fail"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"no"}}`)
		case body.Stream:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"n\":%d}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n", n)
		default:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"answer-%d","choices":[{"message":{"role":"assistant","content":"Paris"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`, n)
		}
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *cacheUpstream) count(path string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls[path]
}

func chatBody(question string, extra ...string) string {
	return `{"model":"chat","messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"` + question + `"}]` + strings.Join(extra, "") + `}`
}

func cacheRequest(h http.Handler, key, body string, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCacheExact(t *testing.T) {
	up := newCacheUpstream(t)
	h := gateway(t, `
providers: {up: {base_url: "`+up.URL+`", api_key: upstream-key}}
models:
  chat: {targets: [{provider: up, model: up-chat, price: {input_per_mtok: 1000000, output_per_mtok: 1000000}}]}
cache: {max_entry_bytes: 400}
keys:
  - {name: a, key: key-a-0123456789, cache: {mode: exact, ttl: 1h}}
  - {name: b, key: key-b-0123456789, cache: {mode: exact, ttl: 1h}}
  - {name: off, key: key-off-0123456789}
`, nil)
	q := chatBody("What is the capital of France?")

	first := cacheRequest(h, "key-a-0123456789", q)
	if first.Code != 200 || first.Header().Get(cacheHeader) != "miss" {
		t.Fatalf("first = %d %q: %s", first.Code, first.Header().Get(cacheHeader), first.Body)
	}
	// The same request again, with the model, user and metadata fields
	// that do not change the answer moved or added, is a hit.
	again := cacheRequest(h, "key-a-0123456789", `{"user":"u1","messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"What is the capital of France?"}],"model":"chat"}`)
	if again.Code != 200 || again.Header().Get(cacheHeader) != "hit" || again.Body.String() != first.Body.String() ||
		again.Header().Get("X-Gateway-Target") != "up/up-chat" || again.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("again = %d %v: %s", again.Code, again.Header(), again.Body)
	}
	if n := up.count("/chat/completions"); n != 1 {
		t.Fatalf("upstream calls = %d, want 1", n)
	}

	for _, tc := range []struct {
		name, key, body, want string
		header                []string
	}{
		{"another static key", "key-b-0123456789", q, "miss", nil},
		{"another parameter", "key-a-0123456789", chatBody("What is the capital of France?", `,"temperature":0.5`), "miss", nil},
		{"no-cache", "key-a-0123456789", q, "miss", []string{"Cache-Control", "no-cache"}},
		{"no-store", "key-a-0123456789", q, "", []string{"Cache-Control", "no-store"}},
		{"cache off", "key-off-0123456789", q, "", nil},
	} {
		before := up.count("/chat/completions")
		rec := cacheRequest(h, tc.key, tc.body, tc.header...)
		if rec.Code != 200 || rec.Header().Get(cacheHeader) != tc.want || up.count("/chat/completions") != before+1 {
			t.Errorf("%s: %d %q, upstream calls %d -> %d", tc.name, rec.Code, rec.Header().Get(cacheHeader), before, up.count("/chat/completions"))
		}
	}
	// no-cache stored a fresh answer, which the next request reuses.
	if rec := cacheRequest(h, "key-a-0123456789", q); rec.Header().Get(cacheHeader) != "hit" || rec.Body.String() == first.Body.String() {
		t.Errorf("after no-cache = %q: %s", rec.Header().Get(cacheHeader), rec.Body)
	}

	// A stream replays the stored event stream.
	stream := chatBody("How tall is the Eiffel Tower?", `,"stream":true`)
	s1 := cacheRequest(h, "key-a-0123456789", stream)
	s2 := cacheRequest(h, "key-a-0123456789", stream)
	if s2.Header().Get(cacheHeader) != "hit" || s2.Header().Get("Content-Type") != "text/event-stream" || s2.Body.String() != s1.Body.String() ||
		!strings.HasSuffix(s2.Body.String(), "data: [DONE]\n\n") {
		t.Fatalf("stream replay = %v: %q", s2.Header(), s2.Body)
	}

	// Errors are not stored.
	before := up.count("/chat/completions")
	cacheRequest(h, "key-a-0123456789", chatBody("fail"))
	if rec := cacheRequest(h, "key-a-0123456789", chatBody("fail")); rec.Code != 400 || rec.Header().Get(cacheHeader) != "miss" || up.count("/chat/completions") != before+2 {
		t.Errorf("error answer cached: %d %q", rec.Code, rec.Header().Get(cacheHeader))
	}
}

func TestCacheEntryLimit(t *testing.T) {
	up := newCacheUpstream(t)
	h := gateway(t, `
providers: {up: {base_url: "`+up.URL+`", api_key: upstream-key}}
models: {chat: {targets: [{provider: up, model: up-chat}]}}
cache: {max_entry_bytes: 20}
keys: [{name: a, key: key-a-0123456789, cache: {mode: exact, ttl: 1h}}]
`, nil)
	q := chatBody("What is the capital of France?")
	cacheRequest(h, "key-a-0123456789", q)
	if rec := cacheRequest(h, "key-a-0123456789", q); rec.Header().Get(cacheHeader) != "miss" || up.count("/chat/completions") != 2 {
		t.Fatalf("an answer over max_entry_bytes was cached: %q", rec.Header().Get(cacheHeader))
	}
}

func TestCacheSemantic(t *testing.T) {
	up := newCacheUpstream(t)
	e := newAdminEnvConfig(t, `
admin: {token: `+adminToken+`}
store: {driver: sqlite, dsn: unused}
providers: {up: {base_url: "`+up.URL+`", api_key: upstream-key}}
models:
  chat: {targets: [{provider: up, model: up-chat, price: {input_per_mtok: 1000000, output_per_mtok: 1000000}}]}
  embed: {targets: [{provider: up, model: up-embed, price: {input_per_mtok: 1000000}}]}
cache: {semantic: {model: embed, threshold: 0.95}}
`)
	var org, team, other, key, otherKey idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"acme","cache":{"mode":"semantic","ttl":"10m"}}`, 201, &org)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"web"}`, 201, &team)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"ops"}`, 201, &other)
	e.call("POST", "/admin/v1/teams/"+team.ID+"/keys", `{"name":"k"}`, 201, &key)
	e.call("POST", "/admin/v1/teams/"+other.ID+"/keys", `{"name":"k"}`, 201, &otherKey)

	first := cacheRequest(e.h, key.Key, chatBody("What is the capital of France?"), "X-Request-Id", "req-first")
	if first.Header().Get(cacheHeader) != "miss" {
		t.Fatalf("first = %q", first.Header().Get(cacheHeader))
	}
	for _, tc := range []struct {
		name, key, body, want string
	}{
		{"reworded question", key.Key, chatBody("what's France's capital city?"), "semantic"},
		{"exact repeat", key.Key, chatBody("What is the capital of France?"), "hit"},
		{"different question", key.Key, chatBody("How tall is the Eiffel Tower?"), "miss"},
		{"below the threshold", key.Key, chatBody("Tell me about the Eiffel Tower."), "miss"},
		{"different context", key.Key, `{"model":"chat","messages":[{"role":"user","content":"Which city is France's capital?"}]}`, "miss"},
		{"another team", otherKey.Key, chatBody("Name the capital city of France."), "miss"},
	} {
		rec := cacheRequest(e.h, tc.key, tc.body)
		if rec.Code != 200 || rec.Header().Get(cacheHeader) != tc.want {
			t.Errorf("%s: %d %q", tc.name, rec.Code, rec.Header().Get(cacheHeader))
		}
		if tc.want == "semantic" && rec.Body.String() != first.Body.String() {
			t.Errorf("%s: served %s, want the first answer", tc.name, rec.Body)
		}
	}
	if n := up.count("/chat/completions"); n != 5 {
		t.Errorf("chat calls = %d, want 5", n)
	}

	e.flush()
	ctx := context.Background()
	rows, err := e.srv.store.QueryUsage(ctx, store.UsageQuery{TeamID: team.ID, From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour), GroupBy: "endpoint"})
	if err != nil {
		t.Fatal(err)
	}
	byEndpoint := map[string]store.UsageRow{}
	for _, r := range rows {
		byEndpoint[r.Group] = r
	}
	chat, emb := byEndpoint["chat.completions"], byEndpoint[cacheEmbeddings]
	// Six chat requests: four answered upstream at $15, two from the
	// cache saving $15 each. Five embedding calls: the exact repeat
	// needed none.
	if chat.Requests != 6 || chat.CacheHits != 2 || chat.SavedUSD != 30 || chat.CostUSD != 60 {
		t.Errorf("chat usage = %+v", chat)
	}
	if emb.Requests != 5 || emb.CostUSD != 20 {
		t.Errorf("embedding usage = %+v", emb)
	}
	reqs, _, err := e.srv.store.ListRequests(ctx, store.RequestQuery{TeamID: team.ID}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	hits := 0
	for _, r := range reqs {
		if r.CacheHit {
			hits++
			if r.CostUSD != 0 || r.InputTokens != 0 {
				t.Errorf("hit charged: %+v", r)
			}
		}
	}
	if hits != 2 {
		t.Errorf("requests marked cached = %d", hits)
	}

	// Clearing the team's entries sends the next request upstream.
	var cleared struct{ Cleared int }
	e.call("DELETE", "/admin/v1/cache?org_id="+org.ID+"&team_id="+team.ID, "", 200, &cleared)
	if cleared.Cleared != 4 {
		t.Errorf("cleared = %d, want 4", cleared.Cleared)
	}
	if rec := cacheRequest(e.h, key.Key, chatBody("What is the capital of France?")); rec.Header().Get(cacheHeader) != "miss" {
		t.Errorf("after clear = %q", rec.Header().Get(cacheHeader))
	}
	audit, _, _ := e.srv.store.ListAudit(ctx, org.ID, store.Page{})
	if len(audit) == 0 || audit[0].Action != "cache.clear" || !strings.Contains(string(audit[0].Details), `"entries":4`) {
		t.Errorf("audit = %+v", audit)
	}
	e.call("DELETE", "/admin/v1/cache?org_id="+org.ID+"&team_id="+other.ID+"x", "", 404, nil)
	e.call("DELETE", "/admin/v1/cache", "", 400, nil)
}

func TestCachePolicyChecks(t *testing.T) {
	e := newAdminEnv(t, "")
	var org idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"acme","cache":{"mode":"semantic","ttl":"1h"}}`, 400, nil)
	e.call("POST", "/admin/v1/orgs", `{"name":"acme","cache":{"mode":"exact","ttl":"9999h"}}`, 400, nil)
	e.call("POST", "/admin/v1/orgs", `{"name":"acme","cache":{"mode":"sometimes"}}`, 400, nil)
	e.call("POST", "/admin/v1/orgs", `{"name":"acme","cache":{"mode":"exact","ttl":"30m"}}`, 201, &org)
	type withCache struct {
		Cache *struct{ Mode, TTL string }
	}
	var off, cleared withCache
	e.call("PATCH", "/admin/v1/orgs/"+org.ID, `{"cache":{"mode":"off"}}`, 200, &off)
	if off.Cache == nil || off.Cache.Mode != "off" {
		t.Errorf("patched cache = %+v", off.Cache)
	}
	e.call("PATCH", "/admin/v1/orgs/"+org.ID, `{"cache":null}`, 200, &cleared)
	if cleared.Cache != nil {
		t.Errorf("cleared cache = %+v", cleared.Cache)
	}
}
