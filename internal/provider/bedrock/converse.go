package bedrock

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"

	"github.com/rnikadi/genaiopengateway/internal/id"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

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
	Tools          []chatTool      `json:"tools"`
	ToolChoice     json.RawMessage `json:"tool_choice"`
	N              *int            `json:"n"`
	Logprobs       *bool           `json:"logprobs"`
	ResponseFormat *struct {
		Type string `json:"type"`
	} `json:"response_format"`
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

// --- Converse shapes ---

type converseRequest struct {
	Messages        []message        `json:"messages"`
	System          []block          `json:"system,omitempty"`
	InferenceConfig *inferenceConfig `json:"inferenceConfig,omitempty"`
	ToolConfig      *toolConfig      `json:"toolConfig,omitempty"`
}

type message struct {
	Role    string  `json:"role"`
	Content []block `json:"content"`
}

type block struct {
	Text       *string     `json:"text,omitempty"`
	Image      *image      `json:"image,omitempty"`
	ToolUse    *toolUse    `json:"toolUse,omitempty"`
	ToolResult *toolResult `json:"toolResult,omitempty"`
}

type image struct {
	Format string `json:"format"`
	Source struct {
		Bytes string `json:"bytes"`
	} `json:"source"`
}

type toolUse struct {
	ToolUseID string          `json:"toolUseId"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
}

type toolResult struct {
	ToolUseID string  `json:"toolUseId"`
	Content   []block `json:"content"`
}

type inferenceConfig struct {
	MaxTokens     *int     `json:"maxTokens,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty"`
	TopP          *float64 `json:"topP,omitempty"`
	StopSequences []string `json:"stopSequences,omitempty"`
}

type toolConfig struct {
	Tools      []toolSpecWrapper `json:"tools"`
	ToolChoice map[string]any    `json:"toolChoice,omitempty"`
}

type toolSpecWrapper struct {
	ToolSpec struct {
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
		InputSchema struct {
			JSON json.RawMessage `json:"json"`
		} `json:"inputSchema"`
	} `json:"toolSpec"`
}

// invalid is a client error found while translating.
type invalid string

func (e invalid) Error() string { return string(e) }

func text(s string) block { return block{Text: &s} }

// translateRequest converts an OpenAI chat request into a Converse
// request. Unsupported features are errors, never silently dropped.
func translateRequest(cr *chatRequest) (*converseRequest, error) {
	switch {
	case cr.N != nil && *cr.N > 1:
		return nil, invalid("n greater than 1 is not supported for this model")
	case cr.Logprobs != nil && *cr.Logprobs:
		return nil, invalid("logprobs is not supported for this model")
	case cr.ResponseFormat != nil && cr.ResponseFormat.Type == "json_schema":
		return nil, invalid("response_format json_schema is not supported for this model")
	}
	out := &converseRequest{}
	ic := &inferenceConfig{Temperature: cr.Temperature, TopP: cr.TopP, MaxTokens: cr.MaxTokens}
	if cr.MaxCompletionTokens != nil {
		ic.MaxTokens = cr.MaxCompletionTokens
	}
	if len(cr.Stop) > 0 && string(cr.Stop) != "null" {
		var one string
		if json.Unmarshal(cr.Stop, &one) == nil {
			ic.StopSequences = []string{one}
		} else if json.Unmarshal(cr.Stop, &ic.StopSequences) != nil {
			return nil, invalid("stop must be a string or an array of strings")
		}
	}
	if ic.MaxTokens != nil || ic.Temperature != nil || ic.TopP != nil || ic.StopSequences != nil {
		out.InferenceConfig = ic
	}

	for i, m := range cr.Messages {
		switch m.Role {
		case "system", "developer":
			t, err := textOf(m.Content)
			if err != nil {
				return nil, invalid(fmt.Sprintf("messages[%d]: %v", i, err))
			}
			if t != "" {
				out.System = append(out.System, text(t))
			}
		case "user":
			blocks, err := userBlocks(m.Content)
			if err != nil {
				return nil, invalid(fmt.Sprintf("messages[%d]: %v", i, err))
			}
			out.Messages = appendMessage(out.Messages, "user", blocks)
		case "assistant":
			var blocks []block
			if t, err := textOf(m.Content); err != nil {
				return nil, invalid(fmt.Sprintf("messages[%d]: %v", i, err))
			} else if t != "" {
				blocks = append(blocks, text(t))
			}
			for _, tc := range m.ToolCalls {
				input := json.RawMessage(tc.Function.Arguments)
				if strings.TrimSpace(tc.Function.Arguments) == "" {
					input = json.RawMessage("{}")
				} else if !json.Valid(input) {
					return nil, invalid(fmt.Sprintf("messages[%d]: tool call arguments are not valid JSON", i))
				}
				blocks = append(blocks, block{ToolUse: &toolUse{ToolUseID: tc.ID, Name: tc.Function.Name, Input: input}})
			}
			if len(blocks) > 0 {
				out.Messages = appendMessage(out.Messages, "assistant", blocks)
			}
		case "tool":
			t, err := textOf(m.Content)
			if err != nil {
				return nil, invalid(fmt.Sprintf("messages[%d]: %v", i, err))
			}
			out.Messages = appendMessage(out.Messages, "user", []block{{ToolResult: &toolResult{ToolUseID: m.ToolCallID, Content: []block{text(t)}}}})
		default:
			return nil, invalid(fmt.Sprintf("messages[%d]: unsupported role %q", i, m.Role))
		}
	}

	if len(cr.Tools) > 0 {
		tc := &toolConfig{}
		for _, t := range cr.Tools {
			if t.Type != "function" {
				return nil, invalid(fmt.Sprintf("tool type %q is not supported", t.Type))
			}
			var w toolSpecWrapper
			w.ToolSpec.Name, w.ToolSpec.Description = t.Function.Name, t.Function.Description
			w.ToolSpec.InputSchema.JSON = t.Function.Parameters
			if len(w.ToolSpec.InputSchema.JSON) == 0 || string(w.ToolSpec.InputSchema.JSON) == "null" {
				w.ToolSpec.InputSchema.JSON = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			tc.Tools = append(tc.Tools, w)
		}
		out.ToolConfig = tc
	}
	if len(cr.ToolChoice) > 0 && string(cr.ToolChoice) != "null" {
		choice, err := toolChoice(cr.ToolChoice)
		if err != nil {
			return nil, err
		}
		if out.ToolConfig == nil {
			return nil, invalid("tool_choice needs tools")
		}
		out.ToolConfig.ToolChoice = choice
	}
	return out, nil
}

func toolChoice(raw json.RawMessage) (map[string]any, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto":
			return map[string]any{"auto": map[string]any{}}, nil
		case "required":
			return map[string]any{"any": map[string]any{}}, nil
		case "none":
			return nil, invalid(`tool_choice "none" is not supported for this model; send the request without tools`)
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
		return nil, invalid("tool_choice must be auto, required or a named function")
	}
	return map[string]any{"tool": map[string]any{"name": named.Function.Name}}, nil
}

// appendMessage merges consecutive messages of one role, since Converse
// requires roles to alternate.
func appendMessage(msgs []message, role string, blocks []block) []message {
	if n := len(msgs); n > 0 && msgs[n-1].Role == role {
		msgs[n-1].Content = append(msgs[n-1].Content, blocks...)
		return msgs
	}
	return append(msgs, message{Role: role, Content: blocks})
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

// imageFormats are the formats Converse takes, by media type.
var imageFormats = map[string]string{"image/png": "png", "image/jpeg": "jpeg", "image/gif": "gif", "image/webp": "webp"}

func userBlocks(raw json.RawMessage) ([]block, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []block{text(s)}, nil
	}
	var parts []chatPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("content must be a string or an array of parts")
	}
	var out []block
	for _, p := range parts {
		switch p.Type {
		case "text":
			out = append(out, text(p.Text))
		case "image_url":
			if p.ImageURL == nil {
				return nil, fmt.Errorf("image_url part has no url")
			}
			rest, ok := strings.CutPrefix(p.ImageURL.URL, "data:")
			if !ok {
				return nil, fmt.Errorf("images must be sent as base64 data URLs for this model")
			}
			meta, data, ok := strings.Cut(rest, ",")
			mediaType, isBase64 := strings.CutSuffix(meta, ";base64")
			format := imageFormats[mediaType]
			if !ok || !isBase64 || format == "" {
				return nil, fmt.Errorf("images must be base64 data URLs of type png, jpeg, gif or webp")
			}
			img := &image{Format: format}
			img.Source.Bytes = data
			out = append(out, block{Image: img})
		default:
			return nil, fmt.Errorf("content part type %q is not supported for this model", p.Type)
		}
	}
	return out, nil
}

// --- responses ---

type converseUsage struct {
	InputTokens           int64 `json:"inputTokens"`
	OutputTokens          int64 `json:"outputTokens"`
	CacheReadInputTokens  int64 `json:"cacheReadInputTokens"`
	CacheWriteInputTokens int64 `json:"cacheWriteInputTokens"`
}

// tokens counts cache reads and writes as input; cache writes are billed
// at the input price, as for Anthropic.
func (u converseUsage) tokens() usage.Tokens {
	return usage.Tokens{
		InputTokens:       u.InputTokens + u.CacheReadInputTokens + u.CacheWriteInputTokens,
		OutputTokens:      u.OutputTokens,
		CachedInputTokens: u.CacheReadInputTokens,
		Reported:          true,
	}
}

func chatUsage(u converseUsage) map[string]any {
	t := u.tokens()
	return map[string]any{
		"prompt_tokens":         t.InputTokens,
		"completion_tokens":     t.OutputTokens,
		"total_tokens":          t.Total(),
		"prompt_tokens_details": map[string]any{"cached_tokens": t.CachedInputTokens},
	}
}

type converseResponse struct {
	Output struct {
		Message struct {
			Content []struct {
				Text    *string  `json:"text"`
				ToolUse *toolUse `json:"toolUse"`
			} `json:"content"`
		} `json:"message"`
	} `json:"output"`
	StopReason string         `json:"stopReason"`
	Usage      *converseUsage `json:"usage"`
}

func finishReason(stop string) string {
	switch stop {
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "guardrail_intervened", "content_filtered":
		return "content_filter"
	}
	return "stop"
}

func translateResponse(cr *converseResponse, model string) map[string]any {
	var b strings.Builder
	var calls []map[string]any
	for _, c := range cr.Output.Message.Content {
		switch {
		case c.ToolUse != nil:
			args := string(c.ToolUse.Input)
			if args == "" || args == "null" {
				args = "{}"
			}
			calls = append(calls, map[string]any{"id": c.ToolUse.ToolUseID, "type": "function",
				"function": map[string]any{"name": c.ToolUse.Name, "arguments": args}})
		case c.Text != nil:
			b.WriteString(*c.Text)
		}
	}
	msg := map[string]any{"role": "assistant", "content": nil}
	if b.Len() > 0 {
		msg["content"] = b.String()
	}
	if len(calls) > 0 {
		msg["tool_calls"] = calls
	}
	out := map[string]any{
		"id": id.New("chatcmpl"), "object": "chat.completion", "created": time.Now().Unix(), "model": model,
		"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": finishReason(cr.StopReason)}},
	}
	if cr.Usage != nil {
		out["usage"] = chatUsage(*cr.Usage)
	}
	return out
}

// chat serves an OpenAI chat completion from Converse.
func (p *Provider) chat(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	var cr chatRequest
	if err := json.Unmarshal(req.Body, &cr); err != nil {
		return errorResponse(http.StatusBadRequest, "invalid_request_error", "request body is not a valid chat completion request"), nil
	}
	conv, err := translateRequest(&cr)
	if err != nil {
		return errorResponse(http.StatusBadRequest, "invalid_request_error", err.Error()), nil
	}
	body, err := json.Marshal(conv)
	if err != nil {
		return nil, err
	}
	action := "converse"
	if cr.Stream {
		action = "converse-stream"
	}
	resp, err := p.post(ctx, req.Model, action, body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return openAIError(resp), nil
	}
	resp.Header = http.Header{}
	if cr.Stream {
		resp.Header.Set("Content-Type", "text/event-stream")
		includeUsage := cr.StreamOptions != nil && cr.StreamOptions.IncludeUsage
		resp.Body = converseStream(resp.Body, resp.Usage, cr.Model, includeUsage)
		return resp, nil
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	var out converseResponse
	if err := json.Unmarshal(buf, &out); err != nil {
		return errorResponse(http.StatusBadGateway, "api_error", "upstream returned an unreadable response"), nil
	}
	if out.Usage != nil {
		*resp.Usage = out.Usage.tokens()
	}
	b, _ := json.Marshal(translateResponse(&out, cr.Model))
	resp.Header.Set("Content-Type", "application/json")
	resp.Body = io.NopCloser(bytes.NewReader(b))
	return resp, nil
}

// converseStream translates a ConverseStream event stream into OpenAI
// chat completion chunks. tokens is filled before the reader hits EOF.
func converseStream(upstream io.ReadCloser, tokens *usage.Tokens, model string, includeUsage bool) io.ReadCloser {
	return pipe(upstream, func(w io.Writer) error {
		t := &streamTranslator{w: w, id: id.New("chatcmpl"), model: model, created: time.Now().Unix(), tools: map[int]int{}}
		err := readFrames(upstream, t.frame)
		if t.usage != nil {
			*tokens = t.usage.tokens()
		}
		if err != nil || t.failed {
			return err
		}
		if t.stop != "" {
			reason := finishReason(t.stop)
			if err := t.delta(map[string]any{}, &reason); err != nil {
				return err
			}
		}
		if includeUsage && t.usage != nil {
			if err := t.chunk(map[string]any{"choices": []any{}, "usage": chatUsage(*t.usage)}); err != nil {
				return err
			}
		}
		_, err = io.WriteString(w, "data: [DONE]\n\n")
		return err
	})
}

type streamTranslator struct {
	w         io.Writer
	id, model string
	created   int64
	stop      string
	failed    bool
	usage     *converseUsage
	// tools maps content block indexes to OpenAI tool call indexes.
	tools map[int]int
}

func (t *streamTranslator) frame(m eventstream.Message) error {
	if e, ok := frameError(m); ok {
		t.failed = true
		b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": e.message, "type": e.typ, "code": e.typ}})
		_, err := fmt.Fprintf(t.w, "data: %s\n\n", b)
		return err
	}
	var ev struct {
		ContentBlockIndex int `json:"contentBlockIndex"`
		Start             struct {
			ToolUse *toolUse `json:"toolUse"`
		} `json:"start"`
		Delta struct {
			Text    *string `json:"text"`
			ToolUse *struct {
				Input string `json:"input"`
			} `json:"toolUse"`
		} `json:"delta"`
		StopReason string         `json:"stopReason"`
		Usage      *converseUsage `json:"usage"`
	}
	if json.Unmarshal(m.Payload, &ev) != nil {
		return nil
	}
	switch header(m, ":event-type") {
	case "messageStart":
		return t.delta(map[string]any{"role": "assistant", "content": ""}, nil)
	case "contentBlockStart":
		if tu := ev.Start.ToolUse; tu != nil {
			idx := len(t.tools)
			t.tools[ev.ContentBlockIndex] = idx
			return t.delta(map[string]any{"tool_calls": []any{map[string]any{
				"index": idx, "id": tu.ToolUseID, "type": "function",
				"function": map[string]any{"name": tu.Name, "arguments": ""},
			}}}, nil)
		}
	case "contentBlockDelta":
		switch {
		case ev.Delta.Text != nil:
			return t.delta(map[string]any{"content": *ev.Delta.Text}, nil)
		case ev.Delta.ToolUse != nil:
			idx, ok := t.tools[ev.ContentBlockIndex]
			if !ok {
				return nil
			}
			return t.delta(map[string]any{"tool_calls": []any{map[string]any{
				"index": idx, "function": map[string]any{"arguments": ev.Delta.ToolUse.Input},
			}}}, nil)
		}
	case "messageStop":
		t.stop = ev.StopReason
	case "metadata":
		t.usage = ev.Usage
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
