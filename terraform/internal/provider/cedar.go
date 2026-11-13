package provider

import (
	"context"
	"errors"
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

func asAPIError(err error, target **apiError) bool { return errors.As(err, target) }

type cedarModel struct {
	ID      types.String `tfsdk:"id"`
	OrgID   types.String `tfsdk:"org_id"`
	Text    types.String `tfsdk:"text"`
	Version types.Int64  `tfsdk:"version"`
}

type apiCedar struct {
	Version int64  `json:"version"`
	Text    string `json:"text"`
}

// cedarResource manages an organization's Cedar policies (docs/cedar.md).
type cedarResource struct{ c *client }

func newCedarResource() resource.Resource { return &cedarResource{} }

func (r *cedarResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_cedar_policies"
}

func (r *cedarResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "An organization's Cedar policies. Each change is a new version on the gateway; destroying removes them. Needs the owner role.",
		Attributes: map[string]schema.Attribute{
			"id": idAttribute(),
			"org_id": schema.StringAttribute{
				Required: true, Description: "The organization's id.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"text":    schema.StringAttribute{Required: true, Description: "The policies, in the Cedar language."},
			"version": schema.Int64Attribute{Computed: true, Description: "The version in force."},
		},
	}
}

func (r *cedarResource) Configure(_ context.Context, req resource.ConfigureRequest, _ *resource.ConfigureResponse) {
	r.c = configured(req.ProviderData)
}

func (r *cedarResource) path(m *cedarModel) string {
	return "/admin/v1/orgs/" + url.PathEscape(m.OrgID.ValueString()) + "/cedar"
}

func (r *cedarResource) put(ctx context.Context, m *cedarModel) error {
	var got apiCedar
	if err := r.c.do(ctx, http.MethodPut, r.path(m), map[string]any{"text": m.Text.ValueString()}, &got); err != nil {
		return err
	}
	m.ID, m.Version = m.OrgID, types.Int64Value(got.Version)
	return nil
}

func (r *cedarResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m cedarModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.put(ctx, &m); err != nil {
		resp.Diagnostics.AddError("Saving the Cedar policies failed", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *cedarResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m cedarModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if m.OrgID.IsNull() {
		m.OrgID = m.ID
	}
	var got apiCedar
	err := r.c.do(ctx, http.MethodGet, r.path(&m), nil, &got)
	if isNotFound(err) || (err == nil && got.Text == "") {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Reading the Cedar policies failed", err.Error())
		return
	}
	if strings.TrimSpace(got.Text) != strings.TrimSpace(m.Text.ValueString()) {
		m.Text = types.StringValue(got.Text)
	}
	m.ID, m.Version = m.OrgID, types.Int64Value(got.Version)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *cedarResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m cedarModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.put(ctx, &m); err != nil {
		resp.Diagnostics.AddError("Saving the Cedar policies failed", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *cedarResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m cedarModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.c.do(ctx, http.MethodDelete, r.path(&m), nil, nil); err != nil && !isNotFound(err) {
		resp.Diagnostics.AddError("Removing the Cedar policies failed", err.Error())
	}
}

// ImportState takes the organization's id.
func (r *cedarResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("org_id"), req.ID)...)
}
