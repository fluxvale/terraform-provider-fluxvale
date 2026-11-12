package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/fluxvale/terraform-provider-fluxvale/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ datasource.DataSource = (*instanceDataSource)(nil)

// instanceDataSource is fluxvale_instance — read one existing instance
// by id or by slug (#731 T2). The id form is unique; the slug form
// carries an optional organization context because a slug is unique
// only per [:slug, :cluster_id] (GET /api/v1/instances/by-slug/:slug).
type instanceDataSource struct {
	pd *providerData
}

// NewInstanceDataSource returns the data-source factory provider.go
// registers.
func NewInstanceDataSource() datasource.DataSource {
	return &instanceDataSource{}
}

func (d *instanceDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_instance"
}

// instanceDataSourceModel mirrors the schema. id/slug are the two
// lookup forms (exactly one configured) and both computed back from
// the wire; organization is the slug form's org context.
type instanceDataSourceModel struct {
	ID             types.String `tfsdk:"id"`
	Slug           types.String `tfsdk:"slug"`
	Organization   types.String `tfsdk:"organization"`
	Name           types.String `tfsdk:"name"`
	Namespace      types.String `tfsdk:"namespace"`
	Status         types.String `tfsdk:"status"`
	Exposure       types.String `tfsdk:"exposure"`
	AlwaysOn       types.Bool   `tfsdk:"always_on"`
	Image          types.String `tfsdk:"image"`
	Port           types.Int64  `tfsdk:"port"`
	DeployedAt     types.String `tfsdk:"deployed_at"`
	OrganizationID types.String `tfsdk:"organization_id"`
	AppVersionID   types.String `tfsdk:"app_version_id"`
}

func (d *instanceDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "One existing instance, looked up by `id` or by `slug` — exactly one. The slug form takes an optional `organization` context because a slug is unique only within a cluster.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "The instance id. Exactly one of id / slug.",
			},
			"slug": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "The instance's slug (its DNS subdomain). Exactly one of id / slug.",
			},
			"organization": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "The slug lookup's organization context — its id or slug. Falls back to the provider-level " +
					"`organization`. Ignored for an id lookup (an id is unique). With neither set, the lookup runs " +
					"unfiltered.",
			},
			"name": schema.StringAttribute{
				Computed:    true,
				Description: "The instance's display name.",
			},
			"namespace": schema.StringAttribute{
				Computed:    true,
				Description: "The fluxvale-app-<id> namespace (null until deployed).",
			},
			"status": schema.StringAttribute{
				Computed:    true,
				Description: "The state machine's status.",
			},
			"exposure": schema.StringAttribute{
				Computed:    true,
				Description: "\"public\" or \"private\".",
			},
			"always_on": schema.BoolAttribute{
				Computed:    true,
				Description: "Whether the instance opts out of scale-to-zero.",
			},
			"image": schema.StringAttribute{
				Computed:    true,
				Description: "The app version's image, set at create (not deploy-derived).",
			},
			"port": schema.Int64Attribute{
				Computed:    true,
				Description: "The app version's port, set at create (not deploy-derived).",
			},
			"deployed_at": schema.StringAttribute{
				Computed:    true,
				Description: "When the current deploy was enqueued (null before the first deploy).",
			},
			"organization_id": schema.StringAttribute{
				Computed:    true,
				Description: "The owning organization's id (requested via the sparse fieldset).",
			},
			"app_version_id": schema.StringAttribute{
				Computed:    true,
				Description: "The instance's app version id, set at create (requested via the sparse fieldset).",
			},
		},
	}
}

func (d *instanceDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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
	d.pd = pd
}

