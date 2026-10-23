package bodylog

import (
	"encoding/json"
	"reflect"
	"testing"
)

// Stream must produce a stream that Assemble turns back into the same
// response, in each format.
func TestStreamRoundTrips(t *testing.T) {
	cases := map[string]string{
		"chat": `{"id":"c1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"Hello [REDACTED:email]",
			"tool_calls":[{"id":"t1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":3,"completion_tokens":4}}`,
		"anthropic": `{"id":"m1","type":"message","role":"assistant","model":"claude","content":[{"type":"text","text":"Hi there"},
			{"type":"tool_use","id":"t1","name":"f","input":{"q":"x"}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":4}}`,
		"responses": `{"id":"r1","object":"response","status":"completed","model":"m","output":[{"type":"message","id":"msg1","role":"assistant",
			"content":[{"type":"output_text","text":"Answer"}]}],"usage":{"input_tokens":1,"output_tokens":2}}`,
	}
	for name, body := range cases {
		ev, ok := Stream([]byte(body))
		if !ok {
			t.Fatalf("%s: not recognized", name)
		}
		back := Assemble("text/event-stream", ev)
		var want, got map[string]any
		_ = json.Unmarshal([]byte(body), &want)
		if err := json.Unmarshal(back, &got); err != nil {
			t.Fatalf("%s: %v\n%s", name, err, ev)
		}
		if name == "chat" {
			// Assemble does not keep the message's tool call indexes.
			for _, tc := range got["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any) {
				delete(tc.(map[string]any), "index")
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: round trip\n got %v\nwant %v", name, got, want)
		}
	}
	if _, ok := Stream([]byte(`{"something":"else"}`)); ok {
		t.Fatal("an unknown body was recognized")
	}
}
