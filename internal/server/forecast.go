package server

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/alerts"
	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/limits"
	"github.com/rnikadi/genaiopengateway/internal/policy"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

// Spend forecasts and anomaly alerts (docs/forecast.md). A forecast
// extends the last week's daily spend to the end of a monthly budget's
// period; an anomaly is a day that costs several times what the two
// weeks before it did.

const (
	// forecastWindow is how far back the daily rate is measured.
	forecastWindow = 7 * 24 * time.Hour
	// anomalyBaseline is the period a day's spend is compared with.
	anomalyBaseline = 14 * 24 * time.Hour
	day             = 24 * time.Hour
)

// forecast is where a monthly budget is heading.
type forecast struct {
	Scope     string  `json:"scope"` // org or team
	OrgID     string  `json:"org_id"`
	OrgName   string  `json:"org_name"`
	TeamID    string  `json:"team_id,omitempty"`
	TeamName  string  `json:"team_name,omitempty"`
	BudgetUSD float64 `json:"budget_usd"`
	// SpentUSD is the spend since the period started, and DailyUSD the
	// average per day over the last week.
	SpentUSD     float64   `json:"spent_usd"`
	DailyUSD     float64   `json:"daily_usd"`
	ProjectedUSD float64   `json:"projected_usd"`
	PeriodStart  time.Time `json:"period_start"`
	PeriodEnd    time.Time `json:"period_end"`
	// ExhaustsAt is when the budget runs out at the current rate, when
	// that is before the period ends.
	ExhaustsAt *time.Time `json:"exhausts_at,omitempty"`
	Exhausted  bool       `json:"exhausted,omitempty"`
}

// spendMaps sums spend per organization and per team.
func spendMaps(rows []store.ScopeSpend) (orgs, teams map[string]float64) {
	orgs, teams = map[string]float64{}, map[string]float64{}
	for _, r := range rows {
		orgs[r.OrgID] += r.CostUSD
		if r.TeamID != "" {
			teams[r.TeamID] += r.CostUSD
		}
	}
	return orgs, teams
}

// project fills in a forecast's projection from its spend and rate.
func (f *forecast) project(now time.Time) {
	remaining := f.PeriodEnd.Sub(now).Hours() / 24
	f.ProjectedUSD = roundCents(f.SpentUSD + f.DailyUSD*remaining)
	switch {
	case f.SpentUSD >= f.BudgetUSD:
		f.Exhausted = true
	case f.DailyUSD > 0:
		at := now.Add(time.Duration((f.BudgetUSD - f.SpentUSD) / f.DailyUSD * float64(day)))
		if at.Before(f.PeriodEnd) {
			at = at.UTC().Truncate(time.Hour)
			f.ExhaustsAt = &at
		}
	}
	f.SpentUSD, f.DailyUSD = roundCents(f.SpentUSD), roundCents(f.DailyUSD)
}

