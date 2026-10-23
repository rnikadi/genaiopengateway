package bodylog

import (
	"encoding/json"
	"testing"
)

func TestAssembleChatStream(t *testing.T) {
	stream := `data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-x","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}

data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-x","choices":[{"index":0,"delta":{"content":"lo"}}]}

data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"get_","arguments":"{\"ci"}}]}}]}

data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"weather","arguments":"ty\":\"Paris\"}"}}]},"finish_reason":"tool_calls"}]}

data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-x","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":7}}

data: [DONE]

`
	var got struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct{ Name, Arguments string }
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]int `json:"usage"`
	}
	out := Assemble("text/event-stream; charset=utf-8", []byte(stream))
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	c := got.Choices[0]
	if got.Object != "chat.completion" || got.Model != "gpt-x" || c.Message.Content != "Hello" || c.FinishReason != "tool_calls" ||
		len(c.Message.ToolCalls) != 1 || c.Message.ToolCalls[0].ID != "call_1" || c.Message.ToolCalls[0].Function.Name != "get_weather" ||
		c.Message.ToolCalls[0].Function.Arguments != `{"city":"Paris"}` || got.Usage["completion_tokens"] != 7 {
		t.Fatalf("assembled = %s", out)
	}
}

func TestAssembleAnthropicStream(t *testing.T) {
	stream := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","content":[],"usage":{"input_tokens":12}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"there"}}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_1","name":"search","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"q\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"go\"}"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}

event: message_stop
data: {"type":"message_stop"}

`
	var got struct {
		ID         string           `json:"id"`
		StopReason string           `json:"stop_reason"`
		Content    []map[string]any `json:"content"`
		Usage      map[string]int   `json:"usage"`
	}
	out := Assemble("text/event-stream", []byte(stream))
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got.ID != "msg_1" || got.StopReason != "tool_use" || len(got.Content) != 2 || got.Content[0]["text"] != "Hi there" ||
		got.Content[1]["input"].(map[string]any)["q"] != "go" || got.Usage["input_tokens"] != 12 || got.Usage["output_tokens"] != 9 {
		t.Fatalf("assembled = %s", out)
	}
}

func TestAssembleResponsesStream(t *testing.T) {
	stream := `event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"Hi"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"Hi"}]}]}}

`
	out := Assemble("text/event-stream", []byte(stream))
	if string(out) != `{"id":"resp_1","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"Hi"}]}]}` {
		t.Fatalf("assembled = %s", out)
	}
}

func TestAssembleLeavesOtherBodies(t *testing.T) {
	if out := Assemble("application/json", []byte(`{"a":1}`)); string(out) != `{"a":1}` {
		t.Fatalf("json body changed: %s", out)
	}
	unknown := "data: {\"hello\":1}\n\n"
	if out := Assemble("text/event-stream", []byte(unknown)); string(out) != unknown {
		t.Fatalf("unknown stream changed: %s", out)
	}
}
