package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/fluxvale/terraform-provider-fluxvale/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ resource.Resource = (*pocketIdSsoResource)(nil)

// pocketIdSsoResource manages fluxvale_pocket_id_sso — an instance's
// typed PocketID sign-in wiring (#731 T3). One wiring per instance:
// the instance id IS the Terraform id (the wiring row id changes on a
// re-wire, so it must never become the resource identity).
//
// Create connects (the platform dispatches pre-deploy vs deployed);
// Read refreshes the wiring and treats a 404 as gone; Update is the
// rotate-on-change verb — only rotate_secret_on can change in place
// (the identity fields force replacement), and a change rotates the
// client secret without minting a new client; Delete disconnects
// idempotently. Every 400 — busy rollout, feature flag off, custody,
// a not-yet-deployed rotation — surfaces the platform's wording
// verbatim.
type pocketIdSsoResource struct {
	pd *providerData
}

// NewPocketIdSsoResource returns the resource factory provider.go
// registers.
func NewPocketIdSsoResource() resource.Resource {
	return &pocketIdSsoResource{}
}

func (r *pocketIdSsoResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_pocket_id_sso"
}

// pocketIdSsoModel mirrors the schema. The two identity fields are
// config-authoritative (the wire echoes them; reads keep instance_id
// from the lookup key); rotate_secret_on is config-only posture — the
// wire has no echo, and its ONLY job is to trigger a rotation in
// place; the three computed fields mirror the wiring.
type pocketIdSsoModel struct {
	InstanceID         types.String `tfsdk:"instance_id"`
	PocketIDInstanceID types.String `tfsdk:"pocket_id_instance_id"`
	RotateSecretOn     types.String `tfsdk:"rotate_secret_on"`
	PocketIDClientID   types.String `tfsdk:"pocket_id_client_id"`
	PocketIDSecretID   types.String `tfsdk:"pocket_id_secret_id"`
	WiredAt            types.String `tfsdk:"wired_at"`
	ID                 types.String `tfsdk:"id"`
}

func (r *pocketIdSsoResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "An instance's PocketID sign-in wiring — the durable, typed record that the instance's OIDC env is backed by a PocketID client the platform minted and holds. One wiring per instance, so the resource id is the instance id (the wiring row id changes on a re-wire and is deliberately not the Terraform identity). The mutations are gated by the platform's `pocketid_sso_wiring` feature flag (seeded enabled); the read stays available while it is off so a refresh never loses state. Changing `rotate_secret_on` rotates the client secret in place — it does not replace the resource and never loses the client id; changing `instance_id` or `pocket_id_instance_id` replaces it (disconnect + a fresh connect).",
		Attributes: map[string]schema.Attribute{
			"instance_id": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The app instance whose sign-in is wired — a `fluxvale_instance.id`. This is the one-wiring key " +
					"and the resource's Terraform id. Changing it replaces the resource (a different instance is a different wiring).",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"pocket_id_instance_id": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The PocketID instance serving the sign-in — a `fluxvale_instance.id` in the same organization. " +
					"The platform requires it to be public and to have held-key custody (the operator stores its API key once); " +
					"a refusal — wrong org, private, no custody — surfaces verbatim. Changing it replaces the resource: a " +
					"different PocketID is a different client/wiring (the old client is best-effort deleted).",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"rotate_secret_on": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "The rotation trigger — an arbitrary value (a date, a version, a nonce). Creating the resource " +
					"connects; changing this value calls the rotate verb in place: a second active secret is minted, the " +
					"running workload rolls to it, then the old secret is retired. The client id and resource id never change " +
					"(rotation is NOT a replacement — replacing would disconnect and mint a brand-new client). The platform's " +
					"400s (a not-yet-deployed wiring, a lost/deleting instance) surface verbatim. Null means no rotation " +
					"trigger; it is config-only posture — the wire has no echo.",
			},
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "The instance id — the one-wiring key. Stable across rotations and re-reads (the wiring row id is not the Terraform id).",
				// An in-place rotation must not turn the id unknown:
				// anything referencing the resource would plan off it.
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"pocket_id_client_id": schema.StringAttribute{
				Computed:    true,
				Description: "The OIDC client id PocketID minted for this wiring — an identifier, not a credential. Stable across secret rotations; a replacement mints a new one.",
			},
			"pocket_id_secret_id": schema.StringAttribute{
				Computed:    true,
				Description: "PocketID's id for the client's live secret — an identifier, never the secret's value. The value exists once, in the instance's env_vars, and is never echoed; rotation moves this id.",
			},
			"wired_at": schema.StringAttribute{
				Computed:    true,
				Description: "When the client was minted and the env merged (the wiring row's create timestamp; a re-wire writes a fresh one).",
			},
		},
	}
}