// forecasts projects every monthly budget of an organization or team,
// those closest to running out first.
func (s *Server) forecasts(ctx context.Context, now time.Time) ([]forecast, error) {
	t := s.tenants.Load()
	if t == nil {
		return nil, nil
	}
	now = now.UTC()
	start := limits.PeriodStart(policy.PeriodMonthly, now)
	end := start.AddDate(0, 1, 0)
	month, err := s.store.SpendBetween(ctx, start, now.Add(time.Millisecond))
	if err != nil {
		return nil, err
	}
	week, err := s.store.SpendBetween(ctx, now.Add(-forecastWindow), now.Add(time.Millisecond))
	if err != nil {
		return nil, err
	}
	orgSpent, teamSpent := spendMaps(month)
	orgWeek, teamWeek := spendMaps(week)
	monthly := func(b *policy.Budget) bool {
		return b != nil && b.AmountUSD > 0 && b.Period == policy.PeriodMonthly
	}
	out := []forecast{}
	for key, o := range t.orgs {
		if key != o.ID {
			continue
		}
		if monthly(o.Budget) {
			f := forecast{Scope: "org", OrgID: o.ID, OrgName: o.Name, BudgetUSD: o.Budget.AmountUSD,
				SpentUSD: orgSpent[o.ID], DailyUSD: orgWeek[o.ID] / 7, PeriodStart: start, PeriodEnd: end}
			f.project(now)
			out = append(out, f)
		}
		for key, tm := range t.teams[o.ID] {
			if key == tm.ID && monthly(tm.Budget) { // teams are listed by name too
				f := forecast{Scope: "team", OrgID: o.ID, OrgName: o.Name, TeamID: tm.ID, TeamName: tm.Name, BudgetUSD: tm.Budget.AmountUSD,
					SpentUSD: teamSpent[tm.ID], DailyUSD: teamWeek[tm.ID] / 7, PeriodStart: start, PeriodEnd: end}
				f.project(now)
				out = append(out, f)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ar, br := a.ProjectedUSD/a.BudgetUSD, b.ProjectedUSD/b.BudgetUSD; ar != br {
			return ar > br
		}
		return a.OrgID+a.TeamID < b.OrgID+b.TeamID
	})
	return out, nil
}

func (f *forecast) refs() (org, team *alerts.Ref, path string) {
	org = &alerts.Ref{ID: f.OrgID, Name: f.OrgName}
	path = refName(org)
	if f.TeamID != "" {
		team = &alerts.Ref{ID: f.TeamID, Name: f.TeamName}
		path += " / " + refName(team)
	}
	return org, team, path
}

// forecastLoop checks forecasts and anomalies hourly.
func (s *Server) forecastLoop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		s.checkSpend(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// checkSpend raises budget.forecast once per budget and period when the
// budget is set to run out early, and spend.anomaly once per team and
// day when the last 24 hours cost far more than usual.
func (s *Server) checkSpend(ctx context.Context, now time.Time) {
	if s.store == nil {
		return
	}
	now = now.UTC()
	fs, err := s.forecasts(ctx, now)
	if err != nil {
		s.log.Error("forecasting spend failed", "err", err)
		return
	}
	for i := range fs {
		f := &fs[i]
		if f.ExhaustsAt == nil {
			continue
		}
		org, team, path := f.refs()
		id := f.OrgID
		if f.TeamID != "" {
			id = f.TeamID
		}
		s.raise(ctx, fmt.Sprintf("%s:%s:%d", config.EventBudgetForecast, id, f.PeriodStart.Unix()), &alerts.Event{
			Event: config.EventBudgetForecast, Org: org, Team: team,
			Data: map[string]any{
				"scope": f.Scope, "budget_usd": f.BudgetUSD, "spend_usd": f.SpentUSD, "daily_usd": f.DailyUSD,
				"projected_usd": f.ProjectedUSD, "exhausts_at": f.ExhaustsAt, "period": policy.PeriodMonthly, "period_start": f.PeriodStart,
			},
			Text: fmt.Sprintf("*%s* is spending $%.2f a day and will reach its $%.2f monthly budget around %s, before the month ends. It has spent $%.2f so far.",
				path, f.DailyUSD, f.BudgetUSD, f.ExhaustsAt.Format("2 Jan 15:04 MST"), f.SpentUSD),
		})
	}

	an := s.rt.Load().cfg.Alerts.SpendAnomaly
	if an.Factor <= 0 {
		return
	}
	last, err := s.store.SpendBetween(ctx, now.Add(-day), now.Add(time.Millisecond))
	if err != nil {
		s.log.Error("reading recent spend failed", "err", err)
		return
	}
	before, err := s.store.SpendBetween(ctx, now.Add(-day-anomalyBaseline), now.Add(-day))
	if err != nil {
		s.log.Error("reading baseline spend failed", "err", err)
		return
	}
	t := s.tenants.Load()
	if t == nil {
		return
	}
	usual := map[[2]string]float64{}
	for _, b := range before {
		usual[[2]string{b.OrgID, b.TeamID}] = b.CostUSD / (anomalyBaseline.Hours() / 24)
	}
	for _, l := range last {
		base := usual[[2]string{l.OrgID, l.TeamID}]
		// A team with no history has no usual day to compare with.
		if base <= 0 || l.CostUSD < an.MinUSD || l.CostUSD < an.Factor*base {
			continue
		}
		org := &alerts.Ref{ID: l.OrgID}
		if o := t.orgs[l.OrgID]; o != nil {
			org.Name = o.Name
		}
		path, id := refName(org), l.OrgID
		var team *alerts.Ref
		if l.TeamID != "" {
			team = &alerts.Ref{ID: l.TeamID}
			if tm := t.teams[l.OrgID][l.TeamID]; tm != nil {
				team.Name = tm.Name
			}
			path += " / " + refName(team)
			id = l.TeamID
		}
		s.raise(ctx, fmt.Sprintf("%s:%s:%s", config.EventSpendAnomaly, id, now.Format("2006-01-02")), &alerts.Event{
			Event: config.EventSpendAnomaly, Org: org, Team: team,
			Data: map[string]any{
				"spend_24h_usd": roundCents(l.CostUSD), "usual_daily_usd": roundCents(base),
				"factor": float64(int64(l.CostUSD/base*10+0.5)) / 10, "threshold_factor": an.Factor,
			},
			Text: fmt.Sprintf("*%s* spent $%.2f in the last 24 hours, %.1f times its usual $%.2f a day.", path, l.CostUSD, l.CostUSD/base, base),
		})
	}
}

// listForecasts returns the forecast of each monthly budget the caller
// may see usage of.
func (s *Server) listForecasts(w http.ResponseWriter, r *http.Request) error {
	orgID := r.URL.Query().Get("org_id")
	if err := s.readScope(r, config.PermUsageRead, orgID, "", ""); err != nil {
		return err
	}
	all, err := s.forecasts(r.Context(), time.Now())
	if err != nil {
		return err
	}
	out := []forecast{}
	for _, f := range all {
		if orgID == "" || f.OrgID == orgID {
			out = append(out, f)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
	return nil
}
