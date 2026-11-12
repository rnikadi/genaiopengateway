package server

import (
	"bytes"
	"encoding/json"
	"testing"
)

// The WebSocket relays rewrite or drop some client messages so that a
// session stays on the model it was authorized for. These fuzz targets
// check that property against any message (docs/security-audit.md#fuzzing).

// decodedObject parses a message as the upstream would: escapes decoded,
// and with any duplicate key reported.
func decodedObject(data []byte) (map[string]json.RawMessage, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil {
		return nil, false
	}
	return m, !hasDuplicateKeys(data)
}

func FuzzRealtimeSessionModel(f *testing.F) {
	for _, s := range []string{
		`{"type":"session.update","session":{"model":"gpt-x","voice":"v"}}`,
		`{"type":"session.update","session":{"model":"other"}}`,
		`{"type":"session.update","session":{"model":"other"},"type":"x"}`,
		`{"type":"session.update","session":{"model":"other"}}`,
		`{"type":"input_audio_buffer.append","audio":"AAAA"}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		out := withSessionModel(data, "allowed")
		ev, ok := decodedObject(out)
		if ev == nil {
			return // not JSON: the upstream refuses it
		}
		// Only a message that could name session.update to some parser
		// must be free of duplicate keys.
		if !ok && (bytes.Contains(out, []byte("session.update")) || bytes.Contains(out, []byte(`\u`))) {
			t.Fatalf("a message with duplicate keys passed: %s", out)
		}
		var typ string
		if json.Unmarshal(ev["type"], &typ) != nil || typ != "session.update" {
			return
		}
		sess, ok := decodedObject(ev["session"])
		if sess == nil || sess["model"] == nil {
			return
		}
		var model string
		if !ok || json.Unmarshal(sess["model"], &model) != nil || model != "allowed" {
			t.Fatalf("a session.update chose another model: %s -> %s", data, out)
		}
	})
}

func FuzzLiveSetup(f *testing.F) {
	f.Add([]byte(`{"setup":{"model":"models/gemini-live","generationConfig":{}}}`))
	f.Add([]byte(`{"setup":{"model":"a","model":"b"}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		model, rewrite := liveSetup(data)
		if rewrite == nil {
			return
		}
		if model == "" {
			t.Fatal("a setup without a model was accepted")
		}
		msg, ok := decodedObject(rewrite("gemini-upstream"))
		if msg == nil || !ok {
			t.Fatalf("the rewritten setup is not one JSON object: %s", rewrite("gemini-upstream"))
		}
		setup, ok := decodedObject(msg["setup"])
		var got string
		if setup == nil || !ok || json.Unmarshal(setup["model"], &got) != nil || got != "models/gemini-upstream" {
			t.Fatalf("the setup names %q after the rewrite: %s", got, data)
		}
	})
}

func FuzzLiveClientMessage(f *testing.F) {
	f.Add([]byte(`{"setup":{"model":"other"}}`))
	f.Add([]byte(`{"setup":{"model":"other"}}`))
	f.Add([]byte(`{"realtimeInput":{"text":"hi"}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		out := liveClientMessage(data)
		if msg, _ := decodedObject(out); msg != nil && msg["setup"] != nil {
			t.Fatalf("a second setup passed: %s", out)
		}
	})
}

func TestHasDuplicateKeys(t *testing.T) {
	for in, want := range map[string]bool{
		`{"a":1,"b":2}`:                       false,
		`{"a":1,"a":2}`:                       true,
		`{"a":{"b":1},"c":[{"b":1},{"b":2}]}`: false,
		`{"a":{"b":1,"b":2}}`:                 true,
		`{"a":[{"x":1,"x":1}]}`:               true,
		`{"a":"a","b":["a","a"]}`:             false,
		`{"ab":1,"ab":2}`:                     true,
		`[{"k":1},{"k":2}]`:                   false,
		`not json`:                            false,
	} {
		if got := hasDuplicateKeys([]byte(in)); got != want {
			t.Errorf("hasDuplicateKeys(%s) = %v, want %v", in, got, want)
		}
	}
}
