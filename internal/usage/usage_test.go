package usage

import (
	"math"
	"testing"

	"github.com/rnikadi/genaiopengateway/internal/config"
)

func TestFromOpenAI(t *testing.T) {
	cases := map[string]struct {
		body string
		want Tokens
	}{
		"chat": {
			`{"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":40},"completion_tokens_details":{"reasoning_tokens":5}}}`,
			Tokens{InputTokens: 100, OutputTokens: 20, CachedInputTokens: 40, ReasoningTokens: 5, Reported: true},
		},
		"embeddings": {
			`{"data":[],"usage":{"prompt_tokens":8,"total_tokens":8}}`,
			Tokens{InputTokens: 8, Reported: true},
		},
		"responses object": {
			`{"usage":{"input_tokens":50,"output_tokens":10,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":2}}}`,
			Tokens{InputTokens: 50, OutputTokens: 10, CachedInputTokens: 3, ReasoningTokens: 2, Reported: true},
		},
		"responses completed event": {
			`{"type":"response.completed","response":{"usage":{"input_tokens":7,"output_tokens":1}}}`,
			Tokens{InputTokens: 7, OutputTokens: 1, Reported: true},
		},
	}
	for name, c := range cases {
		got, ok := FromOpenAI([]byte(c.body))
		if !ok || got != c.want {
			t.Errorf("%s: got %+v %v, want %+v", name, got, ok, c.want)
		}
	}
	for _, body := range []string{`{"choices":[]}`, `[DONE]`, `{"usage":null}`} {
		if _, ok := FromOpenAI([]byte(body)); ok {
			t.Errorf("%s: expected no usage", body)
		}
	}
}

func TestCost(t *testing.T) {
	p := config.Price{InputPerMTok: 2, OutputPerMTok: 10, CachedInputPerMTok: 0.5}
	got := Cost(Tokens{InputTokens: 1_000_000, CachedInputTokens: 400_000, OutputTokens: 100_000}, p)
	// 600k * 2 + 400k * 0.5 + 100k * 10 = 1.2 + 0.2 + 1.0
	if math.Abs(got-2.4) > 1e-9 {
		t.Fatalf("cost = %v, want 2.4", got)
	}
	noCachedPrice := Cost(Tokens{InputTokens: 10, CachedInputTokens: 10}, config.Price{InputPerMTok: 1e6})
	if noCachedPrice != 10 {
		t.Fatalf("cached tokens without a cached price should use the input price, got %v", noCachedPrice)
	}
}

func TestCostAudioTokens(t *testing.T) {
	tok := Tokens{InputTokens: 1_000_000, CachedInputTokens: 100_000, OutputTokens: 500_000, AudioInputTokens: 600_000, AudioOutputTokens: 400_000}
	// Without audio prices, audio tokens cost as text tokens.
	if got := Cost(tok, config.Price{InputPerMTok: 4, OutputPerMTok: 16}); math.Abs(got-(1*4+0.5*16)) > 1e-9 {
		t.Fatalf("text-priced cost = %v", got)
	}
	// 300k text in at 4, 100k cached at 4, 600k audio in at 32, 100k text
	// out at 16 and 400k audio out at 64.
	got := Cost(tok, config.Price{InputPerMTok: 4, OutputPerMTok: 16, AudioInputPerMTok: 32, AudioOutputPerMTok: 64})
	if want := 0.3*4 + 0.1*4 + 0.6*32 + 0.1*16 + 0.4*64; math.Abs(got-want) > 1e-9 {
		t.Fatalf("cost = %v, want %v", got, want)
	}
}
