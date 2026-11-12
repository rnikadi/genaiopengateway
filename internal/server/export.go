package server

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/store"
)

// Cost exports for finance (docs/chargeback.md): a chargeback statement
// per team, and the same usage in the columns of the FinOps FOCUS
// specification, which cost tools import.

// publishers names who makes the models of a provider type, for the
// FOCUS PublisherName column. Types that front many publishers, such as
// openai (any compatible server) and bedrock, keep the provider's name.
var publishers = map[string]string{
	"anthropic": "Anthropic", "gemini": "Google", "vertex": "Google", "cohere": "Cohere",
	"elevenlabs": "ElevenLabs", "deepgram": "Deepgram", "assemblyai": "AssemblyAI", "stability": "Stability AI", "fal": "fal",
}

var focusColumns = []string{
	"BilledCost", "EffectiveCost", "ListCost", "ContractedCost", "BillingCurrency",
	"BillingAccountId", "BillingAccountName", "SubAccountId", "SubAccountName",
	"BillingPeriodStart", "BillingPeriodEnd", "ChargePeriodStart", "ChargePeriodEnd",
	"ChargeCategory", "ChargeClass", "ChargeDescription", "ChargeFrequency",
	"ConsumedQuantity", "ConsumedUnit", "PricingQuantity", "PricingUnit",
	"ProviderName", "PublisherName", "InvoiceIssuerName",
	"ServiceCategory", "ServiceName", "SkuId", "ResourceId", "ResourceName", "ResourceType", "Tags",
	"x_Requests", "x_InputTokens", "x_OutputTokens", "x_CachedInputTokens", "x_ReasoningTokens",
	"x_Images", "x_Characters", "x_AudioSeconds", "x_VideoSeconds", "x_CacheHits", "x_CacheSavedCost",
}

var chargebackColumns = []string{
	"period_start", "period_end", "organization_id", "organization", "team_id", "team",
	"requests", "input_tokens", "output_tokens", "cached_input_tokens", "cost_usd", "share_of_total", "cache_saved_usd",
}

