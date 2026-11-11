package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	dschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type orgModel struct {
	ID   types.String `tfsdk:"id"`
	Name types.String `tfsdk:"name"`
	policyModel
}

type apiOrg struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	OrgID string `json:"org_id"`
	apiPolicy
}

func idAttribute() schema.StringAttribute {
	return schema.StringAttribute{
		Computed:      true,
		Description:   "The gateway's id.",
		PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
	}
}

// orgResource manages an organization.
type orgResource struct{ c *client }

func newOrgResource() resource.Resource { return &orgResource{} }

func (r *orgResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_organization"
}

func (r *orgResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "An organization: the top of the tenancy, with a policy its teams and keys inherit. Changing it needs the owner role.",
		Attributes: withPolicy(map[string]schema.Attribute{
			"id":   idAttribute(),
			"name": schema.StringAttribute{Required: true, Description: "Unique without case."},
		}),
	}
}

func (r *orgResource) Configure(_ context.Context, req resource.ConfigureRequest, _ *resource.ConfigureResponse) {
	r.c = configured(req.ProviderData)
}

func (r *orgResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m orgModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body := m.body()
	body["name"] = m.Name.ValueString()
	var got apiOrg
	if err := r.c.do(ctx, http.MethodPost, "/admin/v1/orgs", body, &got); err != nil {
		resp.Diagnostics.AddError("Creating the organization failed", err.Error())
		return
	}
	m.ID, m.Name = types.StringValue(got.ID), types.StringValue(got.Name)
	m.read(got.apiPolicy)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *orgResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m orgModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var got apiOrg
	err := r.c.do(ctx, http.MethodGet, "/admin/v1/orgs/"+url.PathEscape(m.ID.ValueString()), nil, &got)
	if isNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Reading the organization failed", err.Error())
		return
	}
	m.Name = types.StringValue(got.Name)
	m.read(got.apiPolicy)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *orgResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m orgModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body := m.body()
	body["name"] = m.Name.ValueString()
	var got apiOrg
	if err := r.c.do(ctx, http.MethodPatch, "/admin/v1/orgs/"+url.PathEscape(m.ID.ValueString()), body, &got); err != nil {
		resp.Diagnostics.AddError("Updating the organization failed", err.Error())
		return
	}
	m.read(got.apiPolicy)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *orgResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m orgModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	err := r.c.do(ctx, http.MethodDelete, "/admin/v1/orgs/"+url.PathEscape(m.ID.ValueString()), nil, nil)
	if err != nil && !isNotFound(err) {
		resp.Diagnostics.AddError("Deleting the organization failed", err.Error())
	}
}

func (r *orgResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}

// orgDataSource finds an organization by name.
type orgDataSource struct{ c *client }

func newOrgDataSource() datasource.DataSource { return &orgDataSource{} }

type orgDataModel struct {
	ID   types.String `tfsdk:"id"`
	Name types.String `tfsdk:"name"`
}

func (d *orgDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_organization"
}

func (d *orgDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = dschema.Schema{
		Description: "Looks up an organization by name, such as one created in the admin UI.",
		Attributes: map[string]dschema.Attribute{
			"name": dschema.StringAttribute{Required: true},
			"id":   dschema.StringAttribute{Computed: true},
		},
	}
}

func (d *orgDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, _ *datasource.ConfigureResponse) {
	d.c = configured(req.ProviderData)
}

func (d *orgDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var m orgDataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	after := ""
	for {
		var page struct {
			Data    []apiOrg `json:"data"`
			HasMore bool     `json:"has_more"`
		}
		q := url.Values{"limit": {"500"}}
		if after != "" {
			q.Set("after", after)
		}
		if err := d.c.do(ctx, http.MethodGet, "/admin/v1/orgs?"+q.Encode(), nil, &page); err != nil {
			resp.Diagnostics.AddError("Listing organizations failed", err.Error())
			return
		}
		for _, o := range page.Data {
			if equalFold(o.Name, m.Name.ValueString()) {
				m.ID, m.Name = types.StringValue(o.ID), types.StringValue(o.Name)
				resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
				return
			}
		}
		if !page.HasMore || len(page.Data) == 0 {
			break
		}
		after = page.Data[len(page.Data)-1].ID
	}
	resp.Diagnostics.AddError("No such organization", fmt.Sprintf("The gateway has no organization named %q.", m.Name.ValueString()))
}
