// Package bodylog turns captured request and response bodies into what
// body logging stores (docs/body-logging.md).
package bodylog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"mime"
	"sort"
	"strings"
)

// Assemble returns a response body as stored: the body itself, or for a
// server-sent event stream the result the stream built up. OpenAI chat
// completions, OpenAI responses and Anthropic messages streams are
// assembled; any other stream is returned unchanged.
func Assemble(contentType string, body []byte) []byte {
	if mt, _, _ := mime.ParseMediaType(contentType); mt != "text/event-stream" {
		return body
	}
	events := parseSSE(body)
	for _, assemble := range []func([]sseEvent) ([]byte, bool){responsesStream, anthropicStream, chatStream} {
		if out, ok := assemble(events); ok {
			return out
		}
	}
	return body
}

type sseEvent struct {
	name string
	data []byte
}

func parseSSE(body []byte) []sseEvent {
	var out []sseEvent
	var cur sseEvent
	var data [][]byte
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	flush := func() {
		if len(data) > 0 {
			cur.data = bytes.Join(data, []byte("\n"))
			out = append(out, cur)
		}
		cur, data = sseEvent{}, nil
	}
	for sc.Scan() {
		line := sc.Bytes()
		switch {
		case len(line) == 0:
			flush()
		case bytes.HasPrefix(line, []byte("event:")):
			cur.name = strings.TrimSpace(string(line[6:]))
		case bytes.HasPrefix(line, []byte("data:")):
			data = append(data, bytes.TrimPrefix(line[5:], []byte(" ")))
		}
	}
	flush()
	return out
}

// responsesStream takes the final response from response.completed.
func responsesStream(events []sseEvent) ([]byte, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		var e struct {
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
		}
		if json.Unmarshal(events[i].data, &e) == nil && (e.Type == "response.completed" || e.Type == "response.incomplete" || e.Type == "response.failed") && e.Response != nil {
			return e.Response, true
		}
	}
	return nil, false
}

// chatStream assembles chat.completion.chunk events into a completion.
func chatStream(events []sseEvent) ([]byte, bool) {
	type toolCall struct {
		ID       string `json:"id,omitempty"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	type choice struct {
		content      strings.Builder
		reasoning    strings.Builder
		tools        map[int]*toolCall
		finishReason string
	}
	var model, id string
	var usage json.RawMessage
	choices := map[int]*choice{}
	seen := false
	for _, ev := range events {
		var c struct {
			ID      string          `json:"id"`
			Object  string          `json:"object"`
			Model   string          `json:"model"`
			Usage   json.RawMessage `json:"usage"`
			Choices []struct {
				Index int `json:"index"`
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal(ev.data, &c) != nil || c.Object != "chat.completion.chunk" {
			continue
		}
		seen = true
		if c.Model != "" {
			model = c.Model
		}
		if c.ID != "" {
			id = c.ID
		}
		if len(c.Usage) > 0 && string(c.Usage) != "null" {
			usage = c.Usage
		}
		for _, ch := range c.Choices {
			st := choices[ch.Index]
			if st == nil {
				st = &choice{tools: map[int]*toolCall{}}
				choices[ch.Index] = st
			}
			st.content.WriteString(ch.Delta.Content)
			st.reasoning.WriteString(ch.Delta.ReasoningContent)
			for _, tc := range ch.Delta.ToolCalls {
				t := st.tools[tc.Index]
				if t == nil {
					t = &toolCall{Type: "function"}
					st.tools[tc.Index] = t
				}
				if tc.ID != "" {
					t.ID = tc.ID
				}
				t.Function.Name += tc.Function.Name
				t.Function.Arguments += tc.Function.Arguments
			}
			if ch.FinishReason != nil {
				st.finishReason = *ch.FinishReason
			}
		}
	}
	if !seen {
		return nil, false
	}
	type message struct {
		Role             string     `json:"role"`
		Content          string     `json:"content"`
		ReasoningContent string     `json:"reasoning_content,omitempty"`
		ToolCalls        []toolCall `json:"tool_calls,omitempty"`
	}
	type outChoice struct {
		Index        int     `json:"index"`
		Message      message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	}
	out := struct {
		ID      string          `json:"id,omitempty"`
		Object  string          `json:"object"`
		Model   string          `json:"model,omitempty"`
		Choices []outChoice     `json:"choices"`
		Usage   json.RawMessage `json:"usage,omitempty"`
	}{ID: id, Object: "chat.completion", Model: model, Usage: usage}
	for _, idx := range sortedKeys(choices) {
		st := choices[idx]
		m := message{Role: "assistant", Content: st.content.String(), ReasoningContent: st.reasoning.String()}
		for _, ti := range sortedKeys(st.tools) {
			m.ToolCalls = append(m.ToolCalls, *st.tools[ti])
		}
		out.Choices = append(out.Choices, outChoice{Index: idx, Message: m, FinishReason: st.finishReason})
	}
	b, _ := json.Marshal(out)
	return b, true
}

// anthropicStream assembles Anthropic messages events into a message.
func anthropicStream(events []sseEvent) ([]byte, bool) {
	var msg map[string]any
	type block struct {
		raw     map[string]any
		text    strings.Builder
		partial strings.Builder
	}
	blocks := map[int]*block{}
	for _, ev := range events {
		var e struct {
			Type         string          `json:"type"`
			Message      map[string]any  `json:"message"`
			Index        int             `json:"index"`
			ContentBlock map[string]any  `json:"content_block"`
			Delta        json.RawMessage `json:"delta"`
			Usage        map[string]any  `json:"usage"`
		}
		if json.Unmarshal(ev.data, &e) != nil {
			continue
		}
		switch e.Type {
		case "message_start":
			msg = e.Message
		case "content_block_start":
			blocks[e.Index] = &block{raw: e.ContentBlock}
		case "content_block_delta":
			b := blocks[e.Index]
			if b == nil {
				continue
			}
			var d struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				Thinking    string `json:"thinking"`
			}
			_ = json.Unmarshal(e.Delta, &d)
			b.text.WriteString(d.Text + d.Thinking)
			b.partial.WriteString(d.PartialJSON)
		case "message_delta":
			if msg == nil {
				continue
			}
			var d map[string]any
			_ = json.Unmarshal(e.Delta, &d)
			for k, v := range d {
				msg[k] = v
			}
			if e.Usage != nil {
				u, _ := msg["usage"].(map[string]any)
				if u == nil {
					u = map[string]any{}
				}
				for k, v := range e.Usage {
					u[k] = v
				}
				msg["usage"] = u
			}
		}
	}
	if msg == nil {
		return nil, false
	}
	var content []any
	for _, idx := range sortedKeys(blocks) {
		b := blocks[idx]
		c := b.raw
		if c == nil {
			c = map[string]any{}
		}
		switch c["type"] {
		case "text":
			c["text"] = b.text.String()
		case "thinking":
			c["thinking"] = b.text.String()
		case "tool_use", "server_tool_use":
			var input any
			if json.Unmarshal([]byte(b.partial.String()), &input) == nil {
				c["input"] = input
			} else if b.partial.Len() > 0 {
				c["input"] = b.partial.String()
			}
		}
		content = append(content, c)
	}
	msg["content"] = content
	out, _ := json.Marshal(msg)
	return out, true
}

func sortedKeys[V any](m map[int]V) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}
