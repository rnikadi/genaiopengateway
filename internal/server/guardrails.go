package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/auth"
	"github.com/rnikadi/genaiopengateway/internal/bodylog"
	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/guardrail"
	"github.com/rnikadi/genaiopengateway/internal/id"
	"github.com/rnikadi/genaiopengateway/internal/mcpgw"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/store"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

// Guardrails (docs/guardrails.md): handlers run the guardrails a
// request's policies and virtual server name, record what acted, and
// block or redact.

const codeGuardrailBlocked = "guardrail_blocked"

// guardrailsHeader lists the guardrails that acted on a model call.
const guardrailsHeader = "X-Gog-Guardrails"

// maxCheckedResponse bounds a response held back for output guardrails.
const maxCheckedResponse = 16 << 20

// maxClassifierReply bounds what is read of a classifier's reply.
const maxClassifierReply = 1 << 20

func (s *Server) newGuardrails(cfg *config.Config) (*guardrail.Set, error) {
	client := &http.Client{
		Transport: s.client.Transport,
		// A redirect could send the texts somewhere else.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return guardrail.New(cfg.Guardrails, client, mcpgw.Version, s.classify, s.metrics.ObserveGuardrail)
}

func guardMeta(requestID string, p *auth.Principal) guardrail.Meta {
	return guardrail.Meta{RequestID: requestID, OrgID: p.OrgID, TeamID: p.TeamID, KeyID: p.KeyID, Value: p}
}

// recordGuardrails stores what the guardrails did, never the text.
func (s *Server) recordGuardrails(meta guardrail.Meta, decisions []guardrail.Decision) {
	if s.guardEvents == nil {
		return
	}
	for _, d := range decisions {
		e := store.GuardrailEvent{
			ID: id.New("gev"), RequestID: meta.RequestID, Time: time.Now().UTC(),
			OrgID: meta.OrgID, TeamID: meta.TeamID, KeyID: meta.KeyID,
			Guardrail: d.Guardrail, Stage: d.Stage, Action: d.Action, Failed: d.Failed, Reason: d.Reason,
			LatencyMS: d.Latency.Milliseconds(),
		}
		if len(d.Counts) > 0 {
			e.Counts, _ = json.Marshal(d.Counts)
		}
		s.guardEvents.Add(e)
	}
}

// guardrailSummary is the value of the guardrails header.
func guardrailSummary(decisions []guardrail.Decision) string {
	parts := make([]string, 0, len(decisions))
	for _, d := range decisions {
		parts = append(parts, d.Guardrail+"="+d.Action)
	}
	return strings.Join(parts, ", ")
}

func blockedMessage(d *guardrail.Decision) string {
	msg := "blocked by guardrail " + d.Guardrail
	if d.Reason != "" {
		msg += ": " + d.Reason
	}
	return msg
}

// guardRequest runs input guardrails on a model request. It returns the
// body to send, which differs from raw only when a guardrail redacted.
func (s *Server) guardRequest(ctx context.Context, rt *runtime, meta guardrail.Meta, names []string, raw []byte) ([]byte, guardrail.Result) {
	if !rt.guards.Applies(config.StageInput, names) {
		return raw, guardrail.Result{}
	}
	doc, err := guardrail.ParseDoc(raw)
	if err != nil {
		return raw, guardrail.Result{}
	}
	res := rt.guards.Run(ctx, config.StageInput, names, doc.Texts(), meta)
	s.recordGuardrails(meta, res.Decisions)
	if !res.Changed || res.Blocked != nil {
		return raw, res
	}
	doc.SetTexts(res.Texts)
	return doc.JSON(), res
}

// guardResponse runs output guardrails on a whole response. It returns
// the body to send: the original, or for a redaction the redacted
// response, as an event stream when the original was one.
func (s *Server) guardResponse(ctx context.Context, rt *runtime, meta guardrail.Meta, names []string, body []byte, contentType string) ([]byte, guardrail.Result) {
	streamed := strings.HasPrefix(contentType, "text/event-stream")
	if !streamed && strings.HasPrefix(contentType, "text/") {
		// A transcript as text, SRT or WebVTT: the whole body is the text.
		res := rt.guards.Run(ctx, config.StageOutput, names, []string{string(body)}, meta)
		s.recordGuardrails(meta, res.Decisions)
		if res.Changed && res.Blocked == nil {
			return []byte(res.Texts[0]), res
		}
		return body, res
	}
	assembled := body
	if streamed {
		assembled = bodylog.Assemble(contentType, body)
	}
	doc, err := guardrail.ParseDoc(assembled)
	if err != nil {
		if !streamed {
			return body, guardrail.Result{}
		}
		// A stream the gateway cannot read must not pass unchecked.
		d := guardrail.Decision{Guardrail: rt.guards.Names(config.StageOutput, names)[0], Stage: config.StageOutput,
			Action: config.ActionBlock, Reason: "the stream could not be read"}
		s.recordGuardrails(meta, []guardrail.Decision{d})
		return body, guardrail.Result{Blocked: &d, Decisions: []guardrail.Decision{d}}
	}
	res := rt.guards.Run(ctx, config.StageOutput, names, doc.Texts(), meta)
	s.recordGuardrails(meta, res.Decisions)
	if !res.Changed || res.Blocked != nil {
		return body, res
	}
	doc.SetTexts(res.Texts)
	out := doc.JSON()
	if streamed {
		if ev, ok := bodylog.Stream(out); ok {
			return ev, res
		}
		// A stream the gateway cannot rebuild must not leak what was
		// redacted.
		res.Blocked = &guardrail.Decision{Guardrail: res.Decisions[len(res.Decisions)-1].Guardrail, Stage: config.StageOutput,
			Action: config.ActionBlock, Reason: "the redacted stream could not be rebuilt"}
		return body, res
	}
	return out, res
}

// errResponseTooLarge is returned when a response is too large to hold
// back for output guardrails; it is blocked.
var errResponseTooLarge = errors.New("response is too large to check")

func readChecked(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxCheckedResponse+1))
	if err == nil && len(b) > maxCheckedResponse {
		err = errResponseTooLarge
	}
	return b, err
}

