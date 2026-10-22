package usage

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type sink struct {
	mu      sync.Mutex
	got     []Record
	batches int
	fail    int
}

func (s *sink) write(_ context.Context, b []Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail > 0 {
		s.fail--
		return errors.New("db down")
	}
	s.got = append(s.got, b...)
	s.batches++
	return nil
}

func (s *sink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestWriterFlushesOnCloseAndBatches(t *testing.T) {
	s := &sink{}
	w := NewWriter(s.write, quiet)
	for i := 0; i < batchSize+10; i++ {
		w.Add(Record{RequestID: "r"})
	}
	w.Close(context.Background())
	if s.count() != batchSize+10 || s.batches < 2 {
		t.Fatalf("wrote %d records in %d batches", s.count(), s.batches)
	}
	w.Close(context.Background()) // second close is safe
}

func TestWriterRetriesFailedBatch(t *testing.T) {
	s := &sink{fail: 1}
	w := NewWriter(s.write, quiet)
	w.Add(Record{RequestID: "r"})
	deadline := time.Now().Add(5 * time.Second)
	for s.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	w.Close(context.Background())
	if s.count() != 1 || w.Dropped() != 0 {
		t.Fatalf("count %d dropped %d", s.count(), w.Dropped())
	}
}
