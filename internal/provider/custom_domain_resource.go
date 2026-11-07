package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/fluxvale/fluxvale/apps/tfprovider/internal/client"
	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.ResourceWithImportState = (*customDomainResource)(nil)
	_ resource.ResourceWithModifyPlan  = (*customDomainResource)(nil)
)

// createWaitDefault bounds the wait_for_verification poll: the
// platform's observer pass verifies on its ~15-minute cadence, so
// three passes is the honest default.
const createWaitDefault = 45 * time.Minute

// customDomainResource manages fluxvale_custom_domain — the caller's
// own domain registered on the private-access edge. A skin over the
// register/read/remove surface: register lands the row :pending
// :custom and mints the wildcard Certificate inside the create's
// transaction (failure = no row); verification is issuance — the
// platform's observer pass is the ONLY promoter to :verified; remove
// is the full teardown server-side. Every refusal (paywall, scope,
// duplicate name) surfaces verbatim.
type customDomainResource struct {
	pd *providerData
}

// NewCustomDomainResource returns the resource factory provider.go
// registers.
func NewCustomDomainResource() resource.Resource {
	return &customDomainResource{}
}

func (r *customDomainResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_custom_domain"
}

// customDomainModel mirrors the schema. organization is written back
// only when the config leaves it null (the import-convergence seed);
// wait_for_verification is config-only posture — the wire has no echo,
// and reads keep the configured value verbatim.
type customDomainModel struct {
	Domain              types.String   `tfsdk:"domain"`
	Organization        types.String   `tfsdk:"organization"`
	WaitForVerification types.Bool     `tfsdk:"wait_for_verification"`
	Timeouts            timeouts.Value `tfsdk:"timeouts"`
	ID                  types.String   `tfsdk:"id"`
	Kind                types.String   `tfsdk:"kind"`
	State               types.String   `tfsdk:"state"`
	OrganizationID      types.String   `tfsdk:"organization_id"`
}

func (r *customDomainResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Your own domain registered on the private-access edge. Register lands the row `pending` and mints the wildcard Certificate in the same transaction; verification is issuance — the platform's observer pass (about a 15-minute cadence) promotes pending → verified once your DNS delegation answers the DNS-01 challenge, and fails the row past its issuance window (~72h). VERIFIED IS NOT RESOLUTION: a verified domain still answers nothing on the tailnet — the split-DNS claim that points the name into the tailnet has no API surface yet; claiming is a separate platform capability on a later slice. This resource registers, verifies (DNS-01), and tears down, nothing more.",
		Attributes: map[string]schema.Attribute{
			"domain": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The domain to register — e.g. `apps.example.com`. The platform canonicalizes the name " +
					"(trimmed, downcased) and stores THAT; the provider canonicalizes the wire request the same way and " +
					"decides replacement on canonical names, so any config casing converges — the plan and state carry " +
					"the form you wrote. Names are platform-wide unique across ALL organizations — the " +
					"duplicate 400 surfaces verbatim (the name may belong to another org). Changing the canonical name " +
					"replaces the resource (a different name is a different row), and destroy really removes it.",
				PlanModifiers: []planmodifier.String{canonicalDomain{}},
			},
			"organization": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Organization to register the domain in — its id or slug. Falls back to the provider-level " +
					"`organization`, then to your sole organization (several memberships without a name is an error). " +
					"The plan normalizes a set value to the resolved id, so a slug config converges with an imported id " +
					"(which carries the id); only a genuinely different organization replaces. Changing it replaces the " +
					"resource (the register takes the org only at create).",
				// No attribute plan modifier: the replacement decision
				// needs the orgs LIST, and schema (and its modifiers) are
				// built once on a throwaway instance — no client there.
				// ModifyPlan decides instead (it runs post-Configure).
			},
			"wait_for_verification": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "Wait out verification before create finishes. Default false: create returns at `pending` " +
					"and refreshes show the progression via `state` — the janitor verifies on its ~15-minute cadence after " +
					"your DNS delegation answers. When true, create polls `state` until `verified` (bounded by the " +
					"`timeouts.create`, default 45m) or fails fast on `failed` (terminal: remove and re-register). A " +
					"timeout leaves the row tainted but tracked — the domain keeps verifying server-side; `tofu untaint` " +
					"(or `terraform untaint`) keeps the registration (otherwise the next apply destroys and re-registers), " +
					"and the following refresh shows the promoted state. Nothing is PATCHable, so flipping this later " +
					"only re-reads.",
			},
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "The access-domain id — the read/remove path key, and the terraform import argument.",
			},
			"kind": schema.StringAttribute{
				Computed:    true,
				Description: "\"custom\" for every row this resource registers. An imported id may point at a platform row — the system-minted \"platform\" kind: inspectable, never registrable.",
			},
			"state": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "pending → verified, plus failed, needs_attention, and suspended. A `suspended` domain is " +
					"legacy state: domains are free, nothing current suspends one — the state and its resume survive for " +
					"old rows only. The raw `PATCH /api/v1/access-domains/:id/resume` is the only resume surface (no " +
					"domains panel, no MCP tool); this resource deliberately does not manage it.",
			},
			"organization_id": schema.StringAttribute{
				Computed:    true,
				Description: "The org the domain registers under (the wire's echo of the resolution).",
			},
		},
		Blocks: map[string]schema.Block{
			"timeouts": timeouts.Block(ctx, timeouts.Opts{
				Create: true,
				CreateDescription: "How long create waits for verification when wait_for_verification is true — a duration " +
					"like \"30m\" or \"2h\". Defaults to 45m (three observer passes); a pending row that never verifies " +
					"fails server-side after its ~72h issuance window either way.",
			}),
		},
	}
}

