package provider

import (
	"context"
	"fmt"

	"github.com/fluxvale/fluxvale/apps/tfprovider/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ datasource.DataSource = (*appsDataSource)(nil)

// appsDataSource is fluxvale_apps — the catalog browse list
// (GET /api/v1/apps), mapped field-for-field from the wire's
// default_fields.
type appsDataSource struct {
	client *client.Client
}

// NewAppsDataSource returns the data-source factory provider.go
// registers.
func NewAppsDataSource() datasource.DataSource {
	return &appsDataSource{}
}

func (d *appsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_apps"
}

// appModel is one catalog app row — App's default_fields, verbatim.
type appModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Slug        types.String `tfsdk:"slug"`
	Tagline     types.String `tfsdk:"tagline"`
	Description types.String `tfsdk:"description"`
	IconURL     types.String `tfsdk:"icon_url"`
	SourceURL   types.String `tfsdk:"source_url"`
	DocsURL     types.String `tfsdk:"docs_url"`
	FileAccess  types.Bool   `tfsdk:"file_access"`
}

type appsModel struct {
	Apps []appModel `tfsdk:"apps"`
}

func (d *appsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "The FluxVale catalog — every deployable app, sorted by name.",
		Attributes: map[string]schema.Attribute{
			"apps": schema.ListNestedAttribute{
				Computed:    true,
				Description: "The catalog's apps, in the API's order (by name).",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id": schema.StringAttribute{
							Computed:    true,
							Description: "The app id.",
						},
						"name": schema.StringAttribute{
							Computed:    true,
							Description: "Display name.",
						},
						"slug": schema.StringAttribute{
							Computed:    true,
							Description: "URL-shaped slug — the app's lookup key.",
						},
						"tagline": schema.StringAttribute{
							Computed:    true,
							Description: "One-liner (null when the app has none).",
						},
						"description": schema.StringAttribute{
							Computed:    true,
							Description: "Longer description (null when absent).",
						},
						"icon_url": schema.StringAttribute{
							Computed:    true,
							Description: "Icon URL (null when absent).",
						},
						"source_url": schema.StringAttribute{
							Computed:    true,
							Description: "Upstream source URL (null when absent).",
						},
						"docs_url": schema.StringAttribute{
							Computed:    true,
							Description: "Docs URL (null when absent).",
						},
						"file_access": schema.BoolAttribute{
							Computed:    true,
							Description: "Whether instances of this app get the WebDAV file-access sidecar.",
						},
					},
				},
			},
		},
	}
}

func (d *appsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	pd, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected provider data type",
			fmt.Sprintf("Got %T, want *providerData. Please report this bug.", req.ProviderData),
		)
		return
	}
	d.client = pd.client
}

func (d *appsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	if d.client == nil {
		resp.Diagnostics.AddError(
			"Provider not configured",
			"The provider was not configured before this data source was used — please report this bug.",
		)
		return
	}

	var data appsModel
	data.Apps = listApps(ctx, d.client, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// listApps maps GET /api/v1/apps rows into the model.
func listApps(ctx context.Context, c *client.Client, diags *diag.Diagnostics) []appModel {
	apps, err := c.ListApps(ctx)
	if err != nil {
		diags.AddError("Unable to list the catalog", err.Error())
		return nil
	}
	out := make([]appModel, 0, len(apps))
	for _, app := range apps {
		out = append(out, appModel{
			ID:          types.StringValue(app.ID),
			Name:        types.StringValue(app.Name),
			Slug:        types.StringValue(app.Slug),
			Tagline:     types.StringPointerValue(app.Tagline),
			Description: types.StringPointerValue(app.Description),
			IconURL:     types.StringPointerValue(app.IconURL),
			SourceURL:   types.StringPointerValue(app.SourceURL),
			DocsURL:     types.StringPointerValue(app.DocsURL),
			FileAccess:  types.BoolValue(app.FileAccess),
		})
	}
	return out
}
