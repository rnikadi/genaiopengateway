package guardrail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/alerts"
	"github.com/rnikadi/genaiopengateway/internal/config"
)

// webhook asks an operator's service, signing the body like alert
// webhooks.
type webhook struct {
	name, url, secret string
	client            *http.Client
	version           string
}

type webhookRequest struct {
	Guardrail string   `json:"guardrail"`
	Stage     string   `json:"stage"`
	RequestID string   `json:"request_id,omitempty"`
	OrgID     string   `json:"org_id,omitempty"`
	TeamID    string   `json:"team_id,omitempty"`
	KeyID     string   `json:"key_id,omitempty"`
	Texts     []string `json:"texts"`
}

type webhookResponse struct {
	Action string   `json:"action"`
	Reason string   `json:"reason"`
	Texts  []string `json:"texts"`
}

// maxWebhookResponse bounds a reply, which carries redacted texts.
const maxWebhookResponse = 32 << 20

func (w *webhook) check(ctx context.Context, meta Meta, stage string, texts []string) (verdict, error) {
	body, err := json.Marshal(webhookRequest{
		Guardrail: w.name, Stage: stage, RequestID: meta.RequestID,
		OrgID: meta.OrgID, TeamID: meta.TeamID, KeyID: meta.KeyID, Texts: texts,
	})
	if err != nil {
		return verdict{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return verdict{}, errors.New("invalid url")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "GenAIOpenGateway/"+w.version)
	if w.secret != "" {
		req.Header.Set(alerts.SignatureHeader, alerts.Sign(w.secret, time.Now(), body))
	}
	resp, err := w.client.Do(req)
	if err != nil {
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err // without the URL
		}
		return verdict{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return verdict{}, fmt.Errorf("status %d", resp.StatusCode)
	}
	var r webhookResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxWebhookResponse)).Decode(&r); err != nil {
		return verdict{}, errors.New("reply is not valid JSON")
	}
	v := verdict{reason: truncate(r.Reason, maxReason)}
	switch r.Action {
	case "allow":
		return verdict{}, nil
	case "block":
		v.match, v.action = true, config.ActionBlock
	case "redact":
		if len(r.Texts) != len(texts) {
			return verdict{}, fmt.Errorf("redact reply has %d texts, want %d", len(r.Texts), len(texts))
		}
		v.match, v.action, v.texts = true, config.ActionRedact, r.Texts
	default:
		return verdict{}, fmt.Errorf("unknown action %q", r.Action)
	}
	return v, nil
}
