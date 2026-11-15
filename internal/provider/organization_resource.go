package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/fluxvale/terraform-provider-fluxvale/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.ResourceWithImportState = (*organizationResource)(nil)
	_ resource.ResourceWithModifyPlan  = (*organizationResource)(nil)
)

// organizationResource manages fluxvale_organization — a team
// organization, the ownership and billing boundary. Create makes the
// authenticated user its owner (the API's actor path); it does not
// fund the org, so the first deploy still meets the top-up gate.
// Name updates in place. Slug is create-only in this provider: the
// machine API's rename is name-only, so a changed slug is an address
// the API cannot set, and the provider rejects the replacement plan
// instead of destroying the org. Destroy deletes an
// EMPTY org only; the platform's empty-org guard refuses anything
// else with its own message.
type organizationResource struct {
	pd *providerData
}

// NewOrganizationResource returns the resource factory provider.go
// registers.
func NewOrganizationResource() resource.Resource {
	return &organizationResource{}
}

func (r *organizationResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_organization"
}

// organizationModel mirrors the schema.
type organizationModel struct {
	Name types.String `tfsdk:"name"`
	Slug types.String `tfsdk:"slug"`
	ID   types.String `tfsdk:"id"`
}

func (r *organizationResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A team organization — the ownership and billing boundary for every instance, domain, and gateway. Creating it makes the authenticated user its owner; it does not fund the org, so the first deploy still meets the top-up gate. Destroy deletes an EMPTY org only; an org that still owns anything, or has ever had a wallet row (created by the first org or billing page visit, a top-up, or a deploy), is refused with the platform's message. Set `lifecycle { prevent_destroy = true }` on any org you care about.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required:    true,
				Description: "The organization's display name (at most 100 characters). Updates in place.",
			},
			"slug": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The organization's address: 2–39 characters of lowercase letters, digits, and hyphens, " +
					"starting and ending with a letter or digit (`^[a-z0-9][a-z0-9-]*[a-z0-9]$`). " +
					"Create-only in this provider — the machine API's rename is name-only. Changing it plans a replacement, " +
					"and the provider rejects that plan. Rename the address in the panel, then `terraform state rm` and " +
					"`terraform import <address> <new-slug>`. The account may own at most 10 organizations (personal org included). " +
					"Reserved brand slugs are refused by the platform for every token, platform admins included.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "The organization id — the refresh/update/delete path key.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
		},
	}
}

func (r *organizationResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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
	r.pd = pd
}

// data returns the configured providerData or a diagnostic — every
// CRUD path starts here.
func (r *organizationResource) data(diags *diag.Diagnostics) *providerData {
	if r.pd == nil {
		diags.AddError(
			"Provider not configured",
			"The provider was not configured before this resource was used — please report this bug.",
		)
	}
	return r.pd
}

func (r *organizationResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data organizationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	pd := r.data(&resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	// No resolveOrgID: the org resource is not org-scoped, and the
	// provider-level "organization" default does not apply to it.
	org, err := pd.client.CreateOrganization(ctx, client.CreateOrganizationInput{
		Name: data.Name.ValueString(),
		Slug: data.Slug.ValueString(),
	})
	if err != nil {
		resp.Diagnostics.AddError("Unable to create the organization", err.Error())
		return
	}
	mapOrganization(&data, org)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *organizationResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data organizationModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	pd := r.data(&resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	if readOrganization(ctx, pd, &data, &resp.Diagnostics) {
		resp.State.RemoveResource(ctx)
		return
	}
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// readOrganization reads by id, or resolves the import slug when the
// id is not known yet. A 404 removes from state: the platform hides a
// non-member's org behind not-found (anti-enumeration), so a deleted
// org and a revoked membership look identical. The next apply then
// plans a create — which fails if the slug is still taken. A 403
// (missing organizations:read) is an error and keeps state; a scope
// problem must never forget the billing boundary.
func readOrganization(ctx context.Context, pd *providerData, data *organizationModel, diags *diag.Diagnostics) bool {
	var (
		org client.Organization
		err error
	)

	if data.ID.IsNull() || data.ID.IsUnknown() {
		org, err = pd.client.GetOrganizationBySlug(ctx, data.Slug.ValueString())
	} else {
		org, err = pd.client.GetOrganization(ctx, data.ID.ValueString())
	}

	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return true
		}
		diags.AddError("Unable to read the organization", err.Error())
		return false
	}
	mapOrganization(data, org)
	return false
}

func mapOrganization(data *organizationModel, org client.Organization) {
	data.ID = types.StringValue(org.ID)
	data.Name = types.StringValue(org.Name)
	data.Slug = types.StringValue(org.Slug)
}

// ModifyPlan rejects a slug change up front. A replacement's destroy
// half either fails (non-empty org) or deletes the billing boundary;
// neither is a plan a config file should be able to request. The check
// is value-based: `tofu apply -replace=...` and a slug unknown at plan
// time still plan a replacement, so the docs state that limit.
func (r *organizationResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	// Create planning (no state) and destroy planning (no plan) have no
	// slug to compare.
	if req.Plan.Raw.IsNull() || req.State.Raw.IsNull() {
		return
	}

	var plan, state organizationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if plan.Slug.IsUnknown() || plan.Slug.Equal(state.Slug) {
		return
	}

	resp.Diagnostics.AddError(
		"Replacing fluxvale_organization is rejected — a slug change would destroy the organization",
		fmt.Sprintf("Terraform's state has slug %q; your configuration asks for %q. A non-empty org's destroy is refused, "+
			"and an empty one would still lose its id and billing history. Set slug = %q. "+
			"To follow a panel rename: terraform state rm <this resource's address>, then "+
			"terraform import <this resource's address> %s.",
			state.Slug.ValueString(), plan.Slug.ValueString(), state.Slug.ValueString(), state.Slug.ValueString()),
	)
}

func (r *organizationResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state organizationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	pd := r.data(&resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	// id is Computed; a name-only change plans it UNKNOWN on some
	// paths — carry the stored identity before using it.
	plan.ID = state.ID

	org, err := pd.client.UpdateOrganization(ctx, plan.ID.ValueString(), plan.Name.ValueString())
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Unable to update the organization", err.Error())
		return
	}
	mapOrganization(&plan, org)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *organizationResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state organizationModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	pd := r.data(&resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := pd.client.DeleteOrganization(ctx, state.ID.ValueString()); err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Unable to delete the organization", err.Error())
		return
	}
	resp.State.RemoveResource(ctx)
}

func (r *organizationResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// By slug — the org's natural handle: terraform import
	// fluxvale_organization.team acme. Read resolves it and fills id.
	resource.ImportStatePassthroughID(ctx, path.Root("slug"), req, resp)
}
