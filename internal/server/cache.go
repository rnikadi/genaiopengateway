package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/auth"
	"github.com/rnikadi/genaiopengateway/internal/cache"
	"github.com/rnikadi/genaiopengateway/internal/guardrail"
	"github.com/rnikadi/genaiopengateway/internal/policy"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/store"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

// cacheHeader tells the client whether the answer came from the response
// cache: hit, semantic or miss.
const cacheHeader = "X-Gog-Cache"

// cacheEmbeddings is the endpoint semantic lookups are metered as.
const cacheEmbeddings = "cache.embeddings"

// cachePlan is how one request uses the response cache (docs/cache.md).
type cachePlan struct {
	key    string        // the exact entry's key
	ttl    time.Duration // how long a stored answer is reused
	lookup bool          // false for Cache-Control: no-cache
	// semantic is set for a semantic lookup: the context the question
	// is matched in, and the question.
	semantic bool
	ctxKey   string
	question string
	vector   []float32 // the question's embedding, once computed
}

// planCache returns how a request uses the cache, or nil when it does
// not: the caller's policy turns it off, the endpoint is not cached or
// the client sent Cache-Control: no-store.
func planCache(r *http.Request, p *auth.Principal, ep provider.Endpoint, model string, guards []string, raw []byte) *cachePlan {
	c := p.Cache()
	if c.Mode == "" || c.Mode == policy.CacheOff {
		return nil
	}
	switch ep {
	case provider.ChatCompletions, provider.Messages, provider.Embeddings:
	default:
		return nil
	}
	cc := strings.ToLower(strings.Join(r.Header.Values("Cache-Control"), ","))
	if strings.Contains(cc, "no-store") {
		return nil
	}
	org, team := cacheScope(p)
	parts := []string{string(ep), model, strings.Join(guards, ",")}
	key, err := cache.Key(org, team, raw, parts...)
	if err != nil {
		return nil
	}
	plan := &cachePlan{key: key, ttl: c.Duration(), lookup: !strings.Contains(cc, "no-cache")}
	if c.Mode == policy.CacheSemantic && ep != provider.Embeddings {
		if q, rest, ok := cache.Question(raw); ok {
			if plan.ctxKey, err = cache.Key(org, team, rest, parts...); err == nil {
				plan.semantic, plan.question = true, q
			}
		}
	}
	return plan
}

// cacheScope is the organization and team a principal's entries belong
// to. Static keys share a team, so each key gets its own entries.
func cacheScope(p *auth.Principal) (string, string) {
	if p.OrgID == auth.StaticOrgID {
		return p.OrgID, p.TeamID + "." + p.KeyID
	}
	return p.OrgID, p.TeamID
}

// cacheLookup returns a stored answer for the plan and whether it was a
// semantic match, or nil on a miss. Store errors are misses.
func (s *Server) cacheLookup(ctx context.Context, rt *runtime, p *auth.Principal, requestID string, plan *cachePlan) (*cache.Entry, bool) {
	if !plan.lookup {
		return nil, false
	}
	if e, err := s.cache.Get(ctx, plan.key); err != nil {
		s.log.Warn("cache lookup failed", "request_id", requestID, "err", err)
		return nil, false
	} else if e != nil {
		return e, false
	}
	if !plan.semantic {
		return nil, false
	}
	v, err := s.embedQuestion(ctx, rt, p, requestID, plan.question)
	if err != nil {
		s.log.Warn("embedding the question for the cache failed", "request_id", requestID, "err", err)
		return nil, false
	}
	plan.vector = v
	key, _, ok := s.cacheIndex.Nearest(plan.ctxKey, v, rt.cfg.Cache.Semantic.Threshold)
	if !ok {
		return nil, false
	}
	e, err := s.cache.Get(ctx, key)
	if err != nil || e == nil {
		return nil, false
	}
	return e, true
}

// cacheStore keeps a successful answer and, for a semantic plan, its
// question's embedding.
func (s *Server) cacheStore(ctx context.Context, plan *cachePlan, requestID string, e *cache.Entry) {
	if err := s.cache.Set(ctx, plan.key, e, plan.ttl); err != nil {
		s.log.Warn("cache store failed", "request_id", requestID, "err", err)
		return
	}
	if plan.semantic && plan.vector != nil {
		s.cacheIndex.Add(plan.ctxKey, plan.key, plan.vector, plan.ttl)
	}
}

