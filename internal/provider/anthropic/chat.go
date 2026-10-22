package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

// defaultMaxTokens is used when the client sets no limit, since the
// Messages API requires one.
const defaultMaxTokens = 4096

// --- OpenAI request shapes (the subset we translate) ---

type chatRequest struct {
	Model               string          `json:"model"`
	Messages            []chatMessage   `json:"messages"`
	MaxTokens           *int            `json:"max_tokens"`
	MaxCompletionTokens *int            `json:"max_completion_tokens"`
	Temperature         *float64        `json:"temperature"`
	TopP                *float64        `json:"top_p"`
	Stop                json.RawMessage `json:"stop"`
	Stream              bool            `json:"stream"`
	StreamOptions       *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	Tools             []chatTool      `json:"tools"`
	ToolChoice        json.RawMessage `json:"tool_choice"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls"`
	N                 *int            `json:"n"`
	Logprobs          *bool           `json:"logprobs"`
	ResponseFormat    *struct {
		Type string `json:"type"`
	} `json:"response_format"`
	User string `json:"user"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []chatToolCall  `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

type chatPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// --- Anthropic request shapes ---

type msgRequest struct {
	Model         string          `json:"model"`
	System        string          `json:"system,omitempty"`
	Messages      []msgMessage    `json:"messages"`
	MaxTokens     int             `json:"max_tokens"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	Tools         []msgTool       `json:"tools,omitempty"`
	ToolChoice    *msgToolChoice  `json:"tool_choice,omitempty"`
	Metadata      *map[string]any `json:"metadata,omitempty"`
}

type msgMessage struct {
	Role    string     `json:"role"`
	Content []msgBlock `json:"content"`
}

type msgBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Source    *imageSource    `json:"source,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
}

type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type msgTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type msgToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use,omitempty"`
}

// invalid is a client error found while translating.
type invalid string

func (e invalid) Error() string { return string(e) }

