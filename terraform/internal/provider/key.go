package provider

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type keyModel struct {
	ID        types.String `tfsdk:"id"`
	TeamID    types.String `tfsdk:"team_id"`
	Name      types.String `tfsdk:"name"`
	Owner     types.String `tfsdk:"owner"`
	ExpiresAt types.String `tfsdk:"expires_at"`
	Prefix    types.String `tfsdk:"prefix"`
	Secret    types.String `tfsdk:"secret"`
	policyModel
}

type apiKey struct {
	ID        string     `json:"id"`
	TeamID    string     `json:"team_id"`
	Name      string     `json:"name"`
	Owner     string     `json:"owner"`
	Prefix    string     `json:"prefix"`
	Key       string     `json:"key"`
	ExpiresAt *time.Time `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at"`
	apiPolicy
}

// keyResource manages a virtual key. Destroying it revokes it.
type keyResource struct{ c *client }

func newKeyResource() resource.Resource { return &keyResource{} }

func (r *keyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_key"
}

func (r *keyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	keep := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		Description: "A virtual key of a team. The secret is known only to the state of the configuration that created it; destroying the key revokes it.",
		Attributes: withPolicy(map[string]schema.Attribute{
			"id": idAttribute(),
			"team_id": schema.StringAttribute{
				Required: true, Description: "The team's id.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"name":       schema.StringAttribute{Required: true},
			"owner":      schema.StringAttribute{Optional: true, Description: "Who the key belongs to, for alerts."},
			"expires_at": schema.StringAttribute{Optional: true, Description: "When the key stops working, in RFC 3339."},
			"prefix":     schema.StringAttribute{Computed: true, Description: "The secret's first characters, as the admin UI shows them.", PlanModifiers: keep},
			"secret": schema.StringAttribute{
				Computed: true, Sensitive: true, PlanModifiers: keep,
				Description: "The key. Empty after an import, since the gateway shows a key only once.",
			},
		}),
	}
}

func (r *keyResource) Configure(_ context.Context, req resource.ConfigureRequest, _ *resource.ConfigureResponse) {
	r.c = configured(req.ProviderData)
}

func (m *keyModel) body() map[string]any {
	b := m.policyModel.body()
	b["name"] = m.Name.ValueString()
	b["owner"] = m.Owner.ValueString()
	b["expires_at"] = nil
	if v := m.ExpiresAt.ValueString(); v != "" {
		b["expires_at"] = v
	}
	return b
}

func (m *keyModel) read(k apiKey) {
	m.ID, m.TeamID, m.Name, m.Prefix = types.StringValue(k.ID), types.StringValue(k.TeamID), types.StringValue(k.Name), types.StringValue(k.Prefix)
	if k.Owner != "" || !m.Owner.IsNull() {
		m.Owner = types.StringValue(k.Owner)
	}
	if k.ExpiresAt == nil {
		m.ExpiresAt = types.StringNull()
	} else if t, err := time.Parse(time.RFC3339, m.ExpiresAt.ValueString()); err != nil || !t.Equal(*k.ExpiresAt) {
		m.ExpiresAt = types.StringValue(k.ExpiresAt.Format(time.RFC3339))
	}
	m.policyModel.read(k.apiPolicy)
}

func (r *keyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m keyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var got apiKey
	if err := r.c.do(ctx, http.MethodPost, "/admin/v1/teams/"+url.PathEscape(m.TeamID.ValueString())+"/keys", m.body(), &got); err != nil {
		resp.Diagnostics.AddError("Creating the key failed", err.Error())
		return
	}
	m.read(got)
	m.Secret = types.StringValue(got.Key)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *keyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m keyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var got apiKey
	err := r.c.do(ctx, http.MethodGet, "/admin/v1/keys/"+url.PathEscape(m.ID.ValueString()), nil, &got)
	if isNotFound(err) || (err == nil && got.RevokedAt != nil) {
		// A revoked key cannot come back, so Terraform makes a new one.
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Reading the key failed", err.Error())
		return
	}
	m.read(got)
	if m.Secret.IsNull() || m.Secret.IsUnknown() {
		m.Secret = types.StringValue("")
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *keyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m keyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var got apiKey
	if err := r.c.do(ctx, http.MethodPatch, "/admin/v1/keys/"+url.PathEscape(m.ID.ValueString()), m.body(), &got); err != nil {
		resp.Diagnostics.AddError("Updating the key failed", err.Error())
		return
	}
	m.read(got)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *keyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m keyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	err := r.c.do(ctx, http.MethodPost, "/admin/v1/keys/"+url.PathEscape(m.ID.ValueString())+"/revoke", nil, nil)
	if err != nil && !isNotFound(err) {
		var ae *apiError
		if asAPIError(err, &ae) && ae.Status == http.StatusConflict {
			return // already revoked
		}
		resp.Diagnostics.AddError("Revoking the key failed", err.Error())
	}
}

func (r *keyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
