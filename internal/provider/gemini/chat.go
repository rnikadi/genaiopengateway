package gemini

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
	Seed                *int64          `json:"seed"`
	Stream              bool            `json:"stream"`
	StreamOptions       *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	Tools           []chatTool      `json:"tools"`
	ToolChoice      json.RawMessage `json:"tool_choice"`
	N               *int            `json:"n"`
	Logprobs        *bool           `json:"logprobs"`
	ReasoningEffort string          `json:"reasoning_effort"`
	ResponseFormat  *struct {
		Type       string `json:"type"`
		JSONSchema *struct {
			Schema json.RawMessage `json:"schema"`
		} `json:"json_schema"`
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
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
	ExtraContent *extraContent `json:"extra_content,omitempty"`
}

// extraContent carries Gemini's thought signature on a tool call, as
// Google's own OpenAI compatibility layer does.
type extraContent struct {
	Google struct {
		ThoughtSignature string `json:"thought_signature,omitempty"`
	} `json:"google"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// --- Gemini shapes ---

type genRequest struct {
	Contents          []content         `json:"contents"`
	SystemInstruction *content          `json:"systemInstruction,omitempty"`
	Tools             []genTool         `json:"tools,omitempty"`
	ToolConfig        *toolConfig       `json:"toolConfig,omitempty"`
	GenerationConfig  *generationConfig `json:"generationConfig,omitempty"`
}

type content struct {
	Role  string `json:"role,omitempty"`
	Parts []part `json:"parts"`
}

type part struct {
	Text             *string           `json:"text,omitempty"`
	Thought          bool              `json:"thought,omitempty"`
	ThoughtSignature string            `json:"thoughtSignature,omitempty"`
	InlineData       *blob             `json:"inlineData,omitempty"`
	FunctionCall     *functionCall     `json:"functionCall,omitempty"`
	FunctionResponse *functionResponse `json:"functionResponse,omitempty"`
}

type blob struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type functionCall struct {
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type functionResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

type genTool struct {
	FunctionDeclarations []functionDeclaration `json:"functionDeclarations"`
}

type functionDeclaration struct {
	Name                 string          `json:"name"`
	Description          string          `json:"description,omitempty"`
	ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema,omitempty"`
}

type toolConfig struct {
	FunctionCallingConfig struct {
		Mode                 string   `json:"mode"`
		AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
	} `json:"functionCallingConfig"`
}

type generationConfig struct {
	MaxOutputTokens    *int            `json:"maxOutputTokens,omitempty"`
	Temperature        *float64        `json:"temperature,omitempty"`
	TopP               *float64        `json:"topP,omitempty"`
	StopSequences      []string        `json:"stopSequences,omitempty"`
	Seed               *int64          `json:"seed,omitempty"`
	ResponseMimeType   string          `json:"responseMimeType,omitempty"`
	ResponseJSONSchema json.RawMessage `json:"responseJsonSchema,omitempty"`
	ThinkingConfig     *struct {
		ThinkingLevel string `json:"thinkingLevel"`
	} `json:"thinkingConfig,omitempty"`
}

// invalid is a client error found while translating.
type invalid string

func (e invalid) Error() string { return string(e) }

func textPart(s string) part { return part{Text: &s} }

