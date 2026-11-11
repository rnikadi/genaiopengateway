// Package provider implements the GenAIOpenGateway Terraform provider.
package provider

import (
	"context"
	"net/http"
	"os"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// New returns the provider for a version.
func New(version string) func() provider.Provider {
	return func() provider.Provider { return &gatewayProvider{version: version} }
}

type gatewayProvider struct{ version string }

type providerModel struct {
	URL   types.String `tfsdk:"url"`
	Token types.String `tfsdk:"token"`
}

func (p *gatewayProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "genaiopengateway"
	resp.Version = p.version
}

func (p *gatewayProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a GenAIOpenGateway over its admin API.",
		Attributes: map[string]schema.Attribute{
			"url": schema.StringAttribute{
				Optional:    true,
				Description: "The gateway's URL, such as https://gateway.example.com. Defaults to GOG_URL, then http://localhost:8080.",
			},
			"token": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "The admin token. Defaults to GOG_ADMIN_TOKEN.",
			},
		},
	}
}

func (p *gatewayProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var m providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	base := firstOf(m.URL.ValueString(), os.Getenv("GOG_URL"), "http://localhost:8080")
	token := firstOf(m.Token.ValueString(), os.Getenv("GOG_ADMIN_TOKEN"))
	if token == "" {
		resp.Diagnostics.AddError("Missing admin token", "Set the provider's token or GOG_ADMIN_TOKEN.")
		return
	}
	c := &client{base: base, token: token, http: &http.Client{Timeout: 60 * time.Second}, version: p.version}
	resp.ResourceData, resp.DataSourceData = c, c
}

func (p *gatewayProvider) Resources(context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		newOrgResource, newTeamResource, newKeyResource, newCedarResource, newConfigObjectResource,
	}
}

func (p *gatewayProvider) DataSources(context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{newOrgDataSource}
}

func firstOf(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// configured takes the client a resource was configured with.
func configured(data any) *client {
	c, _ := data.(*client)
	return c
}
