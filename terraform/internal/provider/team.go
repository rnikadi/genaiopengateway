package provider

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func equalFold(a, b string) bool { return strings.EqualFold(a, b) }

type teamModel struct {
	ID    types.String `tfsdk:"id"`
	OrgID types.String `tfsdk:"org_id"`
	Name  types.String `tfsdk:"name"`
	policyModel
}

// teamResource manages a team of an organization.
type teamResource struct{ c *client }

func newTeamResource() resource.Resource { return &teamResource{} }

func (r *teamResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_team"
}

func (r *teamResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A team of an organization. Its keys inherit its policy, within the organization's.",
		Attributes: withPolicy(map[string]schema.Attribute{
			"id": idAttribute(),
			"org_id": schema.StringAttribute{
				Required: true, Description: "The organization's id.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"name": schema.StringAttribute{Required: true, Description: "Unique within the organization."},
		}),
	}
}

func (r *teamResource) Configure(_ context.Context, req resource.ConfigureRequest, _ *resource.ConfigureResponse) {
	r.c = configured(req.ProviderData)
}

func (r *teamResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m teamModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body := m.body()
	body["name"] = m.Name.ValueString()
	var got apiOrg
	if err := r.c.do(ctx, http.MethodPost, "/admin/v1/orgs/"+url.PathEscape(m.OrgID.ValueString())+"/teams", body, &got); err != nil {
		resp.Diagnostics.AddError("Creating the team failed", err.Error())
		return
	}
	m.ID = types.StringValue(got.ID)
	m.read(got.apiPolicy)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *teamResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m teamModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var got apiOrg
	err := r.c.do(ctx, http.MethodGet, "/admin/v1/teams/"+url.PathEscape(m.ID.ValueString()), nil, &got)
	if isNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Reading the team failed", err.Error())
		return
	}
	m.Name, m.OrgID = types.StringValue(got.Name), types.StringValue(got.OrgID)
	m.read(got.apiPolicy)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *teamResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m teamModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body := m.body()
	body["name"] = m.Name.ValueString()
	var got apiOrg
	if err := r.c.do(ctx, http.MethodPatch, "/admin/v1/teams/"+url.PathEscape(m.ID.ValueString()), body, &got); err != nil {
		resp.Diagnostics.AddError("Updating the team failed", err.Error())
		return
	}
	m.read(got.apiPolicy)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *teamResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m teamModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	err := r.c.do(ctx, http.MethodDelete, "/admin/v1/teams/"+url.PathEscape(m.ID.ValueString()), nil, nil)
	if err != nil && !isNotFound(err) {
		resp.Diagnostics.AddError("Deleting the team failed", err.Error())
	}
}

func (r *teamResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
