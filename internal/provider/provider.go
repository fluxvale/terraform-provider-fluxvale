// The FluxVale Terraform/OpenTofu provider (ADR-0019 Am. 4): a thin
// skin over the /api/v1 JSON:API. Configure resolves the token,
// pings /api/v1/me, and hands the client to resources and data
// sources. Policy, paywall, and scopes enforce server-side — this
// layer re-implements nothing.
package provider

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/fluxvale/fluxvale/apps/tfprovider/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

const (
	// envToken is the documented credential fallback — stable in the
	// compatibility contract (apps/tfprovider/docs/compatibility.md).
	envToken = "FLUXVALE_PAT"

	// defaultAPIURL matches the contract's documented default; staging
	// overrides via api_url.
	defaultAPIURL = "https://fluxvale.com"
)

var _ provider.Provider = (*fluxvaleProvider)(nil)

// fluxvaleProvider implements provider.Provider.
type fluxvaleProvider struct {
	// version is the binary's version (wired at release build time via
	// -ldflags); it rides the client's User-Agent.
	version string
}

// New returns the provider factory main.go serves.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &fluxvaleProvider{version: version}
	}
}

// providerData is what Configure hands to every resource and data
// source: the authenticated client, the pinged identity, and the
// poll pacing for async lifecycles (deploy wait, teardown wait, the
// custom-domain verify wait — that wait's deadline comes from the
// resource's timeouts.create; only the interval comes from here).
type providerData struct {
	client       *client.Client
	userID       string
	userEmail    string
	organization types.String

	// Poll pacing for the async lifecycles. Configure sets the
	// production values (10s interval, 10m deadline — the deploy and
	// teardown Oban jobs); tests shorten both.
	pollInterval time.Duration
	pollTimeout  time.Duration
}

func (p *fluxvaleProvider) Metadata(ctx context.Context, req provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "fluxvale"
	resp.Version = p.version
}

func (p *fluxvaleProvider) Schema(ctx context.Context, req provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Terraform provider for FluxVale — a thin client over the /api/v1 JSON:API (ADR-0019 Am. 4).",
		Attributes: map[string]schema.Attribute{
			"api_url": schema.StringAttribute{
				Optional:    true,
				Description: "Base URL of the FluxVale API. Defaults to https://fluxvale.com; override for staging.",
			},
			"token": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "FluxVale personal access token (fvpat_…). Falls back to the FLUXVALE_PAT environment variable.",
			},
			"organization": schema.StringAttribute{
				Optional:    true,
				Description: "Default organization for resources that need one — its id or slug. A resource's own \"organization\" attribute wins over this; with neither set, resources fall back to your sole organization.",
			},
		},
	}
}

// providerConfig is the model of the provider block.
type providerConfig struct {
	APIURL       types.String `tfsdk:"api_url"`
	Token        types.String `tfsdk:"token"`
	Organization types.String `tfsdk:"organization"`
}

func (p *fluxvaleProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var config providerConfig
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Symmetric trim: a stray space in either source must not become a
	// confusing 401 blamed on the token itself.
	token := strings.TrimSpace(stringValue(config.Token))
	if token == "" {
		token = strings.TrimSpace(os.Getenv(envToken))
	}
	if token == "" {
		resp.Diagnostics.AddError(
			"Missing FluxVale API token",
			"Set the \"token\" provider attribute or the "+envToken+" environment variable.",
		)
		return
	}

	// An unknown api_url is an error, not the default: silently
	// substituting production would send this credential to the wrong
	// environment (staging setups point here).
	if config.APIURL.IsUnknown() {
		resp.Diagnostics.AddError(
			"API URL is not known yet",
			"The \"api_url\" provider attribute is computed from a value that is not known at plan time. "+
				"Set a known URL, or leave it unset for the default ("+defaultAPIURL+").",
		)
		return
	}

	apiURL := stringValue(config.APIURL)
	if apiURL == "" {
		apiURL = defaultAPIURL
	}

	providerClient := client.New(apiURL, token, p.version, nil)

	// Auth ping: /api/v1/me is scopeless — any live token works, so a
	// bad credential fails here, in Configure, with the server's own
	// wording — not at the first resource.
	me, err := providerClient.Me(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Unable to authenticate with the FluxVale API", err.Error())
		return
	}

	data := &providerData{
		client:       providerClient,
		userID:       me.ID,
		userEmail:    me.Email,
		organization: config.Organization,
		pollInterval: 10 * time.Second,
		pollTimeout:  10 * time.Minute,
	}
	resp.ResourceData = data
	resp.DataSourceData = data
}

// Resources returns the provider's resource set.
func (p *fluxvaleProvider) Resources(ctx context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewInstanceResource,
		NewBackupsResource,
		NewGatewayResource,
		NewCustomDomainResource,
	}
}

// DataSources returns the provider's data-source set.
func (p *fluxvaleProvider) DataSources(ctx context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{NewAppsDataSource, NewAppVersionsDataSource}
}

// stringValue returns a config string only if set AND known. Values
// can be unknown at plan time (computed input variables); an unknown
// value falls back exactly like unset.
func stringValue(v types.String) string {
	if v.IsNull() || v.IsUnknown() {
		return ""
	}
	return v.ValueString()
}