// classify asks a classifier model, through the gateway's own routing,
// and meters the call under the requesting key.
func (s *Server) classify(ctx context.Context, meta guardrail.Meta, model, system, text string) (string, error) {
	rt := s.rt.Load()
	type message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	var msgs []message
	if system != "" {
		msgs = append(msgs, message{"system", system})
	}
	msgs = append(msgs, message{"user", text})
	m, _ := json.Marshal(model)
	ms, _ := json.Marshal(msgs)
	body := map[string]json.RawMessage{"model": m, "messages": ms}
	start := time.Now()
	res, err := rt.router.Route(ctx, provider.ChatCompletions, model, body, provider.Request{})
	if err != nil {
		return "", err
	}
	resp := res.Response
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxClassifierReply))
	_ = resp.Body.Close()
	if err != nil {
		return "", err
	}
	if p, ok := meta.Value.(*auth.Principal); ok && resp.StatusCode == http.StatusOK && resp.Usage != nil {
		cost := usage.Cost(*resp.Usage, res.Target.Price)
		if _, err := s.limiter.Charge(context.WithoutCancel(ctx), p, resp.Usage.Total(), cost); err != nil {
			s.log.Error("charging limits for a classifier failed", "request_id", meta.RequestID, "err", err)
		}
		if s.usage != nil {
			s.usage.Add(usage.Record{
				RequestID: meta.RequestID, Time: start, OrgID: p.OrgID, TeamID: p.TeamID, KeyID: p.KeyID,
				Endpoint: string(provider.ChatCompletions), Model: model, Provider: res.Target.Provider, UpstreamModel: res.Target.Model,
				Status: resp.StatusCode, LatencyMS: time.Since(start).Milliseconds(), Tokens: *resp.Usage, CostUSD: cost,
			})
		}
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("classifier model answered %d", resp.StatusCode)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(raw, &out) != nil || len(out.Choices) == 0 {
		return "", errors.New("classifier reply has no message")
	}
	return out.Choices[0].Message.Content, nil
}

// guardTool serves mcpgw.Options.Guard: the guardrails of the caller's
// policies and of the virtual server, on a tool call's text.
func (s *Server) guardTool(ctx context.Context, caller *mcpgw.Caller, virtualServer, stage string, texts []string) ([]string, bool, string) {
	rt := s.rt.Load()
	names := rt.cfg.MCP.VirtualServers[virtualServer].Guardrails
	var meta guardrail.Meta
	if caller != nil {
		p := caller.Principal
		names = append(slices.Clone(names), p.Guardrails()...)
		meta = guardMeta(caller.RequestID, p)
	}
	if !rt.guards.Applies(stage, names) {
		return texts, false, ""
	}
	res := rt.guards.Run(ctx, stage, names, texts, meta)
	s.recordGuardrails(meta, res.Decisions)
	if res.Blocked != nil {
		return texts, false, "The gateway " + blockedMessage(res.Blocked) + "."
	}
	return res.Texts, res.Changed, ""
}

// listGuardrailEvents serves GET /admin/v1/guardrail-events.
func (s *Server) listGuardrailEvents(w http.ResponseWriter, r *http.Request) error {
	pg, err := page(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	if err := s.readScope(r, q.Get("org_id"), q.Get("team_id"), q.Get("key_id")); err != nil {
		return err
	}
	gq := store.GuardrailEventQuery{
		OrgID: q.Get("org_id"), TeamID: q.Get("team_id"), KeyID: q.Get("key_id"),
		Guardrail: q.Get("guardrail"), Action: q.Get("action"), RequestID: q.Get("request_id"),
	}
	for name, dst := range map[string]*time.Time{"from": &gq.From, "to": &gq.To} {
		if v := q.Get(name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return badRequest("%s must be an RFC 3339 time", name)
			}
			*dst = t
		}
	}
	items, more, err := s.store.ListGuardrailEvents(r.Context(), gq, pg)
	if err != nil {
		return err
	}
	return writeList(w, items, more)
}
