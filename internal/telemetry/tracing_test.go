package telemetry

import (
	"testing"
)

func TestRequestAttributes(t *testing.T) {
	got := map[string]string{}
	for _, kv := range RequestAttributes([]byte(`{"max_completion_tokens":100,"temperature":0.5,"top_p":0.9,"seed":7,"stop":["END","STOP"],"n":2}`)) {
		got[string(kv.Key)] = kv.Value.String()
	}
	want := map[string]string{
		"gen_ai.request.max_tokens": "100", "gen_ai.request.temperature": "0.5", "gen_ai.request.top_p": "0.9",
		"gen_ai.request.seed": "7", "gen_ai.request.stop_sequences": `["END","STOP"]`, "gen_ai.request.choice.count": "2",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if len(RequestAttributes([]byte(`{"model":"m"}`))) != 0 || RequestAttributes([]byte(`not json`)) != nil {
		t.Error("attributes from a body without parameters")
	}
	if SpanName("chat", "gpt") != "chat gpt" || SpanName("chat", "") != "chat" {
		t.Error("span name")
	}
}