// translateRequest converts an OpenAI chat request into a generateContent
// request. Unsupported features are errors, never silently dropped.
func translateRequest(cr *chatRequest) (*genRequest, error) {
	switch {
	case cr.N != nil && *cr.N > 1:
		return nil, invalid("n greater than 1 is not supported for this model")
	case cr.Logprobs != nil && *cr.Logprobs:
		return nil, invalid("logprobs is not supported for this model")
	}
	gr := &genRequest{}
	gc := &generationConfig{Temperature: cr.Temperature, TopP: cr.TopP, Seed: cr.Seed, MaxOutputTokens: cr.MaxTokens}
	if cr.MaxCompletionTokens != nil {
		gc.MaxOutputTokens = cr.MaxCompletionTokens
	}
	if len(cr.Stop) > 0 && string(cr.Stop) != "null" {
		var one string
		if json.Unmarshal(cr.Stop, &one) == nil {
			gc.StopSequences = []string{one}
		} else if json.Unmarshal(cr.Stop, &gc.StopSequences) != nil {
			return nil, invalid("stop must be a string or an array of strings")
		}
	}
	if rf := cr.ResponseFormat; rf != nil {
		switch rf.Type {
		case "", "text":
		case "json_object":
			gc.ResponseMimeType = "application/json"
		case "json_schema":
			gc.ResponseMimeType = "application/json"
			if rf.JSONSchema != nil {
				gc.ResponseJSONSchema = rf.JSONSchema.Schema
			}
		default:
			return nil, invalid(fmt.Sprintf("response_format type %q is not supported", rf.Type))
		}
	}
	switch cr.ReasoningEffort {
	case "":
	case "minimal", "low", "medium", "high":
		gc.ThinkingConfig = &struct {
			ThinkingLevel string `json:"thinkingLevel"`
		}{cr.ReasoningEffort}
	default:
		return nil, invalid(fmt.Sprintf("reasoning_effort %q is not supported for this model", cr.ReasoningEffort))
	}
	gr.GenerationConfig = gc

	// Gemini's function responses carry the function name, which OpenAI
	// tool messages only reference by call id.
	names := map[string]string{}
	var system []part
	for i, m := range cr.Messages {
		switch m.Role {
		case "system", "developer":
			text, err := textOf(m.Content)
			if err != nil {
				return nil, invalid(fmt.Sprintf("messages[%d]: %v", i, err))
			}
			if text != "" {
				system = append(system, textPart(text))
			}
		case "user":
			parts, err := userParts(m.Content)
			if err != nil {
				return nil, invalid(fmt.Sprintf("messages[%d]: %v", i, err))
			}
			gr.Contents = appendContent(gr.Contents, "user", parts)
		case "assistant":
			var parts []part
			if text, err := textOf(m.Content); err != nil {
				return nil, invalid(fmt.Sprintf("messages[%d]: %v", i, err))
			} else if text != "" {
				parts = append(parts, textPart(text))
			}
			for _, tc := range m.ToolCalls {
				args := json.RawMessage(tc.Function.Arguments)
				if strings.TrimSpace(tc.Function.Arguments) == "" {
					args = json.RawMessage("{}")
				} else if !json.Valid(args) {
					return nil, invalid(fmt.Sprintf("messages[%d]: tool call arguments are not valid JSON", i))
				}
				p := part{FunctionCall: &functionCall{Name: tc.Function.Name, Args: args}}
				if tc.ExtraContent != nil {
					p.ThoughtSignature = tc.ExtraContent.Google.ThoughtSignature
				}
				parts = append(parts, p)
				names[tc.ID] = tc.Function.Name
			}
			if len(parts) > 0 {
				gr.Contents = appendContent(gr.Contents, "model", parts)
			}
		case "tool":
			name, ok := names[m.ToolCallID]
			if !ok {
				return nil, invalid(fmt.Sprintf("messages[%d]: tool_call_id %q matches no earlier tool call", i, m.ToolCallID))
			}
			text, err := textOf(m.Content)
			if err != nil {
				return nil, invalid(fmt.Sprintf("messages[%d]: %v", i, err))
			}
			gr.Contents = appendContent(gr.Contents, "user", []part{{FunctionResponse: &functionResponse{Name: name, Response: responseObject(text)}}})
		default:
			return nil, invalid(fmt.Sprintf("messages[%d]: unsupported role %q", i, m.Role))
		}
	}
	if len(system) > 0 {
		gr.SystemInstruction = &content{Parts: system}
	}

	if len(cr.Tools) > 0 {
		var decls []functionDeclaration
		for _, t := range cr.Tools {
			if t.Type != "function" {
				return nil, invalid(fmt.Sprintf("tool type %q is not supported", t.Type))
			}
			d := functionDeclaration{Name: t.Function.Name, Description: t.Function.Description}
			if len(t.Function.Parameters) > 0 && string(t.Function.Parameters) != "null" {
				d.ParametersJSONSchema = t.Function.Parameters
			}
			decls = append(decls, d)
		}
		gr.Tools = []genTool{{FunctionDeclarations: decls}}
	}
	if len(cr.ToolChoice) > 0 && string(cr.ToolChoice) != "null" {
		tc, err := toolChoice(cr.ToolChoice)
		if err != nil {
			return nil, err
		}
		gr.ToolConfig = tc
	}
	return gr, nil
}

// responseObject wraps a tool result as the JSON object Gemini expects:
// the result itself when it is an object, otherwise under "result".
func responseObject(text string) json.RawMessage {
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(text), &obj) == nil && obj != nil {
		return json.RawMessage(text)
	}
	b, _ := json.Marshal(map[string]string{"result": text})
	return b
}

// appendContent merges consecutive contents of one role.
func appendContent(cs []content, role string, parts []part) []content {
	if n := len(cs); n > 0 && cs[n-1].Role == role {
		cs[n-1].Parts = append(cs[n-1].Parts, parts...)
		return cs
	}
	return append(cs, content{Role: role, Parts: parts})
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

func userParts(raw json.RawMessage) ([]part, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []part{textPart(s)}, nil
	}
	var parts []chatPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("content must be a string or an array of parts")
	}
	var out []part
	for _, p := range parts {
		switch p.Type {
		case "text":
			out = append(out, textPart(p.Text))
		case "image_url":
			if p.ImageURL == nil {
				return nil, fmt.Errorf("image_url part has no url")
			}
			b, err := dataURL(p.ImageURL.URL)
			if err != nil {
				return nil, err
			}
			out = append(out, part{InlineData: b})
		default:
			return nil, fmt.Errorf("content part type %q is not supported for this model", p.Type)
		}
	}
	return out, nil
}