// canonicalDomain is the domain attribute's plan modifier: it decides
// replacement on CANONICAL names but never plans a value core would
// reject — for an attribute the user set, a planned known value must
// equal the config value or the non-null prior. So: on create (null
// prior) the plan carries the config value verbatim (the wire request
// canonicalizes at send; the platform canonicalizes server-side
// anyway); when the configured form differs from prior only by
// canonicalization, the plan carries the PRIOR value — planned==prior
// is the accepted shape, and the same-domain casing diff never
// replaces; anything else keeps the config value and replaces.
//
// It runs in the schema's plan-modification pass (before the
// resource's ModifyPlan), which is the only layer where a rewrite can
// precede the RequiresReplace decision.
type canonicalDomain struct{}

func (canonicalDomain) Description(_ context.Context) string {
	return "Decides replacement on canonical names (trim + lowercase) — same-name casing converges, different names replace."
}

func (m canonicalDomain) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (canonicalDomain) PlanModifyString(_ context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	// Unknown or null plan values can't be decided on yet — the apply
	// will tell.
	if req.PlanValue.IsNull() || req.PlanValue.IsUnknown() {
		return
	}
	canonical := strings.ToLower(strings.TrimSpace(req.PlanValue.ValueString()))

	// Create (null or unknown prior): the plan value IS the config
	// value — rewriting it would trip core's plan rule.
	if req.StateValue.IsNull() || req.StateValue.IsUnknown() {
		return
	}

	// Same domain, different form: plan the PRIOR value.
	if strings.ToLower(strings.TrimSpace(req.StateValue.ValueString())) == canonical {
		resp.PlanValue = req.StateValue
		return
	}

	// A genuinely different canonical name: the config value stays (the
	// plan rule) and the decision carries — a different name is a
	// different row.
	resp.RequiresReplace = true
}