// embedQuestion embeds a question with cache.semantic.model, metered to
// the caller as endpoint cache.embeddings.
func (s *Server) embedQuestion(ctx context.Context, rt *runtime, p *auth.Principal, requestID, question string) ([]float32, error) {
	model := rt.cfg.Cache.Semantic.Model
	start := time.Now()
	body := map[string]json.RawMessage{}
	body["model"], _ = json.Marshal(model)
	body["input"], _ = json.Marshal(question)
	res, err := rt.router.Route(ctx, provider.Embeddings, model, body, provider.Request{})
	if err != nil {
		return nil, err
	}
	resp := res.Response
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	// Its own request id keeps it a separate row in the Requests list.
	rec := usage.Record{
		RequestID: requestID + ":cache", Time: start, OrgID: p.OrgID, TeamID: p.TeamID, KeyID: p.KeyID,
		Endpoint: cacheEmbeddings, Model: model, Provider: res.Target.Provider, UpstreamModel: res.Target.Model,
		Status: resp.StatusCode, LatencyMS: time.Since(start).Milliseconds(),
	}
	if resp.StatusCode == http.StatusOK {
		rec.Tokens = *resp.Usage
		rec.CostUSD = usage.Cost(rec.Tokens, res.Target.Price)
		spent, err := s.limiter.Charge(context.WithoutCancel(ctx), p, rec.Tokens.Total(), rec.CostUSD)
		if err != nil {
			s.log.Error("charging limits failed", "request_id", requestID, "err", err)
		}
		s.budgetAlerts(context.WithoutCancel(ctx), p, spent)
	}
	if s.usage != nil {
		s.usage.Add(rec)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("the embedding model answered " + http.StatusText(resp.StatusCode))
	}
	var out struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Data) == 0 || len(out.Data[0].Embedding) == 0 {
		return nil, errors.New("the embedding model returned no embedding")
	}
	return out.Data[0].Embedding, nil
}

// clearCache removes an organization's or a team's cached answers.
func (s *Server) clearCache(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	q := r.URL.Query()
	orgID, teamID := q.Get("org_id"), q.Get("team_id")
	if orgID == "" {
		return &apiError{http.StatusBadRequest, "invalid_request", "org_id is required"}
	}
	if _, err := s.store.GetOrg(ctx, orgID); err != nil {
		return err
	}
	if teamID != "" {
		t, err := s.store.GetTeam(ctx, teamID)
		if err != nil {
			return err
		}
		if t.OrgID != orgID {
			return store.ErrNotFound
		}
	}
	if err := write(r, orgID); err != nil {
		return err
	}
	prefix := cache.Prefix(orgID, teamID)
	n, err := s.cache.Clear(ctx, prefix)
	if err != nil {
		return err
	}
	s.cacheIndex.Clear(prefix)
	if err := s.audit(ctx, "cache.clear", "cache", prefix, orgID, map[string]any{"team_id": teamID, "entries": n}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"cleared": n})
	return nil
}

// serveCached answers a request from the cache. A hit is recorded with
// no tokens and no cost, and what the answer first cost as saved.
func (s *Server) serveCached(w http.ResponseWriter, rec *requestLog, p *auth.Principal, ep provider.Endpoint, e *cache.Entry, semantic bool,
	acted []guardrail.Decision, start time.Time, raw []byte) {
	rec.Cache, rec.SavedUSD = "hit", e.CostUSD
	if semantic {
		rec.Cache = "semantic"
	}
	rec.Provider, rec.UpstreamModel = e.Provider, e.Model
	rec.Target, rec.Status = e.Provider+"/"+e.Model, http.StatusOK
	if e.ContentType != "" {
		w.Header().Set("Content-Type", e.ContentType)
	}
	w.Header().Set("X-Gateway-Target", rec.Target)
	w.Header().Set(cacheHeader, rec.Cache)
	setGuardrailsHeader(w, acted)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(e.Body)
	if capture := s.newCapture(p); capture != nil {
		_, _ = capture.Write(e.Body)
		s.logModelBody(rec, p, ep, start, raw, capture, e.ContentType)
	}
	if s.usage != nil {
		s.usage.Add(usage.Record{
			RequestID: rec.RequestID, Time: start, OrgID: p.OrgID, TeamID: p.TeamID, KeyID: p.KeyID,
			Endpoint: string(ep), Model: rec.Model, Provider: e.Provider, UpstreamModel: e.Model,
			Status: http.StatusOK, LatencyMS: time.Since(start).Milliseconds(), CacheHit: true, SavedUSD: e.CostUSD,
		})
	}
}