// dataURL converts a base64 data URL. The gateway does not fetch image
// URLs for clients.
func dataURL(u string) (*blob, error) {
	rest, ok := strings.CutPrefix(u, "data:")
	if !ok {
		return nil, fmt.Errorf("images must be sent as base64 data URLs for this model")
	}
	meta, data, ok := strings.Cut(rest, ",")
	mediaType, isBase64 := strings.CutSuffix(meta, ";base64")
	if !ok || !isBase64 || mediaType == "" {
		return nil, fmt.Errorf("image data URLs must be base64 encoded with a media type")
	}
	return &blob{MimeType: mediaType, Data: data}, nil
}

func toolChoice(raw json.RawMessage) (*toolConfig, error) {
	tc := &toolConfig{}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto":
			tc.FunctionCallingConfig.Mode = "AUTO"
		case "none":
			tc.FunctionCallingConfig.Mode = "NONE"
		case "required":
			tc.FunctionCallingConfig.Mode = "ANY"
		default:
			return nil, invalid(fmt.Sprintf("tool_choice %q is not supported", s))
		}
		return tc, nil
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
	tc.FunctionCallingConfig.Mode = "ANY"
	tc.FunctionCallingConfig.AllowedFunctionNames = []string{named.Function.Name}
	return tc, nil
}

// --- responses ---

