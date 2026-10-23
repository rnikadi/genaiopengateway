package usage

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Writer batches records, such as usage records, and writes them off the
// request path.
type Writer[T any] struct {
	sink      func(context.Context, []T) error
	log       *slog.Logger
	what      string
	queue     chan T
	batchSize int
	interval  time.Duration
	dropped   atomic.Int64
	failed    atomic.Int64
	done      chan struct{}
	mu        sync.RWMutex // guards closed against Add racing Close
	closed    bool
}

// Writer defaults; see docs/observability.md.
const (
	queueSize = 100_000
	batchSize = 500
	interval  = time.Second
)

// NewWriter starts a writer that calls sink with batches of records.
// what names the records in log messages.
func NewWriter[T any](sink func(context.Context, []T) error, log *slog.Logger, what string) *Writer[T] {
	w := &Writer[T]{
		sink: sink, log: log, what: what,
		queue:     make(chan T, queueSize),
		batchSize: batchSize, interval: interval,
		done: make(chan struct{}),
	}
	go w.run()
	return w
}

// Add queues a record without blocking. When the queue is full the
// record is dropped and counted.
func (w *Writer[T]) Add(r T) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
		w.dropped.Add(1)
		return
	}
	select {
	case w.queue <- r:
	default:
		w.dropped.Add(1)
	}
}

// Dropped returns how many records were dropped because the queue was
// full or a batch could not be written.
func (w *Writer[T]) Dropped() int64 { return w.dropped.Load() + w.failed.Load() }

// Close stops accepting records and flushes what is queued, waiting at
// most until ctx is done.
func (w *Writer[T]) Close(ctx context.Context) {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.queue)
	}
	w.mu.Unlock()
	select {
	case <-w.done:
	case <-ctx.Done():
		w.log.Warn("writer did not flush before shutdown", "records", w.what)
	}
}

func (w *Writer[T]) run() {
	defer close(w.done)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	var batch []T
	for {
		select {
		case r, ok := <-w.queue:
			if !ok {
				w.flush(batch, true)
				return
			}
			batch = append(batch, r)
			if len(batch) >= w.batchSize {
				batch = w.flush(batch, false)
			}
		case <-ticker.C:
			batch = w.flush(batch, false)
		}
	}
}

// flush writes a batch and returns what is left to retry. A failed batch
// is retried on the next tick; when it grows past ten batches the oldest
// records are dropped so memory stays bounded.
func (w *Writer[T]) flush(batch []T, final bool) []T {
	if len(batch) == 0 {
		return batch
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w.sink(ctx, batch); err != nil {
		w.log.Error("writing records failed", "records", w.what, "count", len(batch), "err", err)
		if final {
			w.failed.Add(int64(len(batch)))
			return nil
		}
		if over := len(batch) - 10*w.batchSize; over > 0 {
			w.failed.Add(int64(over))
			batch = batch[over:]
		}
		return batch
	}
	return batch[:0]
}