func (r *customDomainResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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
func (r *customDomainResource) data(diags *diag.Diagnostics) *providerData {
	if r.pd == nil {
		diags.AddError(
			"Provider not configured",
			"The provider was not configured before this resource was used — please report this bug.",
		)
	}
	return r.pd
}

func (r *customDomainResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data customDomainModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	pd := r.data(&resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	orgID := resolveOrgID(ctx, pd.client, data.Organization, pd.organization, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	seedDomainOrganization(&data, orgID)

	dom, err := pd.client.CreateAccessDomain(ctx, client.CreateAccessDomainInput{
		// The wire carries the canonical form — the platform
		// canonicalizes server-side anyway, and the plan keeps the
		// configured form (core's plan rule), so state must too.
		Domain:         strings.ToLower(strings.TrimSpace(data.Domain.ValueString())),
		OrganizationID: orgID,
	})
	if err != nil {
		// Pre-POST: nothing landed (a failed register rolls the
		// Certificate mint back with the row) — no state write. The
		// platform-wide context rides the DUPLICATE refusal only (400
		// with the server's "is already registered" wording); a 403
		// paywall or 5xx tells its own story.
		msg := err.Error()
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusBadRequest &&
			strings.Contains(msg, "is already registered") {
			// No adoption probe — the name is unique across ALL
			// organizations, so the row behind the refusal may belong
			// to another org entirely.
			msg += " — names are unique platform-wide, across all organizations: the registered name may belong to another org."
		}
		resp.Diagnostics.AddError(
			"Unable to register the custom domain",
			msg,
		)
		return
	}
	data.ID = types.StringValue(dom.ID)
	mapDomain(&data, dom)

	// The platform now has this row (Certificate minted inside the
	// transaction). Tainted tracked state beats an untracked leak (the
	// gateway's post-POST stance): write what we know so every later
	// failure — wait timeout, the failed arm, the final read — leaves a
	// tracked resource the next apply updates or forgets. mapDomain
	// fills every computed field, so no unknowns remain to clear.
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Verification is issuance: the observer pass (about a 15-minute
	// cadence, after the owner delegates DNS) is the only promoter.
	// wait_for_verification only decides whether create waits out that
	// cadence. VERIFIED IS NOT RESOLUTION — see waitDomainVerified.
	if desiredWait(data.WaitForVerification) {
		waitFor, d := data.Timeouts.Create(ctx, createWaitDefault)
		resp.Diagnostics.Append(d...)
		if resp.Diagnostics.HasError() {
			return
		}
		waitDomainVerified(ctx, pd, dom.ID, waitFor, &resp.Diagnostics)
		if resp.Diagnostics.HasError() {
			return
		}
	}

	// Fresh read for state — the wait (when one ran) just saw the row
	// move, and a lingering :pending state would misdescribe it.
	if readDomain(ctx, pd, &data, &resp.Diagnostics) {
		resp.Diagnostics.AddError(
			"Custom domain disappeared during creation",
			fmt.Sprintf("domain %s was removed between register and its final read — refresh and re-apply", dom.ID),
		)
		return
	}
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// waitDomainVerified polls GET /access-domains/:id until the row
// verifies. VERIFIED IS NOT RESOLUTION: verified means the wildcard
// Certificate issued (DNS-01) — the domain still answers nothing on
// the tailnet until the separate claiming capability lands. failed
// fails fast: the wire's public surface carries no status message (the
// pinned field set), so the state string is the whole story the API
// tells — and failed is terminal for a registration; the fix is remove
// and re-register. Everything else keeps polling to the deadline.
func waitDomainVerified(ctx context.Context, pd *providerData, id string, waitFor time.Duration, diags *diag.Diagnostics) {
	var lastState string
	err := pollWithDeadline(ctx, pd.pollInterval, waitFor, func(ctx context.Context) (bool, error) {
		dom, err := pd.client.GetAccessDomain(ctx, id)
		if err != nil {
			return false, err
		}
		lastState = dom.State
		switch dom.State {
		case "verified":
			return true, nil
		case "failed":
			return false, errors.New("the domain landed in failed — DNS-01 issuance never completed inside the " +
				"platform's window, and failed is terminal for a registration: remove and re-register " +
				"(terraform apply -replace)")
		}
		return false, nil
	})
	if err != nil {
		diags.AddError(
			"Custom domain did not verify",
			fmt.Sprintf("domain %s: %v (last state %q)", id, err, lastState),
		)
	}
}

// ModifyPlan reconciles the planned organization against the stored
// row: a set value (id or slug) resolving to the SAME organization
// plans the PRIOR value (planned==prior is the rewrite core accepts —
// this is the import-converges-with-slug-config case), and a genuinely
// different organization keeps the config value in the plan and
// appends RequiresReplace here — the create half performs its own
// resolution.
//
// The decision lives in ModifyPlan, not an attribute plan modifier,
// because the schema — and any modifiers built with it — is constructed
// once on a throwaway instance and cached: no client there. This hook
// runs after resource Configure, on the planning instance.
func (r *customDomainResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	// Create planning (null state) and destroy planning (null plan)
	// have no prior organization to reconcile — the create's own
	// resolution seeds and settles those (the gateway's guard shape).
	if req.Plan.Raw.IsNull() || req.State.Raw.IsNull() {
		return
	}

	var plan, state customDomainModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Unknown (the config left it unset — reads seed the wire's echo)
	// or already-agreeing values need no lookup.
	if plan.Organization.IsNull() || plan.Organization.IsUnknown() ||
		plan.Organization.Equal(state.Organization) {
		return
	}

	// Null or unknown prior: nothing to reconcile against — the config
	// value stays in the plan and the apply's resolution settles it.
	if state.Organization.IsNull() || state.Organization.IsUnknown() {
		return
	}

	if r.pd == nil {
		// Plan hooks only run through a configured server; leaving the
		// plan as-is lets the create's own resolution report the
		// problem.
		return
	}

	planOrgID := resolveOrgID(ctx, r.pd.client, plan.Organization, r.pd.organization, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	// The stored form can be the id (import seed) or the config's own
	// form (create keeps it): resolve it the same way — best-effort, an
	// unresolvable stored form falls back to the literal comparison —
	// so only a genuinely different ORGANIZATION decides.
	stateOrgID := state.Organization.ValueString()
	var stateDiags diag.Diagnostics
	if resolved := resolveOrgID(ctx, r.pd.client, state.Organization, types.StringNull(), &stateDiags); !stateDiags.HasError() {
		stateOrgID = resolved
	}

	if planOrgID == stateOrgID {
		// Same organization in another form: plan the PRIOR value.
		plan.Organization = state.Organization
		resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
		return
	}

	// A genuinely different organization: the config value stays (the
	// plan rule) and the decision replaces.
	resp.RequiresReplace = append(resp.RequiresReplace, path.Root("organization"))
}

func (r *customDomainResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data customDomainModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	pd := r.data(&resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	if readDomain(ctx, pd, &data, &resp.Diagnostics) {
		resp.State.RemoveResource(ctx)
		return
	}
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// readDomain reads by id FIRST, then seeds organization from the
// wire's organization_id (null or unknown configs only — a set value
// is kept verbatim). No orgs LIST: reads go by id, so a multi-org
// token with no default can import and refresh, and a revoked
// membership doesn't break the by-id read. Returns true when the row
// is gone (404 — the caller removes it from state). wait_for_verification
// is config-only and survives Read untouched.
func readDomain(ctx context.Context, pd *providerData, data *customDomainModel, diags *diag.Diagnostics) bool {
	dom, err := pd.client.GetAccessDomain(ctx, data.ID.ValueString())
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return true
		}
		diags.AddError("Unable to read the custom domain", err.Error())
		return false
	}
	mapDomain(data, dom)
	seedDomainOrganization(data, dom.OrganizationID)
	return false
}

// mapDomain writes the wire's fields into the model. The domain is
// form-preserving: a configured value whose canonical form equals the
// wire's survives verbatim (state carries the user's casing, matching
// their config — the plan-rule shape); a null or unknown value (first
// reads, imports) seeds the wire's canonical form; a wire name whose
// canonical form DIFFERS means the row moved under the state — the
// wire is the truth.
func mapDomain(data *customDomainModel, dom client.AccessDomain) {
	data.ID = types.StringValue(dom.ID)
	if data.Domain.IsNull() || data.Domain.IsUnknown() ||
		strings.ToLower(strings.TrimSpace(data.Domain.ValueString())) != dom.Domain {
		data.Domain = types.StringValue(dom.Domain)
	}
	data.Kind = types.StringValue(dom.Kind)
	data.State = types.StringValue(dom.State)
	data.OrganizationID = types.StringValue(dom.OrganizationID)
}

// seedDomainOrganization writes the org the resolution (or the wire's
// echo) actually used into a null OR UNKNOWN organization — the
// import-convergence seed, and the create-plan unknown Terraform marks
// for unset computed values (an unknown must not ride final state).
// A config-set value (slug or id) is kept verbatim: ModifyPlan
// normalizes slug plans to the id instead.
func seedDomainOrganization(data *customDomainModel, orgID string) {
	if data.Organization.IsNull() || data.Organization.IsUnknown() {
		data.Organization = types.StringValue(orgID)
	}
}

// desiredWait is what wait_for_verification asks for: null means the
// default (false — return at pending).
func desiredWait(v types.Bool) bool {
	return !v.IsNull() && !v.IsUnknown() && v.ValueBool()
}

// Update is the re-read-and-set no-op: nothing is PATCHable (the only
// PATCH verb is resume, deliberately not exposed), and everything that
// can change identity — domain, organization — replaces at plan time,
// so an update is only ever wait_for_verification or the timeouts
// changing. A fresh read settles state; 404 removes — gone is gone,
// never plan-over-ghost.
func (r *customDomainResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state customDomainModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	pd := r.data(&resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	// id is Computed with no config source: when wait_for_verification
	// or the timeouts change, core plans it UNKNOWN — and reading by
	// ValueString() would GET /api/v1/access-domains/ (no id), whose
	// 404 removes a LIVE domain from state. The row's identity lives in
	// state; carry it before anything reads by it.
	plan.ID = state.ID

	if readDomain(ctx, pd, &plan, &resp.Diagnostics) {
		resp.State.RemoveResource(ctx)
		return
	}
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *customDomainResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state customDomainModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	pd := r.data(&resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	// The full teardown server-side — hostname pre-check, tailnet
	// unclaim, row destroy, CoreDNS re-render, Certificate safe-delete
	// — one synchronous DELETE, so there is nothing to poll: the 200
	// is done. A 404 is already-gone: idempotent destroy (a panel
	// remove between refresh and apply must not fail the plan). The
	// platform-kind 400 is unreachable for provider-created rows
	// (register forces :custom) but surfaces verbatim if ever seen.
	if err := pd.client.DeleteAccessDomain(ctx, state.ID.ValueString()); err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			resp.State.RemoveResource(ctx) // already gone
			return
		}
		resp.Diagnostics.AddError("Unable to delete the custom domain", err.Error())
		return
	}
	resp.State.RemoveResource(ctx)
}

func (r *customDomainResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// By domain id: terraform import fluxvale_custom_domain.x <id>.
	// Read fills the wire-mirrored fields — domain (the canonical
	// name), kind, state, organization_id — and seeds organization from
	// the resolution, so a config that leaves it unset converges.
	// wait_for_verification stays null; set it (and the timeouts) in
	// your config if you want the next create-armed apply to wait.
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