// translateRequest converts an OpenAI chat request into a Messages
// request. Unsupported features are errors, never silently dropped.
func translateRequest(cr *chatRequest) (*msgRequest, error) {
	switch {
	case cr.N != nil && *cr.N > 1:
		return nil, invalid("n greater than 1 is not supported for this model")
	case cr.Logprobs != nil && *cr.Logprobs:
		return nil, invalid("logprobs is not supported for this model")
	case cr.ResponseFormat != nil && cr.ResponseFormat.Type == "json_schema":
		return nil, invalid("response_format json_schema is not supported for this model")
	}
	mr := &msgRequest{Model: cr.Model, Temperature: cr.Temperature, TopP: cr.TopP, Stream: cr.Stream, MaxTokens: defaultMaxTokens}
	switch {
	case cr.MaxCompletionTokens != nil:
		mr.MaxTokens = *cr.MaxCompletionTokens
	case cr.MaxTokens != nil:
		mr.MaxTokens = *cr.MaxTokens
	}
	if len(cr.Stop) > 0 && string(cr.Stop) != "null" {
		var one string
		if json.Unmarshal(cr.Stop, &one) == nil {
			mr.StopSequences = []string{one}
		} else if json.Unmarshal(cr.Stop, &mr.StopSequences) != nil {
			return nil, invalid("stop must be a string or an array of strings")
		}
	}
	if cr.User != "" {
		mr.Metadata = &map[string]any{"user_id": cr.User}
	}

	var system []string
	for i, m := range cr.Messages {
		switch m.Role {
		case "system", "developer":
			text, err := textOf(m.Content)
			if err != nil {
				return nil, invalid(fmt.Sprintf("messages[%d]: %v", i, err))
			}
			system = append(system, text)
		case "user":
			blocks, err := userBlocks(m.Content)
			if err != nil {
				return nil, invalid(fmt.Sprintf("messages[%d]: %v", i, err))
			}
			mr.Messages = appendMessage(mr.Messages, "user", blocks)
		case "assistant":
			var blocks []msgBlock
			if text, err := textOf(m.Content); err != nil {
				return nil, invalid(fmt.Sprintf("messages[%d]: %v", i, err))
			} else if text != "" {
				blocks = append(blocks, msgBlock{Type: "text", Text: text})
			}
			for _, tc := range m.ToolCalls {
				input := json.RawMessage(tc.Function.Arguments)
				if strings.TrimSpace(tc.Function.Arguments) == "" {
					input = json.RawMessage("{}")
				} else if !json.Valid(input) {
					return nil, invalid(fmt.Sprintf("messages[%d]: tool call arguments are not valid JSON", i))
				}
				blocks = append(blocks, msgBlock{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: input})
			}
			if len(blocks) > 0 {
				mr.Messages = appendMessage(mr.Messages, "assistant", blocks)
			}
		case "tool":
			text, err := textOf(m.Content)
			if err != nil {
				return nil, invalid(fmt.Sprintf("messages[%d]: %v", i, err))
			}
			mr.Messages = appendMessage(mr.Messages, "user", []msgBlock{{Type: "tool_result", ToolUseID: m.ToolCallID, Content: text}})
		default:
			return nil, invalid(fmt.Sprintf("messages[%d]: unsupported role %q", i, m.Role))
		}
	}
	mr.System = strings.Join(system, "\n\n")

	for _, t := range cr.Tools {
		if t.Type != "function" {
			return nil, invalid(fmt.Sprintf("tool type %q is not supported", t.Type))
		}
		schema := t.Function.Parameters
		if len(schema) == 0 || string(schema) == "null" {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		mr.Tools = append(mr.Tools, msgTool{Name: t.Function.Name, Description: t.Function.Description, InputSchema: schema})
	}
	if len(cr.ToolChoice) > 0 && string(cr.ToolChoice) != "null" {
		tc, err := toolChoice(cr.ToolChoice)
		if err != nil {
			return nil, err
		}
		mr.ToolChoice = tc
	}
	if cr.ParallelToolCalls != nil && !*cr.ParallelToolCalls && len(mr.Tools) > 0 {
		if mr.ToolChoice == nil {
			mr.ToolChoice = &msgToolChoice{Type: "auto"}
		}
		if mr.ToolChoice.Type != "none" {
			mr.ToolChoice.DisableParallelToolUse = true
		}
	}
	return mr, nil
}

// appendMessage merges consecutive messages of one role, since the
// Messages API requires roles to alternate.
func appendMessage(msgs []msgMessage, role string, blocks []msgBlock) []msgMessage {
	if n := len(msgs); n > 0 && msgs[n-1].Role == role {
		msgs[n-1].Content = append(msgs[n-1].Content, blocks...)
		return msgs
	}
	return append(msgs, msgMessage{Role: role, Content: blocks})
}

// textOf returns the text of string or text-part content.
func textOf(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var parts []chatPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", fmt.Errorf("content must be a string or an array of parts")
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type != "text" {
			return "", fmt.Errorf("content part type %q is not allowed here", p.Type)
		}
		b.WriteString(p.Text)
	}
	return b.String(), nil
}

func userBlocks(raw json.RawMessage) ([]msgBlock, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []msgBlock{{Type: "text", Text: s}}, nil
	}
	var parts []chatPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("content must be a string or an array of parts")
	}
	var blocks []msgBlock
	for _, p := range parts {
		switch p.Type {
		case "text":
			blocks = append(blocks, msgBlock{Type: "text", Text: p.Text})
		case "image_url":
			if p.ImageURL == nil {
				return nil, fmt.Errorf("image_url part has no url")
			}
			src, err := imageFrom(p.ImageURL.URL)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, msgBlock{Type: "image", Source: src})
		default:
			return nil, fmt.Errorf("content part type %q is not supported for this model", p.Type)
		}
	}
	return blocks, nil
}

// imageFrom converts a data URL or an http(s) URL into an image source.
func imageFrom(u string) (*imageSource, error) {
	if rest, ok := strings.CutPrefix(u, "data:"); ok {
		meta, data, ok := strings.Cut(rest, ",")
		mediaType, isBase64 := strings.CutSuffix(meta, ";base64")
		if !ok || !isBase64 || mediaType == "" {
			return nil, fmt.Errorf("image data URLs must be base64 encoded with a media type")
		}
		return &imageSource{Type: "base64", MediaType: mediaType, Data: data}, nil
	}
	if strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") {
		return &imageSource{Type: "url", URL: u}, nil
	}
	return nil, fmt.Errorf("image url must be a data URL or an http(s) URL")
}

