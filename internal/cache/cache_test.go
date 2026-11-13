package cache

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func stores(t *testing.T) (map[string]Store, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { c.Close() })
	out := map[string]Store{"memory": NewMemory(1 << 20), "miniredis": NewValkey(c)}
	if addr := os.Getenv("GOG_TEST_VALKEY_ADDR"); addr != "" {
		real := redis.NewClient(&redis.Options{Addr: addr})
		t.Cleanup(func() { real.Close() })
		out["valkey"] = NewValkey(real)
	}
	return out, mr
}

func TestStores(t *testing.T) {
	all, _ := stores(t)
	for name, s := range all {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			a, b := Prefix("org", "a")+name+"1", Prefix("org", "b")+name+"2"
			if e, err := s.Get(ctx, a); e != nil || err != nil {
				t.Fatalf("missing = %v, %v", e, err)
			}
			for _, k := range []string{a, b} {
				if err := s.Set(ctx, k, &Entry{ContentType: "application/json", Provider: "up", Model: "m", CostUSD: 0.5, Body: []byte(`{"x":1}`)}, time.Hour); err != nil {
					t.Fatal(err)
				}
			}
			e, err := s.Get(ctx, a)
			if err != nil || e == nil || string(e.Body) != `{"x":1}` || e.CostUSD != 0.5 || e.Provider != "up" {
				t.Fatalf("get = %+v, %v", e, err)
			}
			// Clearing a team leaves the others.
			if n, err := s.Clear(ctx, Prefix("org", "a")); err != nil || n < 1 {
				t.Fatalf("clear = %d, %v", n, err)
			}
			if e, _ := s.Get(ctx, a); e != nil {
				t.Fatal("cleared entry is still there")
			}
			if e, _ := s.Get(ctx, b); e == nil {
				t.Fatal("another team's entry was cleared")
			}
			if _, err := s.Clear(ctx, Prefix("org", "")); err != nil {
				t.Fatal(err)
			}
			if e, _ := s.Get(ctx, b); e != nil {
				t.Fatal("clearing the organization left an entry")
			}
		})
	}
}

func TestValkeyTTL(t *testing.T) {
	all, mr := stores(t)
	s := all["miniredis"]
	ctx := context.Background()
	_ = s.Set(ctx, "gog:cache:o:t:k", &Entry{Body: []byte("x")}, time.Minute)
	mr.FastForward(2 * time.Minute)
	if e, _ := s.Get(ctx, "gog:cache:o:t:k"); e != nil {
		t.Fatal("entry outlived its ttl")
	}
}

func TestMemoryBoundsAndTTL(t *testing.T) {
	m := NewMemory(400)
	now := time.Now()
	m.now = func() time.Time { return now }
	ctx := context.Background()
	big := make([]byte, 100)
	for _, k := range []string{"a", "b"} {
		_ = m.Set(ctx, k, &Entry{Body: big}, time.Minute)
	}
	_, _ = m.Get(ctx, "a") // a is now the most recent
	_ = m.Set(ctx, "c", &Entry{Body: big}, time.Minute)
	if e, _ := m.Get(ctx, "b"); e != nil {
		t.Fatal("least recently used entry was kept")
	}
	if e, _ := m.Get(ctx, "a"); e == nil {
		t.Fatal("recently used entry was dropped")
	}
	_ = m.Set(ctx, "huge", &Entry{Body: make([]byte, 400)}, time.Minute)
	if e, _ := m.Get(ctx, "huge"); e != nil {
		t.Fatal("entry larger than the store was kept")
	}
	now = now.Add(2 * time.Minute)
	if e, _ := m.Get(ctx, "a"); e != nil {
		t.Fatal("expired entry was returned")
	}
}

func TestKey(t *testing.T) {
	k1, _ := Key("o", "t", []byte(`{"model":"a","temperature":0,"messages":[{"role":"user","content":"hi"}],"user":"u1"}`), "chat")
	k2, _ := Key("o", "t", []byte(`{"messages":[{"content":"hi","role":"user"}],"temperature":0,"model":"b","metadata":{"x":1}}`), "chat")
	k3, _ := Key("o", "t", []byte(`{"messages":[{"content":"hi","role":"user"}],"temperature":0.0}`), "chat")
	k4, _ := Key("o", "other", []byte(`{"messages":[{"content":"hi","role":"user"}],"temperature":0}`), "chat")
	k5, _ := Key("o", "t", []byte(`{"messages":[{"content":"hi","role":"user"}],"temperature":0}`), "messages")
	if k1 != k2 {
		t.Fatal("model, user, metadata and key order changed the key")
	}
	if k1 == k3 || k1 == k4 || k1 == k5 {
		t.Fatal("different requests share a key")
	}
	if _, err := Key("o", "t", []byte(`[1]`)); err == nil {
		t.Fatal("a non-object body has a key")
	}
}

func TestIndex(t *testing.T) {
	x := NewIndex(2)
	x.Add("ctx", "k1", []float32{1, 0, 0}, time.Hour)
	x.Add("ctx", "k2", []float32{0, 1, 0}, time.Hour)
	if k, score, ok := x.Nearest("ctx", []float32{0.99, 0.1, 0}, 0.95); !ok || k != "k1" || score < 0.95 {
		t.Fatalf("nearest = %s %v %v", k, score, ok)
	}
	if _, _, ok := x.Nearest("ctx", []float32{0.7, 0.7, 0}, 0.95); ok {
		t.Fatal("a match below the threshold")
	}
	if _, _, ok := x.Nearest("other", []float32{1, 0, 0}, 0.5); ok {
		t.Fatal("a match in another context")
	}
	x.Add("ctx", "k3", []float32{0, 0, 1}, time.Hour)
	if _, _, ok := x.Nearest("ctx", []float32{1, 0, 0}, 0.95); ok {
		t.Fatal("the oldest vector was kept past the bound")
	}
	x.Clear("c")
	if _, _, ok := x.Nearest("ctx", []float32{0, 0, 1}, 0.5); ok {
		t.Fatal("cleared vectors still match")
	}
}

func TestQuestion(t *testing.T) {
	q, rest, ok := Question([]byte(`{"model":"m","messages":[{"role":"system","content":"be brief"},{"role":"user","content":[{"type":"text","text":"what is a falcon?"}]}]}`))
	if !ok || q != "what is a falcon?" || string(rest) != `{"messages":[{"role":"system","content":"be brief"}],"model":"m"}` {
		t.Fatalf("question = %q %s %v", q, rest, ok)
	}
	for _, body := range []string{
		`{"messages":[{"role":"assistant","content":"hi"}]}`,
		`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`,
		`{"messages":[]}`,
		`{"input":"x"}`,
	} {
		if _, _, ok := Question([]byte(body)); ok {
			t.Fatalf("question in %s", body)
		}
	}
}
