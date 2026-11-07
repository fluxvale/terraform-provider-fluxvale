package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/fluxvale/fluxvale/apps/tfprovider/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.ResourceWithImportState = (*gatewayResource)(nil)
	_ resource.ResourceWithModifyPlan  = (*gatewayResource)(nil)
)

// gatewayResource manages fluxvale_access_gateway — the org's
// singleton private-access gateway. A skin over the routed surface:
// create, the singleton read, and the attachment pairing verbs. The
// API has NO gateway-delete route (the scope table's whole gateway
// section), so destroy is remove-from-state with an honest warning —
// which is also why replacements are rejected at plan time: their
// create half would conflict with the row destroy leaves behind.
type gatewayResource struct {
	pd *providerData
}

// NewGatewayResource returns the resource factory provider.go
// registers.
func NewGatewayResource() resource.Resource {
	return &gatewayResource{}
}

func (r *gatewayResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_access_gateway"
}

// gatewayModel mirrors the schema. The write-only auth_key is never
// overwritten from the wire; organization is written back only when
// the config leaves it null (the import-convergence seed); instances
// is config-authoritative — the wire has no attachment list, so reads
// keep the configured set verbatim.
type gatewayModel struct {
	Organization   types.String `tfsdk:"organization"`
	TailnetDomain  types.String `tfsdk:"tailnet_domain"`
	AuthKey        types.String `tfsdk:"auth_key"`
	Instances      types.Set    `tfsdk:"instances"`
	ID             types.String `tfsdk:"id"`
	OrganizationID types.String `tfsdk:"organization_id"`
	Hostname       types.String `tfsdk:"hostname"`
	AccessProvider types.String `tfsdk:"access_provider"`
	State          types.String `tfsdk:"state"`
	StatusMessage  types.String `tfsdk:"status_message"`
	MonthlyCents   types.Int64  `tfsdk:"monthly_cents"`
	ProvisionedAt  types.String `tfsdk:"provisioned_at"`
}

func (r *gatewayResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "The organization's private-access gateway — the always-on pod that puts the org's private instances on your tailnet. One per organization. Destroying this resource removes it from state only: the API exposes no gateway-delete route, so the platform keeps the gateway running (and billing). Replacements are therefore rejected at plan time — they cannot succeed (the identity attributes are create-only, and the destroy half leaves the row behind).",
		Attributes: map[string]schema.Attribute{
			"organization": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Organization to enable the gateway in — its id or slug. Falls back to the provider-level " +
					"`organization`, then to your sole organization (several memberships without a name is an error). " +
					"The resolved id is written back when the config leaves this unset, so imports converge. A config value " +
					"must keep matching what's stored — a mismatch plans a replacement, and replacements are rejected " +
					"(the API has no org-change route; opt out via the panel's Access tab and re-create).",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"tailnet_domain": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Your tailnet's MagicDNS suffix (`<tailnet>.ts.net`) — visible in your Tailscale admin " +
					"console on every device name. Create-only (the API's only PATCH verb is resume), so changing it plans " +
					"a replacement, and replacements are rejected: opt out via the panel's Access tab, then " +
					"`terraform apply -replace=…` (or re-add the resource).",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			// Optional rather than the framework's WriteOnly: write-only
			// attributes need Terraform CLI ≥ 1.11 in every consumer and
			// would forfeit the kept-verbatim-in-state semantics this
			// provider documents for write-only fields (env_vars,
			// secret_access_key). Same contract, cheaper: Optional +
			// Sensitive + the create-time check below.
			"auth_key": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "The Tailscale auth key the gateway joins your tailnet with — required when this " +
					"resource ENABLES a gateway (a fresh create). Write-only: the API never echoes it, so an import " +
					"leaves it null and the provider manages attachments only. The configured value is kept verbatim in " +
					"state. Rotation has no API route (the panel's re-paste is the path), so changing it plans a " +
					"replacement, and replacements are rejected.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"instances": schema.SetAttribute{
				ElementType: types.StringType,
				Optional:    true,
				MarkdownDescription: "Instance ids to attach — one attach/detach call per added/removed id. Attach needs a " +
					"PRIVATE, deployed instance in the same org and a gateway the platform is willing to pair (a suspended " +
					"or tearing-down gateway is refused, the platform's wording surfacing verbatim). Before the first " +
					"attach the provider waits for `active` — stricter than the wire, which also accepts `provisioning`, " +
					"because a provisioning row can flip to `needs_attention` under the attach and 400 the pairing. " +
					"Config-authoritative: the API has no attachment list, so reads keep the configured set and " +
					"out-of-band attach/detach (the panel) is invisible to the provider.",
			},
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "The gateway id — the attachment verbs' path key.",
			},
			"organization_id": schema.StringAttribute{
				Computed:    true,
				Description: "The org the gateway serves (the wire's echo of the resolution).",
			},
			"hostname": schema.StringAttribute{
				Computed:    true,
				Description: "The opaque fv-<8 hex> node name — minted at create, never derived from org or app names.",
			},
			"access_provider": schema.StringAttribute{
				Computed:    true,
				Description: "The access provider (tailscale) — the API field is `provider`, reserved in Terraform schemas, so the attribute is `access_provider`.",
			},
			"state": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The gateway's state machine: provisioning → active (the observer promotes within about a " +
					"minute); needs_attention, suspended, destroying possible. A suspended gateway is the zero-balance " +
					"ladder's out-of-band platform state — resume via the panel or MCP (`resume_gateway`); this resource " +
					"deliberately does not manage it.",
			},
			"status_message": schema.StringAttribute{
				Computed:    true,
				Description: "The panel's friendly copy for the current state (null when there is nothing to say).",
			},
			"monthly_cents": schema.Int64Attribute{
				Computed:    true,
				Description: "The live monthly rate in cents — snapshotted at create from the platform's config (never caller-supplied).",
			},
			"provisioned_at": schema.StringAttribute{
				Computed:    true,
				Description: "When the cluster objects were provisioned (null until then).",
			},
		},
	}
}

