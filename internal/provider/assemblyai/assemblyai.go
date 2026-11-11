// Package assemblyai adapts the AssemblyAI API: asynchronous speech to
// text, served as OpenAI's synchronous transcriptions endpoint.
package assemblyai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/provider/native"
)

// pollInterval is how often a queued transcript is checked.
var pollInterval = time.Second

// Provider is an AssemblyAI upstream.
type Provider struct{ c native.Client }

// New returns an adapter for cfg.
func New(cfg config.Provider, client *http.Client) *Provider {
	return &Provider{c: native.Client{Cfg: cfg, HTTP: client, Auth: func(h http.Header) {
		if key := cfg.APIKey.Value(); key != "" {
			h.Set("Authorization", key)
		}
	}}}
}

// Supports implements provider.Provider.
func (p *Provider) Supports(e provider.Endpoint) bool { return e == provider.Transcriptions }

// transcript is AssemblyAI's transcript object.
type transcript struct {
	ID       string  `json:"id"`
	Status   string  `json:"status"`
	Error    string  `json:"error"`
	Text     string  `json:"text"`
	Language string  `json:"language_code"`
	Duration float64 `json:"audio_duration"`
	Words    []struct {
		Text  string `json:"text"`
		Start int64  `json:"start"`
		End   int64  `json:"end"`
	} `json:"words"`
}

// Send implements provider.Provider. It uploads the file, starts a
// transcript, waits for it within the provider's timeout, and deletes
// it from AssemblyAI once read.
func (p *Provider) Send(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	if req.Endpoint != provider.Transcriptions {
		return native.Error(http.StatusNotFound, "invalid_request_error", "AssemblyAI does not serve "+string(req.Endpoint)), nil
	}
	f := native.Fields(req.Body)
	if bad := native.CheckTranscriptFormat(f["response_format"]); bad != nil {
		return bad, nil
	}
	file := native.File(req.Files, "file")
	if file == nil {
		return native.BadRequest("file is required"), nil
	}
	ctx, cancel := context.WithTimeout(ctx, p.c.Cfg.Timeout.Std())
	defer cancel()

	resp, buf, err := p.c.Do(ctx, http.MethodPost, "/v2/upload", file.Data, "application/octet-stream")
	if err != nil {
		return nil, err
	}
	if bad := native.Failed(resp, buf); bad != nil {
		return bad, nil
	}
	var up struct {
		URL string `json:"upload_url"`
	}
	if json.Unmarshal(buf, &up) != nil || up.URL == "" {
		return native.BadGateway("the upstream did not return an upload URL"), nil
	}

	start := map[string]any{"audio_url": up.URL}
	if req.Model != "" && req.Model != "default" {
		start["speech_model"] = req.Model
	}
	if f["language"] != "" {
		start["language_code"] = f["language"]
	} else {
		start["language_detection"] = true
	}
	if f["prompt"] != "" {
		start["prompt"] = f["prompt"]
	}
	b, _ := json.Marshal(start)
	resp, buf, err = p.c.Do(ctx, http.MethodPost, "/v2/transcript", b, "application/json")
	if err != nil {
		return nil, err
	}
	if bad := native.Failed(resp, buf); bad != nil {
		return bad, nil
	}
	var tr transcript
	if json.Unmarshal(buf, &tr) != nil || tr.ID == "" {
		return native.BadGateway("the upstream did not return a transcript"), nil
	}
	path := "/v2/transcript/" + url.PathEscape(tr.ID)
	defer func() {
		// Delete the transcript and its audio, which AssemblyAI would
		// otherwise keep. Best effort, on a context of its own so a
		// timed out request still cleans up.
		dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer dcancel()
		_, _, _ = p.c.Do(dctx, http.MethodDelete, path, nil, "")
	}()
	for tr.Status != "completed" {
		if tr.Status == "error" {
			return native.Error(http.StatusBadRequest, "invalid_request_error", "AssemblyAI could not transcribe the file: "+tr.Error), nil
		}
		select {
		case <-ctx.Done():
			return native.Error(http.StatusGatewayTimeout, "timeout", "the transcript was not ready within the provider's timeout"), nil
		case <-time.After(pollInterval):
		}
		resp, buf, err = p.c.Do(ctx, http.MethodGet, path, nil, "")
		if err != nil {
			if ctx.Err() != nil {
				continue
			}
			return nil, err
		}
		if bad := native.Failed(resp, buf); bad != nil {
			return bad, nil
		}
		if json.Unmarshal(buf, &tr) != nil {
			return native.BadGateway("the upstream answer is not an AssemblyAI transcript"), nil
		}
	}
	t := native.Transcript{Text: tr.Text, Language: tr.Language, Duration: tr.Duration}
	for _, w := range tr.Words {
		t.Words = append(t.Words, native.Word{Word: w.Text, Start: float64(w.Start) / 1000, End: float64(w.End) / 1000})
	}
	return t.Respond(f["response_format"]), nil
}
