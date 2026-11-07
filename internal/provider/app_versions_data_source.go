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

var _ datasource.DataSource = (*appVersionsDataSource)(nil)

// appVersionsDataSource is fluxvale_app_versions — one app's deployable
// versions (GET /api/v1/apps/:id/versions), by app slug or id.
type appVersionsDataSource struct {
	client *client.Client
}

// NewAppVersionsDataSource returns the data-source factory provider.go
// registers.
func NewAppVersionsDataSource() datasource.DataSource {
	return &appVersionsDataSource{}
}

func (d *appVersionsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_app_versions"
}

// appVersionModel is one version row — AppVersion's default_fields
// minus the two mixed-type spec objects (configurable_env_vars,
// first_run): no flat Terraform type fits them; the panel renders
// them.
type appVersionModel struct {
	ID               types.String  `tfsdk:"id"`
	AppID            types.String  `tfsdk:"app_id"`
	Version          types.String  `tfsdk:"version"`
	Image            types.String  `tfsdk:"image"`
	Port             types.Int64   `tfsdk:"port"`
	HealthcheckPath  types.String  `tfsdk:"healthcheck_path"`
	StorageMountPath types.String  `tfsdk:"storage_mount_path"`
	DefaultCPUCores  types.Float64 `tfsdk:"default_cpu_cores"`
	DefaultMemoryMB  types.Int64   `tfsdk:"default_memory_mb"`
	DefaultStorageGB types.Int64   `tfsdk:"default_storage_gb"`
	DefaultEnvVars   types.Map     `tfsdk:"default_env_vars"`
	ReleaseNotes     types.String  `tfsdk:"release_notes"`
	PublishedAt      types.String  `tfsdk:"published_at"`
}

type appVersionsModel struct {
	App         types.String      `tfsdk:"app"`
	AppID       types.String      `tfsdk:"app_id"`
	AppVersions []appVersionModel `tfsdk:"app_versions"`
}

func (d *appVersionsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "One catalog app's deployable versions, newest first. Set `app` (slug) or `app_id` — exactly one.",
		Attributes: map[string]schema.Attribute{
			"app": schema.StringAttribute{
				Optional:    true,
				Description: "The app's slug — resolved to an id via GET /apps/:slug. Exactly one of app / app_id.",
			},
			"app_id": schema.StringAttribute{
				Optional:    true,
				Description: "The app's id. Exactly one of app / app_id.",
			},
			"app_versions": schema.ListNestedAttribute{
				Computed:    true,
				Description: "The app's versions, newest first.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id": schema.StringAttribute{
							Computed:    true,
							Description: "The app_version id — fluxvale_instance's app_version_id.",
						},
						"app_id": schema.StringAttribute{
							Computed:    true,
							Description: "The owning app's id.",
						},
						"version": schema.StringAttribute{
							Computed:    true,
							Description: "The version string.",
						},
						"image": schema.StringAttribute{
							Computed:    true,
							Description: "The deploy blueprint's image.",
						},
						"port": schema.Int64Attribute{
							Computed:    true,
							Description: "The deploy blueprint's port.",
						},
						"healthcheck_path": schema.StringAttribute{
							Computed:    true,
							Description: "Path the deployer's probes hit.",
						},
						"storage_mount_path": schema.StringAttribute{
							Computed:    true,
							Description: "Where the data PVC mounts.",
						},
						"default_cpu_cores": schema.Float64Attribute{
							Computed:    true,
							Description: "Recommended cpu_cores (the version's default).",
						},
						"default_memory_mb": schema.Int64Attribute{
							Computed:    true,
							Description: "Recommended memory_mb (the version's default).",
						},
						"default_storage_gb": schema.Int64Attribute{
							Computed:    true,
							Description: "Recommended storage_gb (the version's default).",
						},
						"default_env_vars": schema.MapAttribute{
							ElementType: types.StringType,
							Computed:    true,
							Description: "Env vars every deploy of this version ships.",
						},
						"release_notes": schema.StringAttribute{
							Computed:    true,
							Description: "Release notes (null when absent).",
						},
						"published_at": schema.StringAttribute{
							Computed:    true,
							Description: "When the version was published (null when unpublished).",
						},
					},
				},
			},
		},
	}
}

func (d *appVersionsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *appVersionsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	if d.client == nil {
		resp.Diagnostics.AddError(
			"Provider not configured",
			"The provider was not configured before this data source was used — please report this bug.",
		)
		return
	}

	var data appVersionsModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	data.AppVersions = listAppVersions(ctx, d.client, data.App, data.AppID, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// listAppVersions resolves the slug form (app → app id via
// GET /apps/:slug) and maps the versions list. Exactly one input form;
// both or neither is an error.
func listAppVersions(ctx context.Context, c *client.Client, appSlug, appID types.String, diags *diag.Diagnostics) []appVersionModel {
	hasSlug := !appSlug.IsNull() && !appSlug.IsUnknown()
	hasID := !appID.IsNull() && !appID.IsUnknown()
	switch {
	case hasSlug && hasID:
		diags.AddError(
			"Set exactly one of app / app_id",
			"Both \"app\" (slug) and \"app_id\" are set — pick one form.",
		)
		return nil
	case !hasSlug && !hasID:
		diags.AddError(
			"Set app or app_id",
			"Name the app whose versions to list: \"app\" (its slug) or \"app_id\" (its id).",
		)
		return nil
	}

	if !hasID {
		app, err := c.GetAppBySlug(ctx, appSlug.ValueString())
		if err != nil {
			diags.AddError("Unable to resolve the app slug", err.Error())
			return nil
		}
		appID = types.StringValue(app.ID)
	}

	versions, err := c.ListAppVersions(ctx, appID.ValueString())
	if err != nil {
		diags.AddError("Unable to list the app's versions", err.Error())
		return nil
	}

	out := make([]appVersionModel, 0, len(versions))
	for _, v := range versions {
		envVars, d := envVarMapValue(v.DefaultEnvVars)
		diags.Append(d...)
		if diags.HasError() {
			return nil
		}
		out = append(out, appVersionModel{
			ID:               types.StringValue(v.ID),
			AppID:            types.StringValue(v.AppID),
			Version:          types.StringValue(v.Version),
			Image:            types.StringValue(v.Image),
			Port:             types.Int64Value(v.Port),
			HealthcheckPath:  types.StringValue(v.HealthcheckPath),
			StorageMountPath: types.StringValue(v.StorageMountPath),
			DefaultCPUCores:  types.Float64PointerValue(v.DefaultCPUCores),
			DefaultMemoryMB:  types.Int64PointerValue(v.DefaultMemoryMB),
			DefaultStorageGB: types.Int64PointerValue(v.DefaultStorageGB),
			DefaultEnvVars:   envVars,
			ReleaseNotes:     types.StringPointerValue(v.ReleaseNotes),
			PublishedAt:      types.StringPointerValue(v.PublishedAt),
		})
	}
	return out
}

// envVarMapValue wraps a plain string map into a types.Map.
func envVarMapValue(m map[string]string) (types.Map, diag.Diagnostics) {
	if m == nil {
		return types.MapNull(types.StringType), diag.Diagnostics{}
	}
	elems := make(map[string]types.String, len(m))
	for k, v := range m {
		elems[k] = types.StringValue(v)
	}
	return types.MapValueFrom(context.Background(), types.StringType, elems)
}
