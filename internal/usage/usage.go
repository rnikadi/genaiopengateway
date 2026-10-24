// Package usage measures what a request consumed and what it cost.
package usage

import (
	"encoding/json"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
)

// Tokens counts what one request consumed: tokens, and for media
// endpoints the units they are billed by (docs/modalities.md).
// InputTokens includes CachedInputTokens; OutputTokens includes
// ReasoningTokens.
type Tokens struct {
	InputTokens       int64 `json:"input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
	ReasoningTokens   int64 `json:"reasoning_tokens"`
	// Images generated or edited.
	Images int64 `json:"images"`
	// Characters of text turned into speech.
	Characters int64 `json:"characters"`
	// AudioSeconds of audio transcribed or translated.
	AudioSeconds float64 `json:"audio_seconds"`
	// VideoSeconds of video requested.
	VideoSeconds float64 `json:"video_seconds"`
	// Reported is false when the upstream returned no usage at all.
	Reported bool `json:"-"`
}

// Media reports whether any media unit was counted.
func (t Tokens) Media() bool {
	return t.Images > 0 || t.Characters > 0 || t.AudioSeconds > 0 || t.VideoSeconds > 0
}

// Total returns input plus output tokens.
func (t Tokens) Total() int64 { return t.InputTokens + t.OutputTokens }

// Cost returns the USD cost of one request that consumed t, at price p.
// Cached input tokens use the cached price when one is set, otherwise
// the full input price.
func Cost(t Tokens, p config.Price) float64 {
	cachedPrice := p.CachedInputPerMTok
	if cachedPrice == 0 {
		cachedPrice = p.InputPerMTok
	}
	uncached := t.InputTokens - t.CachedInputTokens
	if uncached < 0 {
		uncached = 0
	}
	return (float64(uncached)*p.InputPerMTok+
		float64(t.CachedInputTokens)*cachedPrice+
		float64(t.OutputTokens)*p.OutputPerMTok+
		float64(t.Characters)*p.PerMChars)/1e6 +
		float64(t.Images)*p.PerImage +
		t.AudioSeconds/60*p.PerAudioMinute +
		t.VideoSeconds*p.PerVideoSecond +
		p.PerRequest
}

// openAIUsage covers the usage objects of chat completions, embeddings
// and responses, which name the same counts differently.
type openAIUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	InputTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func (u *openAIUsage) tokens() Tokens {
	return Tokens{
		InputTokens:       u.PromptTokens + u.InputTokens,
		OutputTokens:      u.CompletionTokens + u.OutputTokens,
		CachedInputTokens: u.PromptTokensDetails.CachedTokens + u.InputTokensDetails.CachedTokens,
		ReasoningTokens:   u.CompletionTokensDetails.ReasoningTokens + u.OutputTokensDetails.ReasoningTokens,
		Reported:          true,
	}
}

// FromOpenAI extracts usage from an OpenAI-style JSON body or stream
// event: a top-level "usage" object (chat, embeddings, chat stream
// chunks) or "response.usage" (Responses API objects and events).
func FromOpenAI(body []byte) (Tokens, bool) {
	var v struct {
		Usage    *openAIUsage `json:"usage"`
		Response *struct {
			Usage *openAIUsage `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(body, &v) != nil {
		return Tokens{}, false
	}
	switch {
	case v.Usage != nil:
		return v.Usage.tokens(), true
	case v.Response != nil && v.Response.Usage != nil:
		return v.Response.Usage.tokens(), true
	}
	return Tokens{}, false
}

// Record is the stored account of one request.
type Record struct {
	RequestID     string
	Time          time.Time
	OrgID         string
	TeamID        string
	KeyID         string
	Endpoint      string
	Model         string
	Provider      string
	UpstreamModel string
	Status        int
	LatencyMS     int64
	Tokens        Tokens
	CostUSD       float64
}
