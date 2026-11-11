package provider

import (
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// policyModel is the policy every level of the tenancy takes
// (docs/virtual-keys.md). Unset settings are no restriction.
type policyModel struct {
	AllowedModels     types.List   `tfsdk:"allowed_models"`
	AllowedMCPServers types.List   `tfsdk:"allowed_mcp_servers"`
	AllowedAgents     types.List   `tfsdk:"allowed_agents"`
	RPM               types.Int64  `tfsdk:"rpm"`
	TPM               types.Int64  `tfsdk:"tpm"`
	Guardrails        types.List   `tfsdk:"guardrails"`
	Residency         types.List   `tfsdk:"residency"`
	LogBodies         types.Bool   `tfsdk:"log_bodies"`
	Budget            *budgetModel `tfsdk:"budget"`
	Cache             *cacheModel  `tfsdk:"cache"`
}

type budgetModel struct {
	AmountUSD types.Float64 `tfsdk:"amount_usd"`
	Period    types.String  `tfsdk:"period"`
}

type cacheModel struct {
	Mode types.String `tfsdk:"mode"`
	TTL  types.String `tfsdk:"ttl"`
}

// policyAttributes are the schema of policyModel.
func policyAttributes() map[string]schema.Attribute {
	list := func(desc string) schema.ListAttribute {
		return schema.ListAttribute{ElementType: types.StringType, Optional: true, Description: desc}
	}
	return map[string]schema.Attribute{
		"allowed_models":      list("Virtual models it may use; unset allows all."),
		"allowed_mcp_servers": list("Virtual MCP servers it may use; unset allows all."),
		"allowed_agents":      list("A2A agents it may call; unset allows all."),
		"rpm":                 schema.Int64Attribute{Optional: true, Description: "Requests per minute; unset is no limit."},
		"tpm":                 schema.Int64Attribute{Optional: true, Description: "Tokens per minute; unset is no limit."},
		"guardrails":          list("Guardrails to run, added to those of the levels above."),
		"residency":           list("Residency labels its requests are pinned to."),
		"log_bodies":          schema.BoolAttribute{Optional: true, Description: "Whether request bodies are logged; unset inherits."},
		"budget": schema.SingleNestedAttribute{
			Optional:    true,
			Description: "Spend limit.",
			Attributes: map[string]schema.Attribute{
				"amount_usd": schema.Float64Attribute{Required: true},
				"period":     schema.StringAttribute{Required: true, Description: "daily, weekly, monthly or total."},
			},
		},
		"cache": schema.SingleNestedAttribute{
			Optional:    true,
			Description: "Response cache; unset inherits.",
			Attributes: map[string]schema.Attribute{
				"mode": schema.StringAttribute{Required: true, Description: "off, exact or semantic."},
				"ttl":  schema.StringAttribute{Optional: true, Description: "How long an answer is reused, such as 1h."},
			},
		},
	}
}

// withPolicy adds the policy attributes to a resource's own.
func withPolicy(own map[string]schema.Attribute) map[string]schema.Attribute {
	for k, v := range policyAttributes() {
		own[k] = v
	}
	return own
}

// body is the policy as a PATCH or POST body that sets every setting,
// clearing those left unset.
func (p *policyModel) body() map[string]any {
	b := map[string]any{
		"allowed_models":      stringList(p.AllowedModels),
		"allowed_mcp_servers": stringList(p.AllowedMCPServers),
		"allowed_agents":      stringList(p.AllowedAgents),
		"guardrails":          stringList(p.Guardrails),
		"residency":           stringList(p.Residency),
		"rpm":                 p.RPM.ValueInt64(),
		"tpm":                 p.TPM.ValueInt64(),
		"budget":              nil,
		"log_bodies":          nil,
		"cache":               nil,
	}
	if !p.LogBodies.IsNull() && !p.LogBodies.IsUnknown() {
		b["log_bodies"] = p.LogBodies.ValueBool()
	}
	if p.Budget != nil {
		b["budget"] = map[string]any{"amount_usd": p.Budget.AmountUSD.ValueFloat64(), "period": p.Budget.Period.ValueString()}
	}
	if p.Cache != nil {
		c := map[string]any{"mode": p.Cache.Mode.ValueString()}
		if ttl := p.Cache.TTL.ValueString(); ttl != "" {
			c["ttl"] = ttl
		}
		b["cache"] = c
	}
	return b
}

// apiPolicy is the policy as the admin API returns it.
type apiPolicy struct {
	AllowedModels     []string `json:"allowed_models"`
	AllowedMCPServers []string `json:"allowed_mcp_servers"`
	AllowedAgents     []string `json:"allowed_agents"`
	RPM               int64    `json:"rpm"`
	TPM               int64    `json:"tpm"`
	Guardrails        []string `json:"guardrails"`
	Residency         []string `json:"residency"`
	LogBodies         *bool    `json:"log_bodies"`
	Budget            *struct {
		AmountUSD float64 `json:"amount_usd"`
		Period    string  `json:"period"`
	} `json:"budget"`
	Cache *struct {
		Mode string `json:"mode"`
		TTL  string `json:"ttl"`
	} `json:"cache"`
}

// read sets the model from the API. The API leaves out empty and zero
// settings, so a setting the configuration has as empty or zero stays
// so rather than turning null.
func (p *policyModel) read(a apiPolicy) {
	p.AllowedModels = readList(p.AllowedModels, a.AllowedModels)
	p.AllowedMCPServers = readList(p.AllowedMCPServers, a.AllowedMCPServers)
	p.AllowedAgents = readList(p.AllowedAgents, a.AllowedAgents)
	p.Guardrails = readList(p.Guardrails, a.Guardrails)
	p.Residency = readList(p.Residency, a.Residency)
	p.RPM = readInt(p.RPM, a.RPM)
	p.TPM = readInt(p.TPM, a.TPM)
	if a.LogBodies != nil {
		p.LogBodies = types.BoolValue(*a.LogBodies)
	} else {
		p.LogBodies = types.BoolNull()
	}
	if a.Budget != nil {
		p.Budget = &budgetModel{AmountUSD: types.Float64Value(a.Budget.AmountUSD), Period: types.StringValue(a.Budget.Period)}
	} else {
		p.Budget = nil
	}
	if a.Cache != nil {
		ttl := types.StringNull()
		if a.Cache.TTL != "" || (p.Cache != nil && !p.Cache.TTL.IsNull()) {
			ttl = types.StringValue(a.Cache.TTL)
		}
		p.Cache = &cacheModel{Mode: types.StringValue(a.Cache.Mode), TTL: ttl}
	} else {
		p.Cache = nil
	}
}

func readList(prior types.List, got []string) types.List {
	if len(got) == 0 {
		if !prior.IsNull() && !prior.IsUnknown() && len(prior.Elements()) == 0 {
			return prior
		}
		return types.ListNull(types.StringType)
	}
	vals := make([]attr.Value, len(got))
	for i, s := range got {
		vals[i] = types.StringValue(s)
	}
	return types.ListValueMust(types.StringType, vals)
}

func readInt(prior types.Int64, got int64) types.Int64 {
	if got == 0 && (prior.IsNull() || prior.IsUnknown()) {
		return types.Int64Null()
	}
	return types.Int64Value(got)
}

func stringList(l types.List) []string {
	out := []string{}
	if l.IsNull() || l.IsUnknown() {
		return out
	}
	for _, v := range l.Elements() {
		if s, ok := v.(types.String); ok {
			out = append(out, s.ValueString())
		}
	}
	return out
}