func (d *instanceDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	if d.pd == nil || d.pd.client == nil {
		resp.Diagnostics.AddError(
			"Provider not configured",
			"The provider was not configured before this data source was used — please report this bug.",
		)
		return
	}

	var data instanceDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	id, hasID := nonEmptyValue(data.ID)
	slug, hasSlug := nonEmptyValue(data.Slug)
	if hasID == hasSlug {
		if hasID {
			resp.Diagnostics.AddError(
				"Set exactly one of id / slug",
				"Both \"id\" and \"slug\" are set — pick one lookup form.",
			)
		} else {
			resp.Diagnostics.AddError(
				"Set exactly one of id / slug",
				"Name the instance to read: \"id\" (its id) or \"slug\" (with \"organization\" when the slug is not unique across your organizations).",
			)
		}
		return
	}

	var (
		in     client.Instance
		err    error
		lookup string
	)
	if hasID {
		// An id is unique — never resolve or send the organization
		// attribute on this form.
		lookup = fmt.Sprintf("id %q", id)
		in, err = d.pd.client.GetInstance(ctx, id)
	} else {
		orgSpec, hasOrg := nonEmptyValue(data.Organization)
		providerOrg, hasProviderOrg := nonEmptyValue(d.pd.organization)
		orgID := ""
		// Resolve the org only when the caller gave a non-empty one
		// (attribute or provider default). With neither, the lookup runs
		// unfiltered: the server answers from the token's memberships
		// and names the remedy if the slug still matches more than one
		// row. Empty strings are unset, exactly like null.
		if hasOrg || hasProviderOrg {
			orgID = resolveOrgID(ctx, d.pd.client, optionalString(orgSpec), optionalString(providerOrg), &resp.Diagnostics)
			if resp.Diagnostics.HasError() {
				return
			}
		}
		lookup = fmt.Sprintf("slug %q", slug)
		if hasOrg {
			lookup += fmt.Sprintf(" in organization %q", orgSpec)
		} else if hasProviderOrg {
			lookup += fmt.Sprintf(" in organization %q", providerOrg)
		}
		in, err = d.pd.client.GetInstanceBySlug(ctx, slug, orgID)
	}
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) {
			switch {
			case apiErr.Status == http.StatusNotFound:
				resp.Diagnostics.AddError(
					"Instance not found",
					fmt.Sprintf("No instance with %s is visible to this token.", lookup),
				)
				return
			case apiErr.Status == http.StatusBadRequest && apiErrorHasCode(apiErr, "multiple_results"):
				resp.Diagnostics.AddError(
					"Ambiguous instance slug",
					fmt.Sprintf("More than one visible instance matches %s. Set \"organization\" to narrow the lookup, or use \"id\" instead. Server: %v",
						lookup, apiErr),
				)
				return
			}
		}
		resp.Diagnostics.AddError("Unable to read the Instance", err.Error())
		return
	}

	mapInstanceDataSource(&data, in)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// mapInstanceDataSource writes the wire's detail fields into the data
// source model. A pending row's namespace/deployed_at nulls map to
// null types values; FK ids absent from the response map to null too
// (the sparse fieldset asks for them, but an older server may omit
// them — a present-but-wrong-typed value is shape drift upstream).
func mapInstanceDataSource(data *instanceDataSourceModel, in client.Instance) {
	data.ID = types.StringValue(in.ID)
	data.Name = types.StringValue(in.Name)
	data.Slug = types.StringValue(in.Slug)
	data.Namespace = types.StringPointerValue(in.Namespace)
	data.Status = types.StringValue(in.Status)
	data.Exposure = types.StringValue(in.Exposure)
	data.AlwaysOn = types.BoolValue(in.AlwaysOn)
	data.Image = types.StringPointerValue(in.Image)
	data.Port = types.Int64PointerValue(in.Port)
	data.DeployedAt = types.StringPointerValue(in.DeployedAt)
	data.OrganizationID = types.StringPointerValue(stringPointer(in.OrganizationID))
	data.AppVersionID = types.StringPointerValue(stringPointer(in.AppVersionID))
}

// nonEmptyValue returns a configured string's trimmed value and
// whether it is set, known, and non-empty. Empty strings (and
// whitespace-only strings) are unset: `id = ""` must not request the
// collection and `organization = ""` must not go through org
// resolution.
func nonEmptyValue(v types.String) (string, bool) {
	if v.IsNull() || v.IsUnknown() {
		return "", false
	}
	s := strings.TrimSpace(v.ValueString())
	return s, s != ""
}

// optionalString maps an already-trimmed value back to a types.String
// for resolveOrgID — empty is null.
func optionalString(s string) types.String {
	if s == "" {
		return types.StringNull()
	}
	return types.StringValue(s)
}

// apiErrorHasCode reports whether the API's errors array carries code.
func apiErrorHasCode(apiErr *client.APIError, code string) bool {
	for _, e := range apiErr.Errors {
		if e.Code == code {
			return true
		}
	}
	return false
}