func toolChoice(raw json.RawMessage) (*msgToolChoice, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto":
			return &msgToolChoice{Type: "auto"}, nil
		case "none":
			return &msgToolChoice{Type: "none"}, nil
		case "required":
			return &msgToolChoice{Type: "any"}, nil
		}
		return nil, invalid(fmt.Sprintf("tool_choice %q is not supported", s))
	}
	var named struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &named) != nil || named.Type != "function" || named.Function.Name == "" {
		return nil, invalid("tool_choice must be auto, none, required or a named function")
	}
	return &msgToolChoice{Type: "tool", Name: named.Function.Name}, nil
}

// --- responses ---

type msgResponse struct {
	ID         string     `json:"id"`
	Model      string     `json:"model"`
	Content    []msgBlock `json:"content"`
	StopReason string     `json:"stop_reason"`
	Usage      anthUsage  `json:"usage"`
}

func finishReason(stop string) string {
	switch stop {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	}
	return "stop"
}

func chatUsage(u anthUsage) map[string]any {
	t := u.tokens()
	return map[string]any{
		"prompt_tokens":         t.InputTokens,
		"completion_tokens":     t.OutputTokens,
		"total_tokens":          t.Total(),
		"prompt_tokens_details": map[string]any{"cached_tokens": t.CachedInputTokens},
	}
}

func translateResponse(mr *msgResponse) map[string]any {
	var text strings.Builder
	var calls []map[string]any
	for _, b := range mr.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			calls = append(calls, map[string]any{
				"id": b.ID, "type": "function",
				"function": map[string]any{"name": b.Name, "arguments": string(b.Input)},
			})
		}
	}
	msg := map[string]any{"role": "assistant", "content": nil}
	if text.Len() > 0 {
		msg["content"] = text.String()
	}
	if len(calls) > 0 {
		msg["tool_calls"] = calls
	}
	return map[string]any{
		"id": mr.ID, "object": "chat.completion", "created": time.Now().Unix(), "model": mr.Model,
		"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": finishReason(mr.StopReason)}},
		"usage":   chatUsage(mr.Usage),
	}
}

// chat serves an OpenAI chat completion from the Messages API.
func (p *Provider) chat(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	var cr chatRequest
	if err := json.Unmarshal(req.Body, &cr); err != nil {
		return errorResponse(http.StatusBadRequest, "invalid_request_error", "request body is not a valid chat completion request"), nil
	}
	mr, err := translateRequest(&cr)
	if err != nil {
		return errorResponse(http.StatusBadRequest, "invalid_request_error", err.Error()), nil
	}
	body, err := json.Marshal(mr)
	if err != nil {
		return nil, err
	}
	resp, err := p.post(ctx, body, cr.Stream, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return translateError(resp)
	}
	resp.Header = http.Header{}
	if cr.Stream {
		resp.Header.Set("Content-Type", "text/event-stream")
		includeUsage := cr.StreamOptions != nil && cr.StreamOptions.IncludeUsage
		resp.Body = translateStream(resp.Body, resp.Usage, includeUsage)
		return resp, nil
	}
	buf, err := readAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var out msgResponse
	if err := json.Unmarshal(buf, &out); err != nil {
		return errorResponse(http.StatusBadGateway, "api_error", "upstream returned an unreadable response"), nil
	}
	*resp.Usage = out.Usage.tokens()
	b, _ := json.Marshal(translateResponse(&out))
	resp.Header.Set("Content-Type", "application/json")
	resp.Body = io.NopCloser(bytes.NewReader(b))
	return resp, nil
}

// errorResponse builds an OpenAI-shaped error response.
func errorResponse(status int, typ, msg string) *provider.Response {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": typ}})
	return &provider.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(b)),
		Usage:      &usage.Tokens{},
	}
}

// translateError converts an Anthropic error body to the OpenAI shape,
// keeping the status so fallback rules still apply.
func translateError(resp *provider.Response) (*provider.Response, error) {
	buf, _ := readAll(resp.Body)
	var e struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(buf, &e) != nil || e.Error.Message == "" {
		e.Error.Type, e.Error.Message = "api_error", strings.TrimSpace(string(buf))
	}
	out := errorResponse(resp.StatusCode, e.Error.Type, e.Error.Message)
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		out.Header.Set("Retry-After", ra)
	}
	return out, nil
}

