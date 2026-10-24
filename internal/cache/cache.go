// Package cache stores model answers for reuse (docs/cache.md): exact
// matches in memory or Valkey, and an in-memory index of question
// embeddings for semantic matches.
package cache

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Entry is a cached answer.
type Entry struct {
	ContentType string `json:"content_type"`
	// Provider and Model name the target that produced the answer.
	Provider string  `json:"provider"`
	Model    string  `json:"model"`
	CostUSD  float64 `json:"cost_usd"`
	Body     []byte  `json:"body"`
}

func (e *Entry) size() int64 {
	return int64(len(e.Body) + len(e.ContentType) + len(e.Provider) + len(e.Model) + 64)
}

// Store holds entries by key.
type Store interface {
	// Get returns an entry, or nil when there is none.
	Get(ctx context.Context, key string) (*Entry, error)
	Set(ctx context.Context, key string, e *Entry, ttl time.Duration) error
	// Clear removes the entries whose keys start with prefix and reports
	// how many there were.
	Clear(ctx context.Context, prefix string) (int, error)
}

// Prefix is the key prefix of an organization's entries, or of one
// team's when team is set.
func Prefix(org, team string) string {
	p := "gog:cache:" + org + ":"
	if team != "" {
		p += team + ":"
	}
	return p
}

// Key is an entry's key: the team's prefix and a digest of the parts
// and the canonical body.
func Key(org, team string, body []byte, parts ...string) (string, error) {
	canon, err := Canonical(body)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	h.Write(canon)
	return Prefix(org, team) + hex.EncodeToString(h.Sum(nil)), nil
}

// Canonical is a JSON object body with sorted keys and without the
// fields that do not change the answer: model, user and metadata.
func Canonical(body []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("body is not a JSON object")
	}
	delete(m, "model")
	delete(m, "user")
	delete(m, "metadata")
	return json.Marshal(m)
}

// Memory is a store in this process, least recently used first out.
type Memory struct {
	mu    sync.Mutex
	max   int64
	used  int64
	order *list.List // of *memItem, most recent first
	items map[string]*list.Element
	now   func() time.Time
}

type memItem struct {
	key     string
	entry   *Entry
	expires time.Time
}

// NewMemory returns a store holding up to maxBytes.
func NewMemory(maxBytes int64) *Memory {
	return &Memory{max: maxBytes, order: list.New(), items: map[string]*list.Element{}, now: time.Now}
}

// Get implements Store.
func (m *Memory) Get(_ context.Context, key string) (*Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	el, ok := m.items[key]
	if !ok {
		return nil, nil
	}
	it := el.Value.(*memItem)
	if !m.now().Before(it.expires) {
		m.remove(el)
		return nil, nil
	}
	m.order.MoveToFront(el)
	return it.entry, nil
}

// Set implements Store.
func (m *Memory) Set(_ context.Context, key string, e *Entry, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.size() > m.max {
		return nil
	}
	if el, ok := m.items[key]; ok {
		m.remove(el)
	}
	m.items[key] = m.order.PushFront(&memItem{key: key, entry: e, expires: m.now().Add(ttl)})
	m.used += e.size()
	for m.used > m.max {
		m.remove(m.order.Back())
	}
	return nil
}

// Clear implements Store.
func (m *Memory) Clear(_ context.Context, prefix string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for key, el := range m.items {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			m.remove(el)
			n++
		}
	}
	return n, nil
}

func (m *Memory) remove(el *list.Element) {
	it := el.Value.(*memItem)
	m.order.Remove(el)
	delete(m.items, it.key)
	m.used -= it.entry.size()
}

// Valkey is a store shared by every replica.
type Valkey struct {
	c redis.UniversalClient
}

// NewValkey stores entries through c.
func NewValkey(c redis.UniversalClient) *Valkey { return &Valkey{c: c} }

// Get implements Store.
func (v *Valkey) Get(ctx context.Context, key string) (*Entry, error) {
	raw, err := v.c.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var e Entry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// Set implements Store.
func (v *Valkey) Set(ctx context.Context, key string, e *Entry, ttl time.Duration) error {
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return v.c.Set(ctx, key, raw, ttl).Err()
}

// Clear implements Store.
func (v *Valkey) Clear(ctx context.Context, prefix string) (int, error) {
	n := 0
	iter := v.c.Scan(ctx, 0, prefix+"*", 500).Iterator()
	var batch []string
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		deleted, err := v.c.Unlink(ctx, batch...).Result()
		n += int(deleted)
		batch = batch[:0]
		return err
	}
	for iter.Next(ctx) {
		batch = append(batch, iter.Val())
		if len(batch) == 500 {
			if err := flush(); err != nil {
				return n, err
			}
		}
	}
	if err := iter.Err(); err != nil {
		return n, err
	}
	return n, flush()
}
