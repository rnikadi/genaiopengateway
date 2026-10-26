package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/alerts"
	"github.com/rnikadi/genaiopengateway/internal/auth"
	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/limits"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

// Alerts (docs/alerts.md): the server decides when to raise one, records
// it once in the store, and hands it to the notifier for webhooks.

// keyExpiryWarning is how long before a key expires its alert is raised.
const keyExpiryWarning = 7 * 24 * time.Hour

// raise records an alert and, if no replica raised it before, audits it
// and sends it to the webhooks that want it.
func (s *Server) raise(ctx context.Context, dedupe string, e *alerts.Event) {
	if s.store == nil {
		return
	}
	data, err := json.Marshal(e.Data)
	if err != nil {
		s.log.Error("encoding an alert failed", "event", e.Event, "err", err)
		return
	}
	a := &store.Alert{Event: e.Event, DedupeKey: dedupe, Data: data}
	if e.Org != nil {
		a.OrgID = e.Org.ID
	}
	fresh, err := s.store.RecordAlert(ctx, a)
	if err != nil {
		s.log.Error("recording an alert failed", "event", e.Event, "err", err)
		return
	}
	if !fresh {
		return
	}
	e.ID, e.Time = a.ID, a.Time
	s.metrics.ObserveAlert(e.Event)
	s.log.Info("alert raised", "alert_id", e.ID, "event", e.Event, "org_id", a.OrgID, "summary", e.Text)
	if err := s.store.AppendAudit(ctx, &store.AuditEntry{
		Actor: "gateway", Action: "alert.sent", ResourceType: "alert", ResourceID: e.ID, OrgID: a.OrgID, Details: data,
	}); err != nil {
		s.log.Error("writing audit entry failed", "action", "alert.sent", "err", err)
	}
	s.notifier.Send(s.webhooksFor(e), e)
}

// webhooksFor returns the configured webhooks that want an event.
func (s *Server) webhooksFor(e *alerts.Event) []config.Webhook {
	var orgs map[string]*store.Org
	if t := s.tenants.Load(); t != nil {
		orgs = t.orgs
	}
	var out []config.Webhook
	for _, w := range s.rt.Load().cfg.Alerts.Webhooks {
		if !w.Wants(e.Event) {
			continue
		}
		if w.Org != "" && (e.Org == nil || orgID(orgs, w.Org) != e.Org.ID) {
			continue
		}
		out = append(out, w)
	}
	return out
}

// refs names the organization, team and key of a principal for alerts.
func (s *Server) refs(p *auth.Principal) (org, team, key *alerts.Ref) {
	org = &alerts.Ref{ID: p.OrgID}
	if t := s.tenants.Load(); t != nil {
		if o := t.orgs[p.OrgID]; o != nil {
			org.Name = o.Name
		}
		if p.TeamID != "" {
			team = &alerts.Ref{ID: p.TeamID}
			if tm := t.teams[p.OrgID][p.TeamID]; tm != nil {
				team.Name = tm.Name
			}
		}
	}
	if p.KeyID != "" {
		key = &alerts.Ref{ID: p.KeyID, Name: p.KeyName}
	}
	return org, team, key
}

func refName(r *alerts.Ref) string {
	if r.Name != "" {
		return r.Name
	}
	return r.ID
}

var levelNames = [3]string{"org", "team", "key"}

// budgetCrossings returns the thresholds each charged budget just reached.
func budgetCrossings(spent []limits.Spend, defaults []float64) map[int][]float64 {
	out := map[int][]float64{}
	for i, sp := range spent {
		amount := sp.Budget.AmountUSD
		if amount <= 0 {
			continue
		}
		thresholds := defaults
		if sp.Budget.AlertAt != nil {
			thresholds = sp.Budget.AlertAt
		}
		for _, t := range thresholds {
			if line := t * amount; sp.Before < line && sp.After >= line {
				out[i] = append(out[i], t)
			}
		}
	}
	return out
}