// translateStream converts a Messages event stream into OpenAI chat
// completion chunks. tokens is filled before the returned reader hits EOF.
func translateStream(upstream io.ReadCloser, tokens *usage.Tokens, includeUsage bool) io.ReadCloser {
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer upstream.Close()
		t := &streamTranslator{w: pw, created: time.Now().Unix(), toolIndex: map[int]int{}}
		err := t.run(upstream)
		*tokens = t.usage.tokens()
		if err == nil && includeUsage && t.usage.seen {
			err = t.chunk(map[string]any{"choices": []any{}, "usage": chatUsage(t.usage.u)})
		}
		if err == nil {
			_, err = io.WriteString(pw, "data: [DONE]\n\n")
		}
		pw.CloseWithError(err)
	}()
	return &translatedBody{PipeReader: pr, upstream: upstream, done: done}
}

// translatedBody stops the translator on Close and waits for it, so
// usage is final once Close returns.
type translatedBody struct {
	*io.PipeReader
	upstream io.Closer
	done     chan struct{}
}

func (b *translatedBody) Close() error {
	_ = b.PipeReader.Close()
	_ = b.upstream.Close()
	<-b.done
	return nil
}

type streamTranslator struct {
	w         io.Writer
	id, model string
	created   int64
	usage     streamUsage
	// toolIndex maps Anthropic content block indexes to OpenAI tool call
	// indexes.
	toolIndex map[int]int
}

func (t *streamTranslator) run(r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		data, ok := bytes.CutPrefix(sc.Bytes(), []byte("data:"))
		if !ok {
			continue
		}
		data = bytes.TrimSpace(data)
		t.usage.observe(data)
		if err := t.event(data); err != nil {
			return err
		}
	}
	return sc.Err()
}

func (t *streamTranslator) event(data []byte) error {
	var ev struct {
		Type    string `json:"type"`
		Index   int    `json:"index"`
		Message struct {
			ID    string `json:"id"`
			Model string `json:"model"`
		} `json:"message"`
		ContentBlock msgBlock `json:"content_block"`
		Delta        struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return nil
	}
	switch ev.Type {
	case "message_start":
		t.id, t.model = ev.Message.ID, ev.Message.Model
		return t.delta(map[string]any{"role": "assistant", "content": ""}, nil)
	case "content_block_start":
		if ev.ContentBlock.Type == "tool_use" {
			idx := len(t.toolIndex)
			t.toolIndex[ev.Index] = idx
			return t.delta(map[string]any{"tool_calls": []any{map[string]any{
				"index": idx, "id": ev.ContentBlock.ID, "type": "function",
				"function": map[string]any{"name": ev.ContentBlock.Name, "arguments": ""},
			}}}, nil)
		}
	case "content_block_delta":
		switch ev.Delta.Type {
		case "text_delta":
			return t.delta(map[string]any{"content": ev.Delta.Text}, nil)
		case "input_json_delta":
			idx, ok := t.toolIndex[ev.Index]
			if !ok {
				return nil
			}
			return t.delta(map[string]any{"tool_calls": []any{map[string]any{
				"index": idx, "function": map[string]any{"arguments": ev.Delta.PartialJSON},
			}}}, nil)
		}
	case "message_delta":
		if ev.Delta.StopReason != "" {
			reason := finishReason(ev.Delta.StopReason)
			return t.delta(map[string]any{}, &reason)
		}
	case "error":
		b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": ev.Error.Message, "type": ev.Error.Type, "code": ev.Error.Type}})
		_, err := fmt.Fprintf(t.w, "data: %s\n\n", b)
		return err
	}
	return nil
}

func (t *streamTranslator) delta(delta map[string]any, finish *string) error {
	choice := map[string]any{"index": 0, "delta": delta, "finish_reason": nil}
	if finish != nil {
		choice["finish_reason"] = *finish
	}
	return t.chunk(map[string]any{"choices": []any{choice}})
}

func (t *streamTranslator) chunk(fields map[string]any) error {
	fields["id"], fields["object"], fields["created"], fields["model"] = t.id, "chat.completion.chunk", t.created, t.model
	b, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(t.w, "data: %s\n\n", b)
	return err
}