// csvText keeps a spreadsheet from running a name as a formula.
func csvText(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

func money6(v float64) string { return strconv.FormatFloat(v, 'f', 6, 64) }
func qty(v float64) string    { return strconv.FormatFloat(v, 'f', -1, 64) }

// consumed picks what a row's usage is measured in.
func consumed(r *store.UsageExportRow) (float64, string) {
	switch {
	case r.InputTokens+r.OutputTokens > 0:
		return float64(r.InputTokens + r.OutputTokens), "Tokens"
	case r.Images > 0:
		return float64(r.Images), "Images"
	case r.Characters > 0:
		return float64(r.Characters), "Characters"
	case r.AudioSeconds+r.VideoSeconds > 0:
		return r.AudioSeconds + r.VideoSeconds, "Seconds"
	}
	return float64(r.Requests), "Requests"
}

// exportUsage streams usage as CSV: format=chargeback (the default) or
// format=focus.
func (s *Server) exportUsage(w http.ResponseWriter, r *http.Request) error {
	uq, err := s.usageFilter(r)
	if err != nil {
		return err
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "chargeback"
	}
	if format != "chargeback" && format != "focus" {
		return badRequest("format must be chargeback or focus")
	}
	ctx := r.Context()
	rows, err := s.store.UsageExport(ctx, uq)
	if err != nil {
		return err
	}
	// Names of the organizations and teams the rows mention. A deleted
	// one keeps its id.
	orgNames, teamNames := map[string]string{}, map[string]string{}
	for i := range rows {
		id := rows[i].OrgID
		if _, ok := orgNames[id]; ok || id == "" {
			continue
		}
		orgNames[id] = id
		if o, err := s.store.GetOrg(ctx, id); err == nil {
			orgNames[id] = o.Name
		}
		for after := ""; ; {
			teams, more, err := s.store.ListTeams(ctx, id, store.Page{After: after, Limit: 500})
			if err != nil {
				return err
			}
			for _, t := range teams {
				teamNames[t.ID] = t.Name
			}
			if !more || len(teams) == 0 {
				break
			}
			after = teams[len(teams)-1].ID
		}
	}
	name := func(m map[string]string, id string) string {
		if n, ok := m[id]; ok {
			return n
		}
		return id
	}

	h := w.Header()
	h.Set("Content-Type", "text/csv; charset=utf-8")
	h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s-%s.csv"`, format, uq.From.Format("20060102"), uq.To.Add(-time.Millisecond).Format("20060102")))
	h.Set("Cache-Control", "no-store")
	cw := csv.NewWriter(w)
	defer cw.Flush()

	if format == "chargeback" {
		type line struct {
			org, team string
			store.UsageRow
		}
		byTeam, order, total := map[string]*line{}, []string{}, 0.0
		for i := range rows {
			x := &rows[i]
			k := x.OrgID + "\x00" + x.TeamID
			l := byTeam[k]
			if l == nil {
				l = &line{org: x.OrgID, team: x.TeamID}
				byTeam[k] = l
				order = append(order, k)
			}
			l.Requests += x.Requests
			l.InputTokens += x.InputTokens
			l.OutputTokens += x.OutputTokens
			l.CachedInputTokens += x.CachedInputTokens
			l.CostUSD += x.CostUSD
			l.SavedUSD += x.SavedUSD
			total += x.CostUSD
		}
		sort.SliceStable(order, func(i, j int) bool {
			a, b := byTeam[order[i]], byTeam[order[j]]
			if an, bn := name(orgNames, a.org), name(orgNames, b.org); an != bn {
				return an < bn
			}
			return a.CostUSD > b.CostUSD
		})
		if err := cw.Write(chargebackColumns); err != nil {
			return err
		}
		from, to := uq.From.UTC().Format(time.RFC3339), uq.To.UTC().Format(time.RFC3339)
		for _, k := range order {
			l := byTeam[k]
			share := 0.0
			if total > 0 {
				share = l.CostUSD / total
			}
			if err := cw.Write([]string{
				from, to, l.org, csvText(name(orgNames, l.org)), l.team, csvText(name(teamNames, l.team)),
				strconv.FormatInt(l.Requests, 10), strconv.FormatInt(l.InputTokens, 10), strconv.FormatInt(l.OutputTokens, 10),
				strconv.FormatInt(l.CachedInputTokens, 10), money6(l.CostUSD), strconv.FormatFloat(share, 'f', 4, 64), money6(l.SavedUSD),
			}); err != nil {
				return err
			}
		}
		return nil
	}

	cfg := s.rt.Load().cfg
	if err := cw.Write(focusColumns); err != nil {
		return err
	}
	for i := range rows {
		x := &rows[i]
		provider, publisher := x.Provider, x.Provider
		if p, ok := cfg.Providers[x.Provider]; ok {
			if pub, ok := publishers[p.Type]; ok {
				publisher = pub
			}
		}
		if provider == "" { // refused before a target was chosen, or answered from the cache
			provider, publisher = "GenAIOpenGateway", "GenAIOpenGateway"
		}
		monthStart := time.Date(x.Day.Year(), x.Day.Month(), 1, 0, 0, 0, 0, time.UTC)
		n, unit := consumed(x)
		tags, _ := json.Marshal(map[string]string{
			"organization": name(orgNames, x.OrgID), "team": name(teamNames, x.TeamID), "endpoint": x.Endpoint, "gateway_model": x.Model,
		})
		sku := x.UpstreamModel
		if sku == "" {
			sku = x.Model
		}
		cost := money6(x.CostUSD)
		if err := cw.Write([]string{
			cost, cost, cost, cost, "USD",
			x.OrgID, csvText(name(orgNames, x.OrgID)), x.TeamID, csvText(name(teamNames, x.TeamID)),
			monthStart.Format(time.RFC3339), monthStart.AddDate(0, 1, 0).Format(time.RFC3339),
			x.Day.Format(time.RFC3339), x.Day.AddDate(0, 0, 1).Format(time.RFC3339),
			"Usage", "", csvText(fmt.Sprintf("%s on %s (%s)", sku, provider, x.Endpoint)), "Usage-Based",
			qty(n), unit, qty(n), unit,
			csvText(provider), csvText(publisher), csvText(provider),
			"AI and Machine Learning", csvText(provider), csvText(sku), csvText(x.Model), csvText(x.Model), "Model", string(tags),
			strconv.FormatInt(x.Requests, 10), strconv.FormatInt(x.InputTokens, 10), strconv.FormatInt(x.OutputTokens, 10),
			strconv.FormatInt(x.CachedInputTokens, 10), strconv.FormatInt(x.ReasoningTokens, 10),
			strconv.FormatInt(x.Images, 10), strconv.FormatInt(x.Characters, 10), qty(x.AudioSeconds), qty(x.VideoSeconds),
			strconv.FormatInt(x.CacheHits, 10), money6(x.SavedUSD),
		}); err != nil {
			return err
		}
	}
	return nil
}
