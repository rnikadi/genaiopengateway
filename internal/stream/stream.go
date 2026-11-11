// Package stream copies records the gateway writes (usage, audit, MCP
// calls, guardrail events) to outside systems as they are written: to a
// webhook as signed NDJSON batches, or to a JSON Lines file for a log
// shipper (docs/streams.md). Delivery is best effort with retries; the
// database stays the record of truth.
package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/alerts"
	"github.com/rnikadi/genaiopengateway/internal/config"
)

// queueSize bounds the records waiting for one stream.
const queueSize = 50_000

// Envelope is one line of a stream: the record type, when the gateway
// sent it, and the record as the admin API shows it.
type Envelope struct {
	Type string          `json:"type"`
	Time time.Time       `json:"time"`
	Data json.RawMessage `json:"data"`
}

// Metrics receives what happened to records.
type Metrics interface {
	ObserveStream(stream, result string, n int)
}

// Manager runs the configured streams. Apply swaps in a new set; a
// stream whose spec did not change keeps its queue.
type Manager struct {
	log     *slog.Logger
	client  *http.Client
	metrics Metrics

	mu      sync.Mutex
	streams map[string]*sink
	ctx     context.Context
	wg      sync.WaitGroup
	// delays between delivery tries; tests shorten them.
	delays []time.Duration
}

// New returns a manager that sends with transport. It stops its streams
// when ctx ends, after a last flush.
func New(ctx context.Context, log *slog.Logger, transport http.RoundTripper, m Metrics) *Manager {
	return &Manager{
		log: log, metrics: m, ctx: ctx, streams: map[string]*sink{},
		client: &http.Client{Transport: transport, Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		delays: []time.Duration{time.Second, 5 * time.Second, 30 * time.Second},
	}
}

// SetRetryDelays replaces the waits between delivery tries.
func (m *Manager) SetRetryDelays(d []time.Duration) { m.delays = d }

// Apply makes the running streams match specs.
func (m *Manager) Apply(specs map[string]config.Stream) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, s := range m.streams {
		if spec, ok := specs[name]; !ok || !reflect.DeepEqual(spec, s.spec) {
			s.stop()
			delete(m.streams, name)
		}
	}
	for name, spec := range specs {
		if _, ok := m.streams[name]; ok {
			continue
		}
		s := &sink{m: m, name: name, spec: spec, queue: make(chan Envelope, queueSize), done: make(chan struct{})}
		m.streams[name] = s
		m.wg.Add(1)
		go s.run()
	}
}

// Wait blocks until every stream has flushed after its context ended.
func (m *Manager) Wait() { m.wg.Wait() }

// Publish queues records of one type for every stream that carries it.
// It never blocks: a full queue drops the records and counts them.
func Publish[T any](m *Manager, recordType string, records []T) {
	if m == nil || len(records) == 0 {
		return
	}
	m.mu.Lock()
	var to []*sink
	for _, s := range m.streams {
		if slices.Contains(s.spec.Records, recordType) {
			to = append(to, s)
		}
	}
	m.mu.Unlock()
	if len(to) == 0 {
		return
	}
	now := time.Now().UTC()
	envs := make([]Envelope, 0, len(records))
	for _, r := range records {
		data, err := json.Marshal(r)
		if err != nil {
			continue
		}
		envs = append(envs, Envelope{Type: recordType, Time: now, Data: data})
	}
	for _, s := range to {
		dropped := 0
		for _, e := range envs {
			select {
			case s.queue <- e:
			default:
				dropped++
			}
		}
		if dropped > 0 && m.metrics != nil {
			m.metrics.ObserveStream(s.name, "dropped", dropped)
		}
	}
}

type sink struct {
	m     *Manager
	name  string
	spec  config.Stream
	queue chan Envelope
	done  chan struct{}
	once  sync.Once
}

func (s *sink) stop() { s.once.Do(func() { close(s.done) }) }

func (s *sink) run() {
	defer s.m.wg.Done()
	t := time.NewTicker(s.spec.FlushInterval.Std())
	defer t.Stop()
	batch := make([]Envelope, 0, s.spec.BatchSize)
	flush := func() {
		if len(batch) > 0 {
			s.deliver(batch)
			batch = batch[:0]
		}
	}
	for {
		select {
		case e := <-s.queue:
			batch = append(batch, e)
			if len(batch) >= s.spec.BatchSize {
				flush()
			}
		case <-t.C:
			flush()
		case <-s.done:
			s.drain(&batch, flush)
			return
		case <-s.m.ctx.Done():
			s.drain(&batch, flush)
			return
		}
	}
}

// drain sends what is queued when the stream stops.
func (s *sink) drain(batch *[]Envelope, flush func()) {
	for {
		select {
		case e := <-s.queue:
			*batch = append(*batch, e)
			if len(*batch) >= s.spec.BatchSize {
				flush()
			}
		default:
			flush()
			return
		}
	}
}

func (s *sink) deliver(batch []Envelope) {
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for _, e := range batch {
		_ = enc.Encode(e)
	}
	var err error
	for try := 0; ; try++ {
		if err = s.send(body.Bytes()); err == nil {
			s.observe("delivered", len(batch))
			return
		}
		if try >= len(s.m.delays) || s.stopping() {
			break
		}
		select {
		case <-time.After(s.m.delays[try]):
		case <-s.done:
		case <-s.m.ctx.Done():
		}
	}
	s.m.log.Warn("stream delivery failed; records dropped", "stream", s.name, "records", len(batch), "err", err)
	s.observe("dropped", len(batch))
}

func (s *sink) stopping() bool {
	select {
	case <-s.done:
		return true
	case <-s.m.ctx.Done():
		return true
	default:
		return false
	}
}

func (s *sink) observe(result string, n int) {
	if s.m.metrics != nil {
		s.m.metrics.ObserveStream(s.name, result, n)
	}
}

func (s *sink) send(body []byte) error {
	switch s.spec.Type {
	case config.StreamFile:
		f, err := os.OpenFile(s.spec.Path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return err
		}
		_, err = f.Write(body)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return err
	case config.StreamWebhook:
		// A fresh context: a stopping gateway still gets its last flush out.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.spec.URL.Value(), bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-ndjson")
		req.Header.Set("User-Agent", "genaiopengateway-stream")
		for k, v := range s.spec.Headers {
			req.Header.Set(k, v.Value())
		}
		if sec := s.spec.Secret.Value(); sec != "" {
			req.Header.Set(alerts.SignatureHeader, alerts.Sign(sec, time.Now(), body))
		}
		resp, err := s.m.client.Do(req)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("status %d", resp.StatusCode)
		}
		return nil
	}
	return fmt.Errorf("unknown stream type %q", s.spec.Type)
}