func (r *gatewayResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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
func (r *gatewayResource) data(diags *diag.Diagnostics) *providerData {
	if r.pd == nil {
		diags.AddError(
			"Provider not configured",
			"The provider was not configured before this resource was used — please report this bug.",
		)
	}
	return r.pd
}

// ModifyPlan rejects any planned replacement up front. Destroy is a
// state-only forget (no API delete route), so a replacement's create
// half always ends unique-organization-conflicted against the row the
// destroy half left running (and billing) — the plan would apply into
// a guaranteed error. The real path is the panel opt-out, then
// terraform apply -replace=… (or re-adding the resource).
func (r *gatewayResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	// Create planning (no state) and destroy planning (no plan) have no
	// replacement to compare.
	if req.Plan.Raw.IsNull() || req.State.Raw.IsNull() {
		return
	}

	var plan, state gatewayModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// An unknown value can't be compared yet — the apply will tell.
	var changed []string
	if !plan.Organization.IsUnknown() && !plan.Organization.Equal(state.Organization) {
		changed = append(changed, "organization")
	}
	if !plan.TailnetDomain.IsUnknown() && !plan.TailnetDomain.Equal(state.TailnetDomain) {
		changed = append(changed, "tailnet_domain")
	}
	if !plan.AuthKey.IsUnknown() && !plan.AuthKey.Equal(state.AuthKey) {
		changed = append(changed, "auth_key")
	}
	if len(changed) == 0 {
		return
	}

	resp.Diagnostics.AddError(
		"Replacing fluxvale_access_gateway is rejected — the replacement cannot succeed",
		fmt.Sprintf("%s changed, which plans a replacement. The API exposes no gateway-delete route, so the destroy half "+
			"only forgets this resource from state while the gateway keeps running (and billing), and the create half "+
			"then hits the organization's one-gateway limit against that row. Real path: opt out via the panel's "+
			"Access tab, then terraform apply -replace=<this resource's address> (or re-add the resource).",
			strings.Join(changed, ", ")),
	)
}

func (r *gatewayResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data gatewayModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	pd := r.data(&resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	// Enabling a gateway needs the key: the wire takes auth_key only at
	// create and never echoes it, so there is no later save to supply
	// it (an import with no auth_key manages attachments only — that is
	// the documented shape, not an error).
	if data.AuthKey.IsNull() || data.AuthKey.IsUnknown() {
		resp.Diagnostics.AddError(
			"auth_key is required to enable a gateway",
			"The gateway joins your tailnet with a Tailscale auth key, and the API accepts it only at create. "+
				"Set auth_key in your config — or import the existing gateway instead of creating one.",
		)
		return
	}

	orgID := resolveOrgID(ctx, pd.client, data.Organization, pd.organization, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	seedOrganization(&data, orgID)

	gw, err := pd.client.CreateGateway(ctx, client.CreateGatewayInput{
		OrganizationID: orgID,
		TailnetDomain:  data.TailnetDomain.ValueString(),
		AuthKey:        data.AuthKey.ValueString(),
	})
	if err != nil {
		// The likeliest refusal is the org's unique-organization
		// identity — the singleton may already exist. Probe before
		// failing: a matching row is adopted, a diverging one is named.
		if !adoptExistingGateway(ctx, pd, orgID, &data, err, &resp.Diagnostics) {
			return
		}
	} else {
		mapGateway(&data, gw)
	}

	// The platform now has (or already had) this gateway. Tainted
	// tracked state beats an untracked leak (the instance resource's
	// post-POST stance): write what we know so every later failure —
	// set conversion, wait timeout, attach refusal — leaves a tracked
	// resource the next apply updates or forgets. mapGateway fills
	// every computed field, so no unknowns remain to clear.
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Attach needs a serving gateway — create lands :provisioning and
	// the observer (a one-minute cadence) is the only promoter to
	// :active, so the attach sequence waits it out first.
	instances, d := instanceSet(ctx, data.Instances)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	applyInstanceDiff(ctx, pd, orgID, data.ID.ValueString(), nil, instances, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	// Final read for state — the create response omits provisioned_at,
	// and the wait (when one ran) just saw the row move; linger-long
	// :provisioning state misdescribes a serving gateway.
	if readGateway(ctx, pd, &data, &resp.Diagnostics) {
		resp.Diagnostics.AddError(
			"Access gateway disappeared during creation",
			fmt.Sprintf("gateway %s was torn down between create and its final read — refresh and re-apply", data.ID.ValueString()),
		)
		return
	}
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// adoptExistingGateway recovers a POST the org's singleton refused.
// A row whose create-only fields match the config IS the gateway this
// create asked for — adopt it with a warning (auth_key can't be
// verified: write-only). A diverging row is named, with the only real
// remediation. Returns false when the caller must stop (a diagnostic
// is already set — either the original POST error when no row exists,
// or the divergence).
func adoptExistingGateway(ctx context.Context, pd *providerData, orgID string, data *gatewayModel, createErr error, diags *diag.Diagnostics) bool {
	gw, err := pd.client.GetOrgGateway(ctx, orgID)
	if err != nil {
		// No row (or unreadable) — nothing to adopt; the POST's own
		// error is the truth.
		diags.AddError("Unable to create the access gateway", createErr.Error())
		return false
	}
	if gw.TailnetDomain != data.TailnetDomain.ValueString() {
		diags.AddError(
			"The organization already has a different access gateway",
			fmt.Sprintf("gateway %s serves tailnet %q; the config asks for %q. The API has no gateway-delete route: "+
				"opt out via the panel's Access tab, then apply again.",
				gw.ID, gw.TailnetDomain, data.TailnetDomain.ValueString()),
		)
		return false
	}
	diags.AddWarning(
		"Adopted the organization's existing gateway",
		fmt.Sprintf("The create was refused but gateway %s (%s) already serves tailnet %q as the config asks — "+
			"Terraform now manages it. auth_key cannot be verified (the API never echoes it): the panel's re-paste "+
			"is the recovery if the key ever needs rotating.",
			gw.ID, gw.Hostname, gw.TailnetDomain),
	)
	mapGateway(data, gw)
	return true
}

// applyInstanceDiff converges the attachments onto the configured set:
// one POST per added id, one DELETE per removed id. A non-empty add
// side first waits the gateway to :active — stricter than the wire
// (attach_instance.ex gates only `state not in [:active, :provisioning]`),
// because a provisioning row can flip to needs_attention under the
// attach and 400 the pairing (`gateway_not_active`): sequencing on the
// observer's own promotion attaches once, against a serving gateway.
func applyInstanceDiff(ctx context.Context, pd *providerData, orgID, gatewayID string, current, configured []string, diags *diag.Diagnostics) {
	added, removed := instanceSetDiff(current, configured)

	if len(added) > 0 {
		waitGatewayActive(ctx, pd, orgID, diags)
		if diags.HasError() {
			return
		}
	}
	for _, id := range added {
		if err := pd.client.AttachGatewayInstance(ctx, gatewayID, id); err != nil {
			diags.AddError("Unable to attach instance "+id, fmt.Sprintf("gateway %s: %v", gatewayID, err))
			return
		}
	}
	for _, id := range removed {
		if err := pd.client.DetachGatewayInstance(ctx, gatewayID, id); err != nil {
			diags.AddError("Unable to detach instance "+id, fmt.Sprintf("gateway %s: %v", gatewayID, err))
			return
		}
	}
}

// waitGatewayActive polls the singleton read until the gateway serves.
// needs_attention, suspended, and destroying are terminal for the wait
// — each with its own remediation; status_message is the whole story
// the API tells (the instance deploy wait's stance).
func waitGatewayActive(ctx context.Context, pd *providerData, orgID string, diags *diag.Diagnostics) {
	var lastState string
	err := pollWithDeadline(ctx, pd.pollInterval, pd.pollTimeout, func(ctx context.Context) (bool, error) {
		gw, err := pd.client.GetOrgGateway(ctx, orgID)
		if err != nil {
			return false, err
		}
		lastState = gw.State
		switch gw.State {
		case "active":
			return true, nil
		case "needs_attention":
			msg := "no message"
			if gw.StatusMessage != nil {
				msg = *gw.StatusMessage
			}
			return false, fmt.Errorf("the gateway landed in needs_attention: %s", msg)
		case "suspended":
			return false, errors.New("the zero-balance ladder suspended the gateway — resume via the panel's Access tab " +
				"or MCP (resume_gateway); the provider deliberately does not expose resume")
		case "destroying":
			return false, errors.New("the gateway is being torn down (an opt-out landed) — let it finish, then re-create")
		}
		return false, nil
	})
	if err != nil {
		diags.AddError(
			"Access gateway did not become active",
			fmt.Sprintf("gateway for org %s: %v (last state %q)", orgID, err, lastState),
		)
	}
}

// seedOrganization writes the org the resolution actually used into a
// config-unset organization — the import-convergence seed. Unknown
// counts as unset: a real CLI plans an Optional+Computed attribute the
// config leaves off as UNKNOWN (the in-process tests craft nulls —
// only the real-tofu gate catches the difference), and an unknown
// value cannot ride state out of apply. A config-set value (slug or
// id) is kept verbatim: it already resolved at create/read.
func seedOrganization(data *gatewayModel, orgID string) {
	if data.Organization.IsNull() || data.Organization.IsUnknown() {
		data.Organization = types.StringValue(orgID)
	}
}

// readGatewayForOrg refreshes the model from the singleton read and
// returns true when the org has no gateway (404 — the caller removes
// it from state).
func readGatewayForOrg(ctx context.Context, pd *providerData, orgID string, data *gatewayModel, diags *diag.Diagnostics) bool {
	gw, err := pd.client.GetOrgGateway(ctx, orgID)
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return true
		}
		diags.AddError("Unable to read the access gateway", err.Error())
		return false
	}
	mapGateway(data, gw)
	return false
}

// readGateway resolves the org (seeding it when the config left it
// null), then reads.
func readGateway(ctx context.Context, pd *providerData, data *gatewayModel, diags *diag.Diagnostics) bool {
	orgID := resolveOrgID(ctx, pd.client, data.Organization, pd.organization, diags)
	if diags.HasError() {
		return false
	}
	seedOrganization(data, orgID)
	return readGatewayForOrg(ctx, pd, orgID, data, diags)
}

// mapGateway writes the wire's fields into the model. The write-only
// auth_key, the config identity organization (seeded elsewhere, only
// when null), and the config-authoritative instances have no wire
// source (or must not be overwritten) — the configured values survive
// Read untouched.
func mapGateway(data *gatewayModel, gw client.AccessGateway) {
	data.ID = types.StringValue(gw.ID)
	data.OrganizationID = types.StringValue(gw.OrganizationID)
	data.Hostname = types.StringValue(gw.Hostname)
	data.TailnetDomain = types.StringValue(gw.TailnetDomain)
	data.AccessProvider = types.StringValue(gw.Provider)
	data.State = types.StringValue(gw.State)
	data.StatusMessage = types.StringPointerValue(gw.StatusMessage)
	data.MonthlyCents = types.Int64Value(gw.MonthlyCents)
	data.ProvisionedAt = types.StringPointerValue(gw.ProvisionedAt)
}

func (r *gatewayResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data gatewayModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	pd := r.data(&resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	if readGateway(ctx, pd, &data, &resp.Diagnostics) {
		resp.State.RemoveResource(ctx)
		return
	}
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *gatewayResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state gatewayModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	pd := r.data(&resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	orgID := resolveOrgID(ctx, pd.client, plan.Organization, pd.organization, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	seedOrganization(&plan, orgID)

	// Fresh read FIRST: a gateway removed out-of-band must not be
	// diffed against — gone removes the resource, never plan-over-ghost
	// (the instance update's 404 stance).
	if readGatewayForOrg(ctx, pd, orgID, &plan, &resp.Diagnostics) {
		resp.State.RemoveResource(ctx)
		return
	}
	if resp.Diagnostics.HasError() {
		return
	}

	configured, d := instanceSet(ctx, plan.Instances)
	resp.Diagnostics.Append(d...)
	current, d := instanceSet(ctx, state.Instances)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}

	applyInstanceDiff(ctx, pd, orgID, plan.ID.ValueString(), current, configured, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	// Fresh read for state — the pre-diff read saw a provisioning row
	// whenever the diff had to wait, and the observer keeps moving
	// state/status_message. instances is config-authoritative and has
	// no wire source, so the plan's set survives untouched.
	if readGatewayForOrg(ctx, pd, orgID, &plan, &resp.Diagnostics) {
		resp.State.RemoveResource(ctx)
		return
	}
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *gatewayResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state gatewayModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// The honest delete: the /api/v1 route table has no gateway-delete
	// route (the only DELETE is per-attachment), so there is nothing to
	// call. The platform keeps the gateway — serving, attached, and
	// billing. Terraform forgets it; the warning is the record.
	resp.Diagnostics.AddWarning(
		"The access gateway is kept by the platform",
		fmt.Sprintf("The FluxVale API exposes no gateway-delete route, so gateway %s stays provisioned (and billing) "+
			"after this destroy — its attachments too. To actually remove it, opt out via the panel's Access tab, "+
			"then import is unnecessary: just delete this resource from your config.", state.ID.ValueString()),
	)
	resp.State.RemoveResource(ctx)
}

func (r *gatewayResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// By organization id or slug — the singleton read's path key:
	// terraform import fluxvale_access_gateway.x <org_id_or_slug>.
	// Read fills the wire-mirrored fields and seeds organization from
	// the resolution, so a config that leaves it unset converges.
	// auth_key and instances stay null — keep your config matching what
	// you deployed; a mismatch plans a replacement, and replacements
	// are rejected at plan time.
	resource.ImportStatePassthroughID(ctx, path.Root("organization"), req, resp)
}

// instanceSet converts the schema's instances into a slice. null and
// unknown both mean "nothing configured".
func instanceSet(ctx context.Context, v types.Set) ([]string, diag.Diagnostics) {
	if v.IsNull() || v.IsUnknown() {
		return nil, diag.Diagnostics{}
	}
	var elems []types.String
	diags := v.ElementsAs(ctx, &elems, false)
	if diags.HasError() {
		return nil, diags
	}
	out := make([]string, len(elems))
	for i, val := range elems {
		out[i] = val.ValueString()
	}
	return out, diag.Diagnostics{}
}

// instanceSetDiff computes the attach/detach lists, preserving the
// input order of each side (deterministic call sequences for tests and
// logs).
func instanceSetDiff(current, configured []string) (added, removed []string) {
	currentSet := make(map[string]bool, len(current))
	for _, id := range current {
		currentSet[id] = true
	}
	configuredSet := make(map[string]bool, len(configured))
	for _, id := range configured {
		configuredSet[id] = true
	}
	for _, id := range configured {
		if !currentSet[id] {
			added = append(added, id)
		}
	}
	for _, id := range current {
		if !configuredSet[id] {
			removed = append(removed, id)
		}
	}
	return added, removed
}