// budgetAlerts raises an alert for each budget threshold a charge reached.
// The comparison is in memory; only a crossing touches the store, off the
// request path.
func (s *Server) budgetAlerts(ctx context.Context, p *auth.Principal, spent []limits.Spend) {
	crossings := budgetCrossings(spent, s.rt.Load().cfg.Alerts.BudgetThresholds)
	if len(crossings) == 0 {
		return
	}
	org, team, key := s.refs(p)
	go func() {
		for i, ts := range crossings {
			sp := spent[i]
			e := &alerts.Event{Event: config.EventBudgetThreshold, Org: org}
			// Name only the levels down to the one whose budget this is.
			path := refName(org)
			if sp.Level >= 1 && team != nil {
				e.Team = team
				path += " / " + refName(team)
			}
			if sp.Level == 2 && key != nil {
				e.Key = key
				path += " / key " + refName(key)
			}
			for _, t := range ts {
				e := *e
				e.Data = map[string]any{
					"scope": levelNames[sp.Level], "threshold": t,
					"spend_usd": roundCents(sp.After), "budget_usd": sp.Budget.AmountUSD,
					"period": sp.Budget.Period, "period_start": sp.PeriodStart,
				}
				e.Text = fmt.Sprintf("*%s* has spent $%.2f of its $%s %s budget (%d%%).",
					path, sp.After, strconv.FormatFloat(sp.Budget.AmountUSD, 'f', -1, 64), sp.Budget.Period, int(t*100+0.5))
				if t >= 1 {
					e.Text += " Requests are refused until the period ends or the budget is raised."
				}
				dedupe := fmt.Sprintf("%s:%s:%d:%g", e.Event, sp.ScopeID, sp.PeriodStart.Unix(), t)
				s.raise(ctx, dedupe, &e)
			}
		}
	}()
}

func roundCents(v float64) float64 { return float64(int64(v*100+0.5)) / 100 }

// toolPending is called by the MCP gateway when a pinned tool is hidden
// until approval.
func (s *Server) toolPending(server, tool, hash string, changed bool) {
	what := "is new"
	if changed {
		what = "changed its definition"
	}
	s.raise(context.Background(), fmt.Sprintf("%s:%s/%s:%s", config.EventMCPToolPending, server, tool, hash), &alerts.Event{
		Event: config.EventMCPToolPending,
		Data:  map[string]any{"server": server, "tool": tool, "hash": hash, "changed": changed},
		Text:  fmt.Sprintf("MCP tool *%s/%s* %s and is hidden until an owner approves it.", server, tool, what),
	})
}

// keyExpiryLoop raises an alert for keys that expire within a week.
func (s *Server) keyExpiryLoop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		s.checkKeyExpiry(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) checkKeyExpiry(ctx context.Context, now time.Time) {
	s.mu.Lock()
	var expiring []*auth.Principal
	for _, p := range s.storeKeys {
		if !p.Previous && p.ExpiresAt != nil && p.ExpiresAt.After(now) && p.ExpiresAt.Sub(now) <= keyExpiryWarning {
			expiring = append(expiring, p)
		}
	}
	s.mu.Unlock()
	for _, p := range expiring {
		org, team, key := s.refs(p)
		path := refName(org)
		if team != nil {
			path += " / " + refName(team)
		}
		data, owner := map[string]any{"expires_at": p.ExpiresAt.UTC()}, ""
		if p.Owner != "" {
			data["owner"], owner = p.Owner, " (owner: "+p.Owner+")"
		}
		s.raise(ctx, fmt.Sprintf("%s:%s:%d", config.EventKeyExpiring, p.KeyID, p.ExpiresAt.Unix()), &alerts.Event{
			Event: config.EventKeyExpiring, Org: org, Team: team, Key: key,
			Data: data,
			Text: fmt.Sprintf("Key *%s*%s of %s expires on %s.", refName(key), owner, path, p.ExpiresAt.UTC().Format("2006-01-02 15:04 MST")),
		})
	}
}

// --- admin API ---

func (s *Server) listAlerts(w http.ResponseWriter, r *http.Request) error {
	pg, err := page(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	if err := read(r, q.Get("org_id")); err != nil {
		return err
	}
	items, more, err := s.store.ListAlerts(r.Context(), q.Get("org_id"), q.Get("event"), pg)
	if err != nil {
		return err
	}
	return writeList(w, items, more)
}

// testAlert sends a test event to one webhook and reports the result.
func (s *Server) testAlert(w http.ResponseWriter, r *http.Request) error {
	if err := requireOwner(r); err != nil {
		return err
	}
	var body struct {
		Webhook string `json:"webhook"`
	}
	if err := decode(r, &body); err != nil {
		return err
	}
	for _, h := range s.rt.Load().cfg.Alerts.Webhooks {
		if h.Name != body.Webhook {
			continue
		}
		e := &alerts.Event{
			ID: "alert_test", Event: "test", Time: time.Now().UTC(), Data: map[string]any{},
			Text: "Test alert from GenAIOpenGateway. Alerts to this webhook work.",
		}
		status, err := s.notifier.Deliver(r.Context(), h, e)
		out := map[string]any{"webhook": h.Name, "status": status, "ok": err == nil}
		if err != nil {
			out["error"] = err.Error()
		}
		writeJSON(w, http.StatusOK, out)
		return nil
	}
	return badRequest("no webhook is named %q", body.Webhook)
}
