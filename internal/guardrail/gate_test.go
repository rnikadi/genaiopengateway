//go:build !race

package guardrail

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
)

// TestBuiltinsLatencyGate fails when the median falls short of the gate.
func TestBuiltinsLatencyGate(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	s, raw := builtins(t), benchRequest()
	names := []string{"pii", "regex"}
	const runs = 101
	times := make([]time.Duration, runs)
	for i := range times {
		start := time.Now()
		d, _ := ParseDoc(raw)
		res := s.Run(context.Background(), config.StageInput, names, d.Texts(), Meta{})
		if !res.Changed {
			t.Fatal("nothing was redacted")
		}
		d.SetTexts(res.Texts)
		_ = d.JSON()
		times[i] = time.Since(start)
	}
	slices.Sort(times)
	if p50 := times[runs/2]; p50 > 10*time.Millisecond {
		t.Fatalf("p50 = %v, over the 10 ms gate", p50)
	} else {
		t.Logf("p50 = %v for %d bytes", p50, len(raw))
	}
}
