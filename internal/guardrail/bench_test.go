package guardrail

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rnikadi/genaiopengateway/internal/config"
)

// benchRequest is a 32 KiB chat request with a little personal data.
func benchRequest() []byte {
	var msgs []map[string]string
	para := strings.Repeat("The quarterly report covers revenue, churn and hiring plans for the platform team. ", 12)
	for len(msgs)*len(para) < 32<<10 {
		msgs = append(msgs, map[string]string{"role": "user", "content": para + "Contact jane.doe@example.com or +1 415 555 0100."})
	}
	b, _ := json.Marshal(map[string]any{"model": "chat", "messages": msgs})
	return b
}

func builtins(b testing.TB) *Set {
	s, err := New(map[string]config.Guardrail{
		"pii":   {Type: config.GuardrailPII, Action: config.ActionRedact, Stages: []string{config.StageInput}},
		"regex": {Type: config.GuardrailRegex, Patterns: []string{`(?i)project\s+falcon`, `EMP-\d{6}`}, Action: config.ActionBlock, Stages: []string{config.StageInput}},
	}, nil, "test", nil, nil)
	if err != nil {
		b.Fatal(err)
	}
	return s
}

// BenchmarkBuiltins measures every built-in on a 32 KiB request, parsing
// and re-encoding included: the v0.5 gate is under 10 ms at p50.
func BenchmarkBuiltins(b *testing.B) {
	s, raw := builtins(b), benchRequest()
	names := []string{"pii", "regex"}
	b.SetBytes(int64(len(raw)))
	for b.Loop() {
		d, _ := ParseDoc(raw)
		res := s.Run(context.Background(), config.StageInput, names, d.Texts(), Meta{})
		d.SetTexts(res.Texts)
		_ = d.JSON()
	}
}
