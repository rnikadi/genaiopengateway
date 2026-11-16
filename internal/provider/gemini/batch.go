package gemini

import (
	"encoding/json"
	"errors"

	"github.com/rnikadi/genaiopengateway/internal/usage"
)

// ChatToGenerate translates an OpenAI chat completion request into a
// generateContent request carrying labels, for callers that send it
// some other way than Send does, such as a Vertex AI batch job. An
// error is the client's to fix.
func ChatToGenerate(body []byte, labels map[string]string) (json.RawMessage, error) {
	var cr chatRequest
	if err := json.Unmarshal(body, &cr); err != nil {
		return nil, errors.New("not a valid chat completion request")
	}
	gr, err := translateRequest(&cr)
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		*genRequest
		Labels map[string]string `json:"labels,omitempty"`
	}{gr, labels})
}

// GenerateToChat translates a generateContent response into a chat
// completion for model. blocked is set instead when a safety filter
// refused the prompt.
func GenerateToChat(resp []byte, model string) (chat map[string]any, tokens usage.Tokens, blocked string, err error) {
	var out genResponse
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, usage.Tokens{}, "", err
	}
	if out.UsageMetadata != nil {
		tokens = out.UsageMetadata.tokens()
	}
	if len(out.Candidates) == 0 && out.PromptFeedback != nil && out.PromptFeedback.BlockReason != "" {
		return nil, tokens, "the prompt was blocked by the provider's safety filter: " + out.PromptFeedback.BlockReason, nil
	}
	return translateResponse(&out, model), tokens, "", nil
}
