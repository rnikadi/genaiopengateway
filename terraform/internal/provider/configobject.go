package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// configKinds are the kinds of stored configuration objects.
var configKinds = []string{"providers", "models", "guardrails", "mcp_servers", "mcp_virtual_servers", "a2a_agents", "alert_webhooks", "streams"}

type configObjectModel struct {
	ID   types.String `tfsdk:"id"`
	Kind types.String `tfsdk:"kind"`
	Name types.String `tfsdk:"name"`
	Spec types.String `tfsdk:"spec"`
}

// configObjectResource manages a stored configuration object: a
// provider, model, guardrail, MCP server, virtual MCP server, alert
// webhook or stream (docs/config.md).
type configObjectResource struct{ c *client }

func newConfigObjectResource() resource.Resource { return &configObjectResource{} }

func (r *configObjectResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_config_object"
}

func (r *configObjectResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	replace := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		Description: "A configuration object stored in the gateway, as the admin UI's Configuration page edits them. Needs the owner role. Destroying it deletes the stored object; one from the config file then applies again.",
		Attributes: map[string]schema.Attribute{
			"id":   idAttribute(),
			"kind": schema.StringAttribute{Required: true, PlanModifiers: replace, Description: "One of " + strings.Join(configKinds, ", ") + "."},
			"name": schema.StringAttribute{Required: true, PlanModifiers: replace},
			"spec": schema.StringAttribute{
				Required: true, Sensitive: true,
				Description: "The object as JSON, as in the config file; use jsonencode(). Secrets, such as a provider's api_key, are sent but the gateway never returns them.",
			},
		},
	}
}

func (r *configObjectResource) Configure(_ context.Context, req resource.ConfigureRequest, _ *resource.ConfigureResponse) {
	r.c = configured(req.ProviderData)
}

func (r *configObjectResource) path(m *configObjectModel) string {
	return "/admin/v1/config/" + url.PathEscape(m.Kind.ValueString()) + "/" + url.PathEscape(m.Name.ValueString())
}

func (r *configObjectResource) put(ctx context.Context, m *configObjectModel) error {
	var spec map[string]any
	if err := json.Unmarshal([]byte(m.Spec.ValueString()), &spec); err != nil || spec == nil {
		return fmt.Errorf("spec must be a JSON object: %v", err)
	}
	if err := r.c.do(ctx, http.MethodPut, r.path(m), spec, nil); err != nil {
		return err
	}
	m.ID = types.StringValue(m.Kind.ValueString() + "/" + m.Name.ValueString())
	return nil
}

func (r *configObjectResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m configObjectModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.put(ctx, &m); err != nil {
		resp.Diagnostics.AddError("Saving the object failed", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *configObjectResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m configObjectModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if m.Kind.IsNull() {
		kind, name, _ := strings.Cut(m.ID.ValueString(), "/")
		m.Kind, m.Name = types.StringValue(kind), types.StringValue(name)
	}
	var list struct {
		Data []struct {
			Kind   string         `json:"kind"`
			Name   string         `json:"name"`
			Source string         `json:"source"`
			Spec   map[string]any `json:"spec"`
		} `json:"data"`
	}
	if err := r.c.do(ctx, http.MethodGet, "/admin/v1/config", nil, &list); err != nil {
		resp.Diagnostics.AddError("Reading the configuration failed", err.Error())
		return
	}
	for _, o := range list.Data {
		if o.Kind != m.Kind.ValueString() || o.Name != m.Name.ValueString() {
			continue
		}
		if o.Source != "store" {
			break
		}
		var prior map[string]any
		_ = json.Unmarshal([]byte(m.Spec.ValueString()), &prior)
		// Secrets read "set", so compare with the configuration's
		// secrets masked the same way.
		if !reflect.DeepEqual(maskLike(prior, o.Spec), normalize(o.Spec)) {
			raw, _ := json.Marshal(o.Spec)
			m.Spec = types.StringValue(string(raw))
		}
		m.ID = types.StringValue(o.Kind + "/" + o.Name)
		resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
		return
	}
	resp.State.RemoveResource(ctx)
}

func (r *configObjectResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m configObjectModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.put(ctx, &m); err != nil {
		resp.Diagnostics.AddError("Saving the object failed", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *configObjectResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m configObjectModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.c.do(ctx, http.MethodDelete, r.path(&m), nil, nil); err != nil && !isNotFound(err) {
		resp.Diagnostics.AddError("Deleting the object failed", err.Error())
	}
}

// ImportState takes kind/name, such as models/gpt-5-mini.
func (r *configObjectResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if !strings.Contains(req.ID, "/") {
		resp.Diagnostics.AddError("Bad import id", "Import a configuration object as kind/name, such as models/gpt-5-mini.")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("spec"), "{}")...)
}

// maskLike returns v with each string replaced by "set" where like has
// "set", normalized as JSON.
func maskLike(v, like any) any {
	switch l := like.(type) {
	case string:
		if _, ok := v.(string); ok && l == "set" {
			return "set"
		}
	case map[string]any:
		if m, ok := v.(map[string]any); ok {
			out := map[string]any{}
			for k, e := range m {
				out[k] = maskLike(e, l[k])
			}
			return normalize(out)
		}
	case []any:
		if s, ok := v.([]any); ok {
			out := make([]any, len(s))
			for i, e := range s {
				var li any
				if i < len(l) {
					li = l[i]
				}
				out[i] = maskLike(e, li)
			}
			return normalize(out)
		}
	}
	return normalize(v)
}

func normalize(v any) any {
	raw, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(raw, &out)
	return out
}
