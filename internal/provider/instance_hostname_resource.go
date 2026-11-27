package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

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
	_ resource.Resource                = (*instanceHostnameResource)(nil)
	_ resource.ResourceWithImportState = (*instanceHostnameResource)(nil)
)

// instanceHostnameResource manages fluxvale_instance_hostname — one
// custom address for an instance: a label (e.g. `git`) minted under a
// verified custom domain the instance's organization owns, giving
// git.<your-domain>. A skin over the mint/list/remove surface: the
// identity (instance, domain, label) is create-only, every change
// replaces the row; the mint's row-stands refusals are probed and
// adopted; every other refusal surfaces verbatim. Whether the platform
// serves the address is the platform's rule, not this resource's.
type instanceHostnameResource struct {
	pd *providerData
}

// NewInstanceHostnameResource returns the resource factory provider.go
// registers.
func NewInstanceHostnameResource() resource.Resource {
	return &instanceHostnameResource{}
}

func (r *instanceHostnameResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_instance_hostname"
}

// instanceHostnameModel mirrors the schema. The three identity fields
// are create-only (RequiresReplace — the member API has no move
// route); id is the hostname row id the remove route takes.
type instanceHostnameModel struct {
	InstanceID     types.String `tfsdk:"instance_id"`
	AccessDomainID types.String `tfsdk:"access_domain_id"`
	Label          types.String `tfsdk:"label"`
	ID             types.String `tfsdk:"id"`
}

func (r *instanceHostnameResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "One custom address for an instance — a label under a verified custom domain the instance's organization owns, minted through the member API and recorded as one platform row. The identity (instance, domain, label) is create-only: every change replaces the row. Whether the platform serves the address is the platform's rule, not this resource's — see the resource's docs.",
		Attributes: map[string]schema.Attribute{
			"instance_id": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The instance this address belongs to — a `fluxvale_instance.<name>.id`. The member API has no " +
					"move route, so the binding is create-only: changing it replaces the row (never the instance). Minting " +
					"against an instance this token cannot see answers the platform's 404, surfaced verbatim.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"access_domain_id": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The access-domain id of a verified custom domain your instance's organization owns — a " +
					"`fluxvale_custom_domain.<name>.id`. The platform refuses a platform domain " +
					"(`only a custom domain can host a custom address`), an unverified one " +
					"(`verify the domain before adding addresses to it`), and one from another organization " +
					"(`that domain belongs to a different organization`) — all 400s surfaced verbatim. " +
					"Create-only: changing it replaces the row.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"label": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The DNS label below the domain — `git` serves `git.<your-domain>`. The platform enforces its " +
					"grammar server-side (lowercase alphanumerics with inner hyphens, 1-63 characters, first and last " +
					"alphanumeric; a violating 400 surfaces verbatim — the provider adds no plan-time check by design). " +
					"Labels are unique per domain, and because domain names are unique platform-wide, label + domain is an " +
					"address unique across the whole platform; minting the same label under the same domain again is refused " +
					"(a 400 or 409 — create adopts a standing identical row instead of failing). " +
					"Create-only: changing it replaces the row.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "The hostname row id — the remove path's key, and the second half of the import id.",
			},
		},
	}
}

