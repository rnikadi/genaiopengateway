package store

import (
	"context"
	"time"
)

// UsageExportRow is one day's usage of one upstream model by one team
// on one endpoint: the grain of the cost exports (docs/chargeback.md).
type UsageExportRow struct {
	Day           time.Time
	OrgID, TeamID string
	Provider      string
	UpstreamModel string
	Model         string
	Endpoint      string
	UsageRow
}

// UsageExport aggregates usage between From (inclusive) and To
// (exclusive) by UTC day, team, upstream model and endpoint. GroupBy is
// ignored.
func (s *Store) UsageExport(ctx context.Context, uq UsageQuery) ([]UsageExportRow, error) {
	src, args := s.usageSource(uq)
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT day AS d, org_id, team_id, provider, upstream_model, model, endpoint, `+usageSums+`
		FROM `+src+`
		GROUP BY d, org_id, team_id, provider, upstream_model, model, endpoint
		ORDER BY d, org_id, team_id, provider, upstream_model, model, endpoint`), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageExportRow{}
	for rows.Next() {
		var r UsageExportRow
		var day int64
		if err := rows.Scan(&day, &r.OrgID, &r.TeamID, &r.Provider, &r.UpstreamModel, &r.Model, &r.Endpoint,
			&r.Requests, &r.InputTokens, &r.OutputTokens, &r.CachedInputTokens, &r.ReasoningTokens,
			&r.Images, &r.Characters, &r.AudioSeconds, &r.VideoSeconds, &r.CostUSD, &r.CacheHits, &r.SavedUSD); err != nil {
			return nil, err
		}
		r.Day = toTime(day * 86400000)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ScopeSpend is what one team of an organization spent. Usage of keys
// without a team has an empty TeamID.
type ScopeSpend struct {
	OrgID, TeamID string
	CostUSD       float64
}

// SpendBetween sums cost per organization and team between from
// (inclusive) and to (exclusive), for forecasts (docs/forecast.md).
func (s *Store) SpendBetween(ctx context.Context, from, to time.Time) ([]ScopeSpend, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT org_id, team_id, COALESCE(SUM(cost_usd), 0) FROM usage
		WHERE ts >= ? AND ts < ? GROUP BY org_id, team_id`), from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScopeSpend
	for rows.Next() {
		var sp ScopeSpend
		if err := rows.Scan(&sp.OrgID, &sp.TeamID, &sp.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}