func (r *pocketIdSsoResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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
func (r *pocketIdSsoResource) data(diags *diag.Diagnostics) *providerData {
	if r.pd == nil {
		diags.AddError(
			"Provider not configured",
			"The provider was not configured before this resource was used — please report this bug.",
		)
	}
	return r.pd
}

func (r *pocketIdSsoResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data pocketIdSsoModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	pd := r.data(&resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	// The plan's identity spellings are authoritative: Ash's uuid cast
	// lowercases on the wire, so a case-fold echo must never replace
	// the configured value (and a materially different echo is a
	// platform bug that must not half-write state).
	planInstanceID := data.InstanceID.ValueString()
	planPocketID := data.PocketIDInstanceID.ValueString()

	wiring, err := pd.client.ConnectPocketId(ctx, planInstanceID, planPocketID)
	if err != nil {
		// Pre-state: the POST refused (busy / flag off / custody /
		// scope), so nothing half-writes state. The platform's wording
		// is the whole story.
		resp.Diagnostics.AddError("Unable to connect the PocketID sign-in", err.Error())
		return
	}
	if !strings.EqualFold(wiring.InstanceID, planInstanceID) {
		resp.Diagnostics.AddError(
			"The platform wired a different instance",
			fmt.Sprintf("connect answered for instance %q, but this resource asked for %q. Terraform wrote no state, but the 201 may "+
				"already have left a live wiring/client on the platform — inspect it and disconnect it there before retrying "+
				"(a retry could mint another client).", wiring.InstanceID, planInstanceID),
		)
		return
	}
	if !strings.EqualFold(wiring.PocketIDInstanceID, planPocketID) {
		resp.Diagnostics.AddError(
			"The platform wired a different PocketID instance",
			fmt.Sprintf("connect answered with PocketID instance %q, but this resource asked for %q. Terraform wrote no state, but the 201 may "+
				"already have left a live wiring/client on the platform — inspect it and disconnect it there before retrying "+
				"(a retry could mint another client).", wiring.PocketIDInstanceID, planPocketID),
		)
		return
	}
	mapPocketIdSso(&data, wiring)
	data.InstanceID = types.StringValue(planInstanceID)
	data.ID = types.StringValue(planInstanceID)
	data.PocketIDInstanceID = types.StringValue(planPocketID)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// readPocketIdSso refreshes the model from the wiring read and returns
// true when the instance has no wiring (404 — the caller removes it
// from state: no wiring is gone). A null/unknown/empty lookup key is
// refused before any request: GET /instances//pocket-id-wiring would
// 404 and remove a LIVE wiring from state.
func readPocketIdSso(ctx context.Context, pd *providerData, data *pocketIdSsoModel, diags *diag.Diagnostics) bool {
	// The lookup key is the state's instance id — an empty key is
	// invalid state, never a reason to forget the resource.
	if data.InstanceID.IsNull() || data.InstanceID.IsUnknown() || data.InstanceID.ValueString() == "" {
		diags.AddError(
			"Invalid PocketID SSO state",
			"The state's \"instance_id\" is null, unknown, or empty — refusing to read the wiring by an empty key "+
				"(a 404 would remove a live wiring from state). Refresh cannot repair a corrupt key: remove the resource "+
				"from state (for example, `terraform state rm <address>`) and re-create it.",
		)
		return false
	}
	instanceID := data.InstanceID.ValueString()
	priorPocketID := data.PocketIDInstanceID.ValueString()
	wiring, err := pd.client.GetPocketIdWiring(ctx, instanceID)
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return true
		}
		diags.AddError("Unable to read the PocketID sign-in wiring", err.Error())
		return false
	}
	mapPocketIdSso(data, wiring)
	// The GET was made BY the state's instance id: that lookup key wins
	// unconditionally, and a differing echo is ignored (it must never
	// move the resource identity — a replacement would then disconnect
	// the wrong instance). A case-fold pocket_id_instance_id echo keeps
	// the state spelling; a materially different one is the wire's truth
	// (an out-of-band re-wire), which Read may drift.
	data.InstanceID = types.StringValue(instanceID)
	data.ID = types.StringValue(instanceID)
	data.PocketIDInstanceID = types.StringValue(pinWireIdentity(wiring.PocketIDInstanceID, priorPocketID))
	return false
}

// pinWireIdentity keeps the prior (configured/state) spelling when the
// wire's echo names the same value case-insensitively — Ash's uuid cast
// lowercases — and takes the wire value when it is materially
// different (an out-of-band re-wire).
func pinWireIdentity(wire, prior string) string {
	if prior != "" && strings.EqualFold(wire, prior) {
		return prior
	}
	return wire
}

// mapPocketIdSso writes the wiring's fields into the model. The id is
// the INSTANCE id — never the wiring row id (the read's data.id), which
// changes on a re-wire. The computed fields mirror the wire exactly;
// rotate_secret_on has no wire source and survives untouched.
func mapPocketIdSso(data *pocketIdSsoModel, wiring client.PocketIdWiring) {
	data.ID = types.StringValue(wiring.InstanceID)
	data.InstanceID = types.StringValue(wiring.InstanceID)
	data.PocketIDInstanceID = types.StringValue(wiring.PocketIDInstanceID)
	data.PocketIDClientID = types.StringValue(wiring.PocketIDClientID)
	data.PocketIDSecretID = types.StringValue(wiring.PocketIDSecretID)
	data.WiredAt = types.StringValue(wiring.WiredAt)
}

func (r *pocketIdSsoResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data pocketIdSsoModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	pd := r.data(&resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	if readPocketIdSso(ctx, pd, &data, &resp.Diagnostics) {
		resp.State.RemoveResource(ctx)
		return
	}
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// Update is the rotate-on-change verb: only rotate_secret_on can
// change in place (the identity fields force replacement at plan
// time), so a changed trigger calls POST rotate and maps the returned
// wiring — the new secret id, the same client id and row identity.
// The equal-trigger arm is defensive (a refresh) and never rotates.
func (r *pocketIdSsoResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state pocketIdSsoModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	pd := r.data(&resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	// The row identity is the state's instance id — never a plan
	// unknown (the custom-domain update's stance). The configured
	// identity spellings are the plan's; every wire echo is folded back
	// onto them so a case-normalized response can never trip the
	// framework's inconsistent-result check.
	instanceID := state.InstanceID.ValueString()
	planPocketID := plan.PocketIDInstanceID.ValueString()
	if plan.PocketIDInstanceID.IsNull() || plan.PocketIDInstanceID.IsUnknown() {
		// Defensive: a valid in-place update cannot reach here with a
		// null/unknown identity (RequiresReplace would have forced a
		// replacement), but pinning an empty string into state would
		// corrupt the next plan.
		planPocketID = state.PocketIDInstanceID.ValueString()
	}
	plan.InstanceID = types.StringValue(instanceID)

	if !plan.RotateSecretOn.Equal(state.RotateSecretOn) {
		wiring, err := pd.client.RotatePocketIdSecret(ctx, instanceID)
		if err != nil {
			var apiErr *client.APIError
			if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
				// An Update must return a state; a gone wiring is a
				// refresh problem, never a silent forget here.
				addWiringGoneUpdateError(&resp.Diagnostics)
				return
			}
			resp.Diagnostics.AddError("Unable to rotate the PocketID client secret", err.Error())
			return
		}
		mapPocketIdSso(&plan, wiring)
		plan.InstanceID = types.StringValue(instanceID)
		plan.ID = types.StringValue(instanceID)
		plan.PocketIDInstanceID = types.StringValue(planPocketID)
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		return
	}

	if readPocketIdSso(ctx, pd, &plan, &resp.Diagnostics) {
		// Same stance as the rotate arm: Update must emit state; the
		// next refresh is what removes a gone wiring.
		addWiringGoneUpdateError(&resp.Diagnostics)
		return
	}
	if resp.Diagnostics.HasError() {
		return
	}
	plan.InstanceID = types.StringValue(instanceID)
	plan.ID = types.StringValue(instanceID)
	plan.PocketIDInstanceID = types.StringValue(planPocketID)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// addWiringGoneUpdateError is the Update 404 answer: the framework
// requires a non-null state from Update, so a gone wiring cannot be
// removed here — name the refresh that will.
func addWiringGoneUpdateError(diags *diag.Diagnostics) {
	diags.AddError(
		"The PocketID sign-in wiring is gone",
		"the sign-in wiring is gone — refresh and apply again; Terraform will remove it from state",
	)
}

func (r *pocketIdSsoResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state pocketIdSsoModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	pd := r.data(&resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	// Disconnect is idempotent by contract: a live wiring deletes the
	// client and clears the row; no wiring answers success with
	// client_deleted false (still a success for Terraform — the
	// desired state is "unwired"). A 404 is already-gone too (the
	// instance was removed out of band); anything else surfaces.
	if _, err := pd.client.DisconnectPocketId(ctx, state.InstanceID.ValueString()); err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Unable to disconnect the PocketID sign-in", err.Error())
		return
	}
	resp.State.RemoveResource(ctx)
}
