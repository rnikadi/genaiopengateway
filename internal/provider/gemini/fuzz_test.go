package gemini

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"

	"github.com/rnikadi/genaiopengateway/internal/usage"
)

// The translators read what clients and upstreams send, so they must
// not panic or hang on any input (docs/security-audit.md#fuzzing). CI
// runs these continuously with ClusterFuzzLite.

func FuzzTranslateRequest(f *testing.F) {
	for _, s := range []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","max_tokens":5,"messages":[{"role":"system","content":"s"},{"role":"user","content":[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`,
		`{"model":"m","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c","content":"r"}],"tools":[{"type":"function","function":{"name":"f","parameters":{}}}],"tool_choice":{"type":"function","function":{"name":"f"}}}`,
		`{"model":"m","messages":[{"role":"user","content":"x"}],"response_format":{"type":"json_schema","json_schema":{"name":"n","schema":{}}},"stop":["a"],"stream":true,"stream_options":{"include_usage":true}}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var cr chatRequest
		if json.Unmarshal(data, &cr) != nil {
			return
		}
		out, err := translateRequest(&cr)
		if err != nil {
			return
		}
		if _, err := json.Marshal(out); err != nil {
			t.Fatalf("a translated request does not encode: %v", err)
		}
	})
}

func FuzzTranslateResponse(f *testing.F) {
	f.Add([]byte(`{}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var mr genResponse
		if json.Unmarshal(data, &mr) != nil {
			return
		}
		if _, err := json.Marshal(translateResponse(&mr, "m")); err != nil {
			t.Fatalf("a translated response does not encode: %v", err)
		}
	})
}

func FuzzTranslateStream(f *testing.F) {
	f.Add([]byte("event: message_start\ndata: {}\n\n"))
	f.Add([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}]}\n\ndata: [DONE]\n\n"))
	f.Add([]byte{0, 0, 0, 16, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		var tok usage.Tokens
		body := translateStream(io.NopCloser(bytes.NewReader(data)), &tok, "m", true)
		_, _ = io.Copy(io.Discard, io.LimitReader(body, 64<<20))
		_ = body.Close()
	})
}