type genResponse struct {
	Candidates []struct {
		Content struct {
			Parts []part `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	UsageMetadata *usageMetadata `json:"usageMetadata"`
	ModelVersion  string         `json:"modelVersion"`
	ResponseID    string         `json:"responseId"`
}

// finishReason maps Gemini's finish reason. Reasons from safety filters
// become content_filter.
func finishReason(reason string, toolCalls bool) string {
	switch reason {
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY":
		return "content_filter"
	}
	if toolCalls {
		return "tool_calls"
	}
	return "stop"
}

func chatUsage(u usageMetadata) map[string]any {
	t := u.tokens()
	return map[string]any{
		"prompt_tokens":             t.InputTokens,
		"completion_tokens":         t.OutputTokens,
		"total_tokens":              t.Total(),
		"prompt_tokens_details":     map[string]any{"cached_tokens": t.CachedInputTokens},
		"completion_tokens_details": map[string]any{"reasoning_tokens": t.ReasoningTokens},
	}
}

// toolCall converts a function call part. Gemini may omit call ids, so
// the gateway makes one.
func toolCall(p part) map[string]any {
	fc := p.FunctionCall
	callID := fc.ID
	if callID == "" {
		callID = id.New("call")
	}
	args := string(fc.Args)
	if args == "" || args == "null" {
		args = "{}"
	}
	tc := map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": fc.Name, "arguments": args}}
	if p.ThoughtSignature != "" {
		tc["extra_content"] = map[string]any{"google": map[string]any{"thought_signature": p.ThoughtSignature}}
	}
	return tc
}

func translateResponse(gr *genResponse, model string) map[string]any {
	var text strings.Builder
	var calls []map[string]any
	reason := ""
	if len(gr.Candidates) > 0 {
		c := gr.Candidates[0]
		reason = c.FinishReason
		for _, p := range c.Content.Parts {
			switch {
			case p.Thought:
			case p.FunctionCall != nil:
				calls = append(calls, toolCall(p))
			case p.Text != nil:
				text.WriteString(*p.Text)
			}
		}
	}
	msg := map[string]any{"role": "assistant", "content": nil}
	if text.Len() > 0 {
		msg["content"] = text.String()
	}
	if len(calls) > 0 {
		msg["tool_calls"] = calls
	}
	if gr.ModelVersion != "" {
		model = gr.ModelVersion
	}
	out := map[string]any{
		"id": completionID(gr.ResponseID), "object": "chat.completion", "created": time.Now().Unix(), "model": model,
		"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": finishReason(reason, len(calls) > 0)}},
	}
	if gr.UsageMetadata != nil {
		out["usage"] = chatUsage(*gr.UsageMetadata)
	}
	return out
}

func completionID(responseID string) string {
	if responseID == "" {
		return id.New("chatcmpl")
	}
	return "chatcmpl-" + responseID
}

// chat serves an OpenAI chat completion from generateContent.
func (p *Provider) chat(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	var cr chatRequest
	if err := json.Unmarshal(req.Body, &cr); err != nil {
		return errorResponse(http.StatusBadRequest, "invalid_request_error", "request body is not a valid chat completion request"), nil
	}
	gr, err := translateRequest(&cr)
	if err != nil {
		return errorResponse(http.StatusBadRequest, "invalid_request_error", err.Error()), nil
	}
	body, err := json.Marshal(gr)
	if err != nil {
		return nil, err
	}
	resp, err := p.post(ctx, req.Model, body, cr.Stream)
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
		resp.Body = translateStream(resp.Body, resp.Usage, cr.Model, includeUsage)
		return resp, nil
	}
	buf, err := readAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var out genResponse
	if err := json.Unmarshal(buf, &out); err != nil {
		return errorResponse(http.StatusBadGateway, "api_error", "upstream returned an unreadable response"), nil
	}
	if out.UsageMetadata != nil {
		*resp.Usage = out.UsageMetadata.tokens()
	}
	if len(out.Candidates) == 0 && out.PromptFeedback != nil && out.PromptFeedback.BlockReason != "" {
		blocked := errorResponse(http.StatusBadRequest, "content_filter", "the prompt was blocked by the provider's safety filter: "+out.PromptFeedback.BlockReason)
		blocked.Usage = resp.Usage
		return blocked, nil
	}
	b, _ := json.Marshal(translateResponse(&out, cr.Model))
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

// translateError converts a Google error body to the OpenAI shape,
// keeping the status so fallback rules still apply.
func translateError(resp *provider.Response) (*provider.Response, error) {
	buf, _ := readAll(resp.Body)
	var e googleError
	typ := "api_error"
	if json.Unmarshal(buf, &e) != nil || e.Error.Message == "" {
		e.Error.Message = strings.TrimSpace(string(buf))
	} else if e.Error.Status != "" {
		typ = strings.ToLower(e.Error.Status)
	}
	out := errorResponse(resp.StatusCode, typ, e.Error.Message)
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		out.Header.Set("Retry-After", ra)
	}
	return out, nil
}

// translateStream converts generateContent events into OpenAI chat
// completion chunks. tokens is filled before the returned reader hits EOF.
func translateStream(upstream io.ReadCloser, tokens *usage.Tokens, model string, includeUsage bool) io.ReadCloser {
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer upstream.Close()
		t := &streamTranslator{w: pw, model: model, created: time.Now().Unix()}
		err := t.run(upstream)
		if t.usage != nil {
			*tokens = t.usage.tokens()
		}
		if err == nil && t.started && !t.failed {
			reason := finishReason(t.reason, t.tools > 0)
			err = t.delta(map[string]any{}, &reason)
		}
		if err == nil && includeUsage && t.usage != nil {
			err = t.chunk(map[string]any{"choices": []any{}, "usage": chatUsage(*t.usage)})
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
	started   bool
	failed    bool
	reason    string
	tools     int
	usage     *usageMetadata
}

func (t *streamTranslator) run(r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		data, ok := bytes.CutPrefix(sc.Bytes(), []byte("data:"))
		if !ok {
			continue
		}
		if err := t.event(bytes.TrimSpace(data)); err != nil {
			return err
		}
	}
	return sc.Err()
}

func (t *streamTranslator) event(data []byte) error {
	var ev struct {
		genResponse
		googleError
	}
	if json.Unmarshal(data, &ev) != nil {
		return nil
	}
	if ev.Error.Message != "" {
		t.failed = true
		typ := strings.ToLower(ev.Error.Status)
		if typ == "" {
			typ = "api_error"
		}
		b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": ev.Error.Message, "type": typ, "code": typ}})
		_, err := fmt.Fprintf(t.w, "data: %s\n\n", b)
		return err
	}
	if ev.UsageMetadata != nil {
		t.usage = ev.UsageMetadata
	}
	if !t.started {
		t.started = true
		t.id = completionID(ev.ResponseID)
		if ev.ModelVersion != "" {
			t.model = ev.ModelVersion
		}
		if err := t.delta(map[string]any{"role": "assistant", "content": ""}, nil); err != nil {
			return err
		}
	}
	if len(ev.Candidates) == 0 {
		if ev.PromptFeedback != nil && ev.PromptFeedback.BlockReason != "" {
			t.reason = "SAFETY"
		}
		return nil
	}
	c := ev.Candidates[0]
	if c.FinishReason != "" {
		t.reason = c.FinishReason
	}
	for _, p := range c.Content.Parts {
		var err error
		switch {
		case p.Thought:
		case p.FunctionCall != nil:
			tc := toolCall(p)
			tc["index"] = t.tools
			t.tools++
			err = t.delta(map[string]any{"tool_calls": []any{tc}}, nil)
		case p.Text != nil && *p.Text != "":
			err = t.delta(map[string]any{"content": *p.Text}, nil)
		}
		if err != nil {
			return err
		}
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