func (r *instanceHostnameResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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
func (r *instanceHostnameResource) data(diags *diag.Diagnostics) *providerData {
	if r.pd == nil {
		diags.AddError(
			"Provider not configured",
			"The provider was not configured before this resource was used — please report this bug.",
		)
	}
	return r.pd
}

func (r *instanceHostnameResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data instanceHostnameModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	pd := r.data(&resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	hostname, err := pd.client.MintInstanceHostname(ctx, client.MintInstanceHostnameInput{
		InstanceID:     data.InstanceID.ValueString(),
		AccessDomainID: data.AccessDomainID.ValueString(),
		Label:          data.Label.ValueString(),
	})
	if err != nil {
		// The platform's row-stands refusals (a refused app restart, a
		// failed env delivery — the two "The address was added…" 400s —
		// and the duplicate-on-this-instance case) would otherwise
		// strand a live row outside state: the next apply would re-mint
		// into the duplicate refusal. The probe adopts the row only
		// when the same domain and label stand on the same instance —
		// an identical row, indistinguishable from ours. The trigger is
		// the status class (400 or 409 — the platform's own route test
		// accepts both behind a standing duplicate, so the status is
		// not pinned), never the sentence: the platform's copy may
		// drift; its status classes are the contract.
		var apiErr *client.APIError
		if errors.As(err, &apiErr) &&
			(apiErr.Status == http.StatusBadRequest || apiErr.Status == http.StatusConflict) {
			rows, lerr := pd.client.ListInstanceHostnames(ctx, data.InstanceID.ValueString())
			if lerr == nil {
				for _, row := range rows {
					if row.AccessDomainID == data.AccessDomainID.ValueString() &&
						row.Label == data.Label.ValueString() {
						mapInstanceHostname(&data, row)
						resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
						resp.Diagnostics.AddWarning(
							"The custom address stands despite the mint's refusal",
							fmt.Sprintf("The mint answered %s — but the same address (domain %s, label %s) already stands "+
								"on instance %s: an identical row, adopted into state as if the mint had succeeded.",
								err.Error(), data.AccessDomainID.ValueString(), data.Label.ValueString(), data.InstanceID.ValueString()),
						)
						return
					}
				}
			}
		}
		// Any other error (the 403 / 404 fences, 5xx) — and a 400/409
		// whose probe found no standing row: the original refusal, the
		// server's wording verbatim. Nothing landed (or nothing is
		// ours) — no state.
		resp.Diagnostics.AddError("Unable to add the custom address", err.Error())
		return
	}

	mapInstanceHostname(&data, hostname)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// mapInstanceHostname writes the wire's fields into the model — the
// wire is the truth, so a drifted identity shows as a replacement in
// the framework's next plan (all three identity attributes are
// RequiresReplace).
func mapInstanceHostname(data *instanceHostnameModel, row client.InstanceHostname) {
	data.ID = types.StringValue(row.ID)
	data.InstanceID = types.StringValue(row.InstanceID)
	data.AccessDomainID = types.StringValue(row.AccessDomainID)
	data.Label = types.StringValue(row.Label)
}

// readInstanceHostname refreshes the model from the instance's list
// and returns true when the row is gone (the caller removes it from
// state). The list has no 404 class — a foreign or gone instance
// answers 200 [], so row-absence is the only "gone" signal. A null
// label/access_domain_id right after import is filled from the wire.
func readInstanceHostname(ctx context.Context, pd *providerData, data *instanceHostnameModel, diags *diag.Diagnostics) bool {
	rows, err := pd.client.ListInstanceHostnames(ctx, data.InstanceID.ValueString())
	if err != nil {
		diags.AddError("Unable to read the custom address", err.Error())
		return false
	}
	for _, row := range rows {
		if row.ID == data.ID.ValueString() {
			mapInstanceHostname(data, row)
			return false
		}
	}
	return true
}

func (r *instanceHostnameResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data instanceHostnameModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	pd := r.data(&resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	if readInstanceHostname(ctx, pd, &data, &resp.Diagnostics) {
		// The platform removed the row (or the instance, whose list
		// answers empty). Gone is gone; the next apply re-mints.
		resp.State.RemoveResource(ctx)
		return
	}
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// Update is the defensive re-read no-op (the custom-domain stance):
// every settable attribute forces replacement, so an in-place update
// never occurs in a real config — nothing here mutates. The identity
// is carried from STATE before anything reads by it (a plan unknown
// for the Computed id must not drive the list), and a gone row
// removes from state — never a clean apply over a ghost.
func (r *instanceHostnameResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state instanceHostnameModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	pd := r.data(&resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	plan.ID = state.ID
	plan.InstanceID = state.InstanceID

	if readInstanceHostname(ctx, pd, &plan, &resp.Diagnostics) {
		resp.State.RemoveResource(ctx)
		return
	}
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *instanceHostnameResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state instanceHostnameModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	pd := r.data(&resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	// One synchronous remove — nothing to poll. The 404 is already
	// gone: a panel remove, or the row cascaded when the instance was
	// destroyed (the platform deletes hostname rows with their
	// instance) — idempotent destroy. The platform-row 400 (the
	// default address is not removable — an imported platform-minted
	// row is inspectable, never removable, the same stance
	// fluxvale_custom_domain takes for kind = "platform" rows)
	// surfaces verbatim and keeps state.
	if err := pd.client.RemoveInstanceHostname(ctx, state.ID.ValueString()); err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			resp.State.RemoveResource(ctx) // already gone
			return
		}
		resp.Diagnostics.AddError("Unable to remove the custom address", err.Error())
		return
	}
	resp.State.RemoveResource(ctx)
}

func (r *instanceHostnameResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// By <instance_id>/<hostname_id>: the read surface is
	// instance-scoped (there is no GET by hostname id), so the import
	// names the instance to list under and the row id to find. Read
	// then fills label and access_domain_id from the list.
	left, right, found := strings.Cut(req.ID, "/")
	if !found || left == "" || right == "" || strings.Contains(right, "/") {
		resp.Diagnostics.AddError(
			"Invalid import id",
			"expected <instance_id>/<hostname_id> — the instance whose list carries the row, then the row id "+
				"(e.g. 0198…/01a20b03-…), separated by exactly one slash",
		)
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("instance_id"), left)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), right)...)
}
