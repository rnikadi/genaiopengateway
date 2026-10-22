package limits

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Backend stores counters shared by limit checks. Values are float64 so
// one interface serves request counts, token counts and USD spend.
type Backend interface {
	// Add atomically adds delta to key and returns the new value. A
	// positive ttl sets the expiry when the key has none.
	Add(ctx context.Context, key string, delta float64, ttl time.Duration) (float64, error)
	// Get returns the values of keys; missing keys read as zero.
	Get(ctx context.Context, keys ...string) ([]float64, error)
	// Raise sets key to v if it is missing or lower, so seeding never
	// lowers a live counter.
	Raise(ctx context.Context, key string, v float64, ttl time.Duration) error
}

// Memory is a single-process backend.
type Memory struct {
	mu   sync.Mutex
	vals map[string]memEntry
	now  func() time.Time
}

type memEntry struct {
	v       float64
	expires time.Time // zero means never
}

// NewMemory returns an empty in-memory backend that drops expired keys
// in the background until ctx is done.
func NewMemory(ctx context.Context) *Memory {
	m := &Memory{vals: map[string]memEntry{}, now: time.Now}
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.sweep()
			}
		}
	}()
	return m
}

func (m *Memory) sweep() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for k, e := range m.vals {
		if e.expired(now) {
			delete(m.vals, k)
		}
	}
}

func (e memEntry) expired(now time.Time) bool { return !e.expires.IsZero() && !now.Before(e.expires) }

func (m *Memory) live(key string) (memEntry, bool) {
	e, ok := m.vals[key]
	if ok && e.expired(m.now()) {
		delete(m.vals, key)
		return memEntry{}, false
	}
	return e, ok
}

// Add implements Backend.
func (m *Memory) Add(_ context.Context, key string, delta float64, ttl time.Duration) (float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.live(key)
	if !ok && ttl > 0 {
		e.expires = m.now().Add(ttl)
	}
	e.v += delta
	m.vals[key] = e
	return e.v, nil
}

// Get implements Backend.
func (m *Memory) Get(_ context.Context, keys ...string) ([]float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]float64, len(keys))
	for i, k := range keys {
		e, _ := m.live(k)
		out[i] = e.v
	}
	return out, nil
}

// Raise implements Backend.
func (m *Memory) Raise(_ context.Context, key string, v float64, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.live(key)
	if ok && e.v >= v {
		return nil
	}
	if !ok && ttl > 0 {
		e.expires = m.now().Add(ttl)
	}
	e.v = v
	m.vals[key] = e
	return nil
}

// Valkey is a backend on Valkey or any Redis-protocol server, shared by
// every gateway replica.
type Valkey struct {
	c redis.UniversalClient
}

// NewValkey connects to addr.
func NewValkey(addr, password string) *Valkey {
	return &Valkey{c: redis.NewClient(&redis.Options{
		Addr: addr, Password: password,
		DialTimeout: 2 * time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second,
		PoolSize: 256,
	})}
}

// Close closes the connection pool.
func (v *Valkey) Close() error { return v.c.Close() }

var addScript = redis.NewScript(`
local v = redis.call('INCRBYFLOAT', KEYS[1], ARGV[1])
if tonumber(ARGV[2]) > 0 and redis.call('PTTL', KEYS[1]) < 0 then
  redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
return v`)

var raiseScript = redis.NewScript(`
local cur = redis.call('GET', KEYS[1])
if cur and tonumber(cur) >= tonumber(ARGV[1]) then return 0 end
if tonumber(ARGV[2]) > 0 then
  redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
else
  redis.call('SET', KEYS[1], ARGV[1])
end
return 1`)

// Add implements Backend.
func (v *Valkey) Add(ctx context.Context, key string, delta float64, ttl time.Duration) (float64, error) {
	s, err := addScript.Run(ctx, v.c, []string{key}, delta, ttl.Milliseconds()).Text()
	if err != nil {
		return 0, err
	}
	return strconv.ParseFloat(s, 64)
}

// Get implements Backend.
func (v *Valkey) Get(ctx context.Context, keys ...string) ([]float64, error) {
	vals, err := v.c.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	out := make([]float64, len(keys))
	for i, raw := range vals {
		if s, ok := raw.(string); ok {
			out[i], _ = strconv.ParseFloat(s, 64)
		}
	}
	return out, nil
}

// Raise implements Backend.
func (v *Valkey) Raise(ctx context.Context, key string, val float64, ttl time.Duration) error {
	return raiseScript.Run(ctx, v.c, []string{key}, val, ttl.Milliseconds()).Err()
}
