package bodylog

import (
	"bytes"
	"encoding/json"
)

// Stream is the reverse of Assemble: it turns an assembled response back
// into a short event stream in the same API format, so a client that
// asked for a stream gets one. Text and tool arguments arrive whole in
// one event each. It reports false for a body it does not recognize.
func Stream(assembled []byte) ([]byte, bool) {
	var probe struct {
		Object     string          `json:"object"`
		Type       string          `json:"type"`
		Candidates json.RawMessage `json:"candidates"`
	}
	if json.Unmarshal(assembled, &probe) != nil {
		return nil, false
	}
	switch {
	case probe.Object == "chat.completion":
		return chatEvents(assembled)
	case probe.Type == "message":
		return anthropicEvents(assembled)
	case probe.Object == "response":
		return responsesEvents(assembled)
	case probe.Candidates != nil:
		// A Gemini stream is a series of partial responses; one whole
		// response is a valid stream of one event.
		var w eventWriter
		w.event("", json.RawMessage(assembled))
		return w.buf.Bytes(), true
	}
	return nil, false
}

type eventWriter struct{ buf bytes.Buffer }

func (w *eventWriter) event(name string, v any) {
	data, _ := json.Marshal(v)
	if name != "" {
		w.buf.WriteString("event: " + name + "\n")
	}
	w.buf.WriteString("data: ")
	w.buf.Write(data)
	w.buf.WriteString("\n\n")
}

func chatEvents(assembled []byte) ([]byte, bool) {
	var c struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Created json.RawMessage `json:"created"`
		Usage   json.RawMessage `json:"usage"`
		Choices []struct {
			Index        int            `json:"index"`
			Message      map[string]any `json:"message"`
			FinishReason *string        `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(assembled, &c) != nil {
		return nil, false
	}
	var w eventWriter
	chunk := func(choices []any, usage json.RawMessage) {
		m := map[string]any{"id": c.ID, "object": "chat.completion.chunk", "model": c.Model, "choices": choices}
		if len(c.Created) > 0 {
			m["created"] = c.Created
		}
		if len(usage) > 0 && string(usage) != "null" {
			m["usage"] = usage
		}
		w.event("", m)
	}
	for _, ch := range c.Choices {
		delta := map[string]any{"role": "assistant"}
		for _, k := range []string{"content", "reasoning_content", "refusal"} {
			if v, ok := ch.Message[k].(string); ok && v != "" {
				delta[k] = v
			}
		}
		if calls, ok := ch.Message["tool_calls"].([]any); ok && len(calls) > 0 {
			indexed := make([]any, 0, len(calls))
			for i, tc := range calls {
				if m, ok := tc.(map[string]any); ok {
					m["index"] = i
					indexed = append(indexed, m)
				}
			}
			delta["tool_calls"] = indexed
		}
		chunk([]any{map[string]any{"index": ch.Index, "delta": delta}}, nil)
		chunk([]any{map[string]any{"index": ch.Index, "delta": map[string]any{}, "finish_reason": ch.FinishReason}}, nil)
	}
	if len(c.Usage) > 0 && string(c.Usage) != "null" {
		chunk([]any{}, c.Usage)
	}
	w.buf.WriteString("data: [DONE]\n\n")
	return w.buf.Bytes(), true
}

func anthropicEvents(assembled []byte) ([]byte, bool) {
	var msg map[string]any
	if json.Unmarshal(assembled, &msg) != nil {
		return nil, false
	}
	content, _ := msg["content"].([]any)
	start := map[string]any{}
	for k, v := range msg {
		start[k] = v
	}
	start["content"] = []any{}
	start["stop_reason"], start["stop_sequence"] = nil, nil
	var w eventWriter
	w.event("message_start", map[string]any{"type": "message_start", "message": start})
	for i, b := range content {
		block, _ := b.(map[string]any)
		if block == nil {
			continue
		}
		head := map[string]any{}
		for k, v := range block {
			head[k] = v
		}
		var delta map[string]any
		switch block["type"] {
		case "text":
			head["text"] = ""
			delta = map[string]any{"type": "text_delta", "text": block["text"]}
		case "thinking":
			head["thinking"] = ""
			delta = map[string]any{"type": "thinking_delta", "thinking": block["thinking"]}
		case "tool_use", "server_tool_use":
			head["input"] = map[string]any{}
			input, _ := json.Marshal(block["input"])
			delta = map[string]any{"type": "input_json_delta", "partial_json": string(input)}
		}
		w.event("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": head})
		if delta != nil {
			w.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": delta})
		}
		w.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
	}
	w.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": msg["stop_reason"], "stop_sequence": msg["stop_sequence"]},
		"usage": msg["usage"],
	})
	w.event("message_stop", map[string]any{"type": "message_stop"})
	return w.buf.Bytes(), true
}

func responsesEvents(assembled []byte) ([]byte, bool) {
	var resp map[string]any
	if json.Unmarshal(assembled, &resp) != nil {
		return nil, false
	}
	output, _ := resp["output"].([]any)
	seq := 0
	var w eventWriter
	emit := func(typ string, fields map[string]any) {
		fields["type"], fields["sequence_number"] = typ, seq
		seq++
		w.event(typ, fields)
	}
	created := map[string]any{}
	for k, v := range resp {
		created[k] = v
	}
	created["status"], created["output"] = "in_progress", []any{}
	emit("response.created", map[string]any{"response": created})
	for oi, it := range output {
		item, _ := it.(map[string]any)
		if item == nil {
			continue
		}
		emit("response.output_item.added", map[string]any{"output_index": oi, "item": item})
		parts, _ := item["content"].([]any)
		for ci, p := range parts {
			part, _ := p.(map[string]any)
			if part == nil || part["type"] != "output_text" {
				continue
			}
			ids := func(m map[string]any) map[string]any {
				m["item_id"], m["output_index"], m["content_index"] = item["id"], oi, ci
				return m
			}
			emit("response.output_text.delta", ids(map[string]any{"delta": part["text"]}))
			emit("response.output_text.done", ids(map[string]any{"text": part["text"]}))
		}
		if item["type"] == "function_call" {
			emit("response.function_call_arguments.done", map[string]any{"item_id": item["id"], "output_index": oi, "arguments": item["arguments"]})
		}
		emit("response.output_item.done", map[string]any{"output_index": oi, "item": item})
	}
	final := "response.completed"
	switch resp["status"] {
	case "incomplete":
		final = "response.incomplete"
	case "failed":
		final = "response.failed"
	}
	emit(final, map[string]any{"response": resp})
	return w.buf.Bytes(), true
}
