package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
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

var _ resource.ResourceWithImportState = (*instanceResource)(nil)

// instanceResource manages fluxvale_instance — an instance of a
// catalog app version. A skin over POST /instances + the lifecycle
// PATCH verbs: every business rule (paywall, allocation bounds,
// version existence) is the server's; this layer surfaces its 400s
// (attribute/guard refusals), 403s (scope/policy), and 404s verbatim.
type instanceResource struct {
	pd *providerData
}

// NewInstanceResource returns the resource factory provider.go
// registers.
func NewInstanceResource() resource.Resource {
	return &instanceResource{}
}

func (r *instanceResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_instance"
}

// instanceModel mirrors the schema. Config identity fields
// (organization/app/app_version) and the write-only env_vars are never
// overwritten from the wire; running is the desired posture (the
// observed one is status). app_version_id is config-or-resolved: set
// it explicitly, or let app + app_version resolve it — Read maps the
// wire's id either way.
type instanceModel struct {
	Organization         types.String  `tfsdk:"organization"`
	Name                 types.String  `tfsdk:"name"`
	AppVersionID         types.String  `tfsdk:"app_version_id"`
	App                  types.String  `tfsdk:"app"`
	AppVersion           types.String  `tfsdk:"app_version"`
	CPUCores             types.Float64 `tfsdk:"cpu_cores"`
	MemoryMB             types.Int64   `tfsdk:"memory_mb"`
	StorageGB            types.Int64   `tfsdk:"storage_gb"`
	EnvVars              types.Map     `tfsdk:"env_vars"`
	SecretEnvVars        types.Map     `tfsdk:"secret_env_vars"`
	SecretEnvVarsVersion types.Int64   `tfsdk:"secret_env_vars_version"`
	SecretEnvVarsKeys    types.List    `tfsdk:"secret_env_vars_keys"`
	Running              types.Bool    `tfsdk:"running"`
	AlwaysOn             types.Bool    `tfsdk:"always_on"`
	Exposure             types.String  `tfsdk:"exposure"`
	ID                   types.String  `tfsdk:"id"`
	Slug                 types.String  `tfsdk:"slug"`
	Namespace            types.String  `tfsdk:"namespace"`
	Status               types.String  `tfsdk:"status"`
	Port                 types.Int64   `tfsdk:"port"`
	Image                types.String  `tfsdk:"image"`
	DeployedAt           types.String  `tfsdk:"deployed_at"`
	OrganizationID       types.String  `tfsdk:"organization_id"`
}

func (r *instanceResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "An instance of a catalog app version — FluxVale's unit of workload. The name, version identity, and organization are create-only (the API has no rename/version-change/org-change routes), so changing them replaces the instance. `env_vars` updates in place through one merge PATCH; the allocation resizes in place: the API's resize is stopped-first (the provider sequences stop → resize → start), CPU/RAM both ways, storage up-only.",
		Attributes: map[string]schema.Attribute{
			"organization": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Organization to deploy into — its id or slug. Falls back to the provider-level " +
					"`organization`, then to your sole organization (several memberships without a name is an error). " +
					"Changing it replaces the instance (the API has no org-change route).",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"organization_id": schema.StringAttribute{
				Computed:    true,
				Description: "The resolved owning organization id (the wire's organization_id) — set by the create's resolution or the import's read.",
			},
			"name": schema.StringAttribute{
				Required:      true,
				Description:   "The instance's display name. Create-only (no rename route) — changing it replaces the instance.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"app_version_id": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "The catalog app_version id to deploy. Either this or `app` + `app_version` (a slug " +
					"and version string, resolved through the catalog); if both forms are set this id wins, and neither is an error. " +
					"The resolved id is written back from the wire. Create-only (no version-change route) — changing it replaces the instance.",
				// Keep the resolved id known across in-place updates (an
				// env merge must not replace a slug-configured instance);
				// a slug or id change still plans a replacement.
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
					stringplanmodifier.RequiresReplace(),
				},
			},
			"app": schema.StringAttribute{
				Optional:      true,
				Description:   "Catalog app slug, with app_version, the slug form of app_version_id. Create-only.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"app_version": schema.StringAttribute{
				Optional:      true,
				Description:   "Version string within app, the slug form of app_version_id. Create-only.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"cpu_cores": schema.Float64Attribute{
				Required:    true,
				Description: "CPU cores (the platform's decimal cpu_cores field). Resizes in place — the API's resize is stopped-first; the provider sequences stop → resize → start.",
			},
			"memory_mb": schema.Int64Attribute{
				Required:    true,
				Description: "Memory in MB (the platform's memory_mb field). Resizes in place — stopped-first, like cpu_cores.",
			},
			"storage_gb": schema.Int64Attribute{
				Required:    true,
				Description: "Storage in GB (the platform's storage_gb field). Resizes in place; storage is up-only — a shrink is the server's 400 naming both figures.",
			},
			"env_vars": schema.MapAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Sensitive:   true,
				MarkdownDescription: "Environment variables for the deploy (merged over the app version's defaults, " +
					"server-side). Updates in place: the provider diffs state against the plan into one merge PATCH; " +
					"a null/omitted map clears the keys this resource previously managed, and a null element is " +
					"rejected — remove the key instead (an empty string is a value). Write-only: the API never echoes " +
					"env_vars, so plan can't see server-side drift by design, and the configured value is kept " +
					"verbatim in state. Deployer-composed keys (OIDC recipe, backing URLs, instance URL) are refused " +
					"on set and skipped on remove, so an apply that stops naming one converges. First-run contract " +
					"`env` keys are fixed — set and remove both refuse with 400 — and `forbidden_env` keys refuse set " +
					"but allow remove; a contract `env` key already in state dead-ends until removed from state/config " +
					"by hand.",
			},
			"secret_env_vars": schema.MapAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Sensitive:   true,
				WriteOnly:   true,
				MarkdownDescription: "Secret environment variables for the deploy, merged into the same in-place env PATCH " +
					"as `env_vars`. Terraform never writes values to state or plan files — the API never echoes them either — " +
					"so this is the strongest input the provider can offer. Because a write-only value cannot produce a plan " +
					"diff, `secret_env_vars_version` is the rotation signal: bump it whenever a value changes, and every key " +
					"in the map is re-sent. A key present in both `env_vars` and `secret_env_vars` is a plan-time error. " +
					"Requires Terraform 1.12+ or OpenTofu 1.13+.",
			},
			"secret_env_vars_version": schema.Int64Attribute{
				Optional: true,
				MarkdownDescription: "Rotation signal for `secret_env_vars` — required whenever the map is set, and must be " +
					"greater than 0. Bump it to send the current secret values again. Removal is drop-key-plus-bump: remove " +
					"the key from `secret_env_vars` and bump this integer; removing every key may instead drop both " +
					"attributes (or set the map to `{}` and bump). With no secrets, leave both unset.",
			},
			"secret_env_vars_keys": schema.ListAttribute{
				ElementType: types.StringType,
				Computed:    true,
				MarkdownDescription: "Sorted keys this resource manages through `secret_env_vars` — never values. The provider " +
					"records the list on apply so reviewers can see which names are managed, and diffs it against the prior " +
					"list to remove dropped keys (the drop-key-plus-bump protocol). `null` after import: the API cannot tell " +
					"the provider which env keys were secret.",
			},
			"running": schema.BoolAttribute{
				Optional:    true,
				Description: "Desired posture — defaults to true. false stops the instance after create; updates flip it via stop/start. The observed state is `status` (the platform's idle sweep may sleep a running instance on its own; this attribute never fights that).",
			},
			"always_on": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Opt out of scale-to-zero (the platform's always_on). Not accepted at create — applied via PATCH always-on; the state carries the server's value.",
			},
			"exposure": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "\"public\" or \"private\" (the platform's exposure; defaults to public). Not accepted at create — applied via PATCH exposure; the state carries the server's value.",
			},
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "The instance id — also the terraform import argument.",
				// In-place updates must not turn the id unknown: anything
				// referencing it (backups, gateway attachments) would plan
				// a replacement off an unknown id.
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"slug": schema.StringAttribute{
				Computed:    true,
				Description: "Haikunate-generated slug; doubles as the DNS subdomain.",
			},
			"namespace": schema.StringAttribute{
				Computed:    true,
				Description: "The fluxvale-app-<id> namespace (null until deployed).",
			},
			"status": schema.StringAttribute{
				Computed:    true,
				Description: "The state machine's status: pending → deploying → starting → running ⇄ stopped (sleeping, restoring, error, deleting, lost possible).",
			},
			"port": schema.Int64Attribute{
				Computed:    true,
				Description: "The deployed app's port (from the version blueprint).",
			},
			"image": schema.StringAttribute{
				Computed:    true,
				Description: "The deployed image (from the version blueprint).",
			},
			"deployed_at": schema.StringAttribute{
				Computed:    true,
				Description: "When the current deploy was enqueued (null before the first deploy).",
			},
		},
	}
}

func (r *instanceResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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
func (r *instanceResource) data(diags *diag.Diagnostics) *providerData {
	if r.pd == nil {
		diags.AddError(
			"Provider not configured",
			"The provider was not configured before this resource was used — please report this bug.",
		)
	}
	return r.pd
}

func (r *instanceResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data instanceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Write-only values live in the configuration, not the planned
	// state (Terraform replaces them with null there). Read them from
	// Config for this apply only.
	if req.Config.Schema != nil {
		var config instanceModel
		resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
		if resp.Diagnostics.HasError() {
			return
		}
		data.SecretEnvVars = config.SecretEnvVars
	}

	pd := r.data(&resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	createInstance(ctx, pd, &data, &resp.Diagnostics)
	// Never let the write-only map ride state — the framework would
	// null it on the real RPC path, and the direct-call tests should
	// see the same contract.
	data.SecretEnvVars = types.MapNull(types.StringType)
	if resp.Diagnostics.HasError() {
		// A post-POST failure leaves a live, billing instance. Tainted
		// tracked state beats an untracked leak: write what we know so
		// the next apply updates/destroys it instead of creating a
		// second one. Pre-POST failures (org/version resolution) leave
		// the id unset — nothing was created.
		if !data.ID.IsNull() && !data.ID.IsUnknown() {
			nullUnknownComputed(&data)
			resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
		}
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// createInstance POSTs the instance, applies the pre-deploy PATCHes,
// enqueues the deploy, and waits for running.
//
// Order follows the wire: the row lands :pending with no namespace, so
// exposure/always-on go FIRST — attribute-only writes (set_exposure's
// cluster apply is gated on a managed namespace; set_always_on is
// always attribute-only), applied before the deploy PATCH so the
// deploy job converges on the final posture instead of racing a live
// K8s apply (AdoptCurrentRow re-reads the row FOR UPDATE). Deploying
// itself is a separate verb — the trigger enqueues from :deploy, not
// :create. stop needs a running row, so it comes after the wait.
func createInstance(ctx context.Context, pd *providerData, data *instanceModel, diags *diag.Diagnostics) {
	versionID, err := resolveAppVersionID(ctx, pd.client, data)
	if err != nil {
		diags.AddError("Unable to resolve the app version", err.Error())
		return
	}

	orgID := resolveOrgID(ctx, pd.client, data.Organization, pd.organization, diags)
	if diags.HasError() {
		return
	}

	envVars, d := envVarMap(ctx, data.EnvVars)
	diags.Append(d...)
	if diags.HasError() {
		return
	}

	diags.Append(secretEnvPairDiagnostics(data.SecretEnvVars, data.SecretEnvVarsVersion)...)
	if diags.HasError() {
		return
	}
	secretVars, d := secretEnvMap(ctx, data.SecretEnvVars)
	diags.Append(d...)
	if diags.HasError() {
		return
	}
	mergedEnv, d := mergeEnvMaps(envVars, secretVars)
	diags.Append(d...)
	if diags.HasError() {
		return
	}
	data.SecretEnvVarsKeys = secretKeysList(sortedKeys(secretVars))

	// The desired posture, captured before the create response's values
	// map into the model — the comparison below is plan-vs-server.
	wantExposure := data.Exposure
	wantAlwaysOn := data.AlwaysOn

	created, err := pd.client.CreateInstance(ctx, client.CreateInstanceInput{
		Name:           data.Name.ValueString(),
		CPUCores:       data.CPUCores.ValueFloat64(),
		MemoryMB:       data.MemoryMB.ValueInt64(),
		StorageGB:      data.StorageGB.ValueInt64(),
		AppVersionID:   versionID,
		OrganizationID: orgID,
		EnvVars:        mergedEnv,
	})
	if err != nil {
		diags.AddError("Unable to create the Instance", err.Error())
		return
	}
	data.ID = types.StringValue(created.ID)
	// Map the create response now — every later failure path leaves
	// state carrying the real slug/status/allocation, not unknowns.
	mapInstance(data, created)

	// Pre-deploy posture (attribute-only on a :pending row).
	if !wantExposure.IsNull() && !wantExposure.IsUnknown() && wantExposure.ValueString() != created.Exposure {
		if _, err := pd.client.InstanceAction(ctx, created.ID, "exposure", map[string]any{"exposure": wantExposure.ValueString()}); err != nil {
			diags.AddError("Unable to set exposure", deployErr(created.ID, err))
			return
		}
	}
	if !wantAlwaysOn.IsNull() && !wantAlwaysOn.IsUnknown() && wantAlwaysOn.ValueBool() != created.AlwaysOn {
		if _, err := pd.client.InstanceAction(ctx, created.ID, "always-on", map[string]any{"always_on": wantAlwaysOn.ValueBool()}); err != nil {
			diags.AddError("Unable to set always_on", deployErr(created.ID, err))
			return
		}
	}

	// The deploy PATCH lands :deploying and enqueues the Oban job.
	if _, err := pd.client.InstanceAction(ctx, created.ID, "deploy", map[string]any{}); err != nil {
		diags.AddError("Unable to deploy the Instance", deployErr(created.ID, err))
		return
	}

	// Wait out the async deploy job. error and lost are the failure
	// class (instance.ex's re-deploy set minus pending) — fail fast on
	// both instead of burning the timeout; status_message is
	// admin-hidden, so the status string is the whole story the API
	// tells.
	lastStatus, err := waitInstanceStatus(ctx, pd, created.ID, "running")
	if err != nil {
		diags.AddError(
			"Instance deploy did not reach running",
			fmt.Sprintf("instance %s: %v (last status %q)", created.ID, err, lastStatus),
		)
		return
	}

	// The stop verb requires a running row — guaranteed by the wait
	// just above; the PATCH is synchronous (its response already
	// carries the new status). No precondition GET needed on a row
	// created seconds ago.
	if !desiredRunning(data.Running) {
		if _, err := pd.client.InstanceAction(ctx, created.ID, "stop", map[string]any{}); err != nil {
			diags.AddError("Unable to stop the Instance", deployErr(created.ID, err))
			return
		}
	}

	// Fresh read for state — the deployed detail surface (or the
	// stopped posture, when running=false), including the FK ids.
	if readInstance(ctx, pd, data, diags) {
		diags.AddError(
			"Instance disappeared during creation",
			fmt.Sprintf("instance %s was deleted between create and its final read — refresh and re-apply", created.ID),
		)
	}
}

// waitInstanceStatus polls GET /instances/:id until the status hits
// want (or the timeout), failing fast on the failure class (error,
// lost). Returns the last observed status for diagnostics.
func waitInstanceStatus(ctx context.Context, pd *providerData, id, want string) (string, error) {
	var last string
	err := pollWithDeadline(ctx, pd.pollInterval, pd.pollTimeout, func(ctx context.Context) (bool, error) {
		in, err := pd.client.GetInstance(ctx, id)
		if err != nil {
			return false, err
		}
		last = in.Status
		switch in.Status {
		case want:
			return true, nil
		case "error", "lost":
			return false, fmt.Errorf("the instance entered %q state (status_message is admin-only; see the panel)", in.Status)
		}
		return false, nil
	})
	return last, err
}

func (r *instanceResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data instanceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	pd := r.data(&resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	if readInstance(ctx, pd, &data, &resp.Diagnostics) {
		resp.State.RemoveResource(ctx)
		return
	}
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// readInstance refreshes the model from GET /instances/:id. It maps
// the detail fields and returns true when the instance is gone
// (404 — the caller removes it from state). Config identity fields
// (organization, app, app_version) and the write-only env_vars are
// never touched; running stays the desired posture.
func readInstance(ctx context.Context, pd *providerData, data *instanceModel, diags *diag.Diagnostics) bool {
	in, err := pd.client.GetInstance(ctx, data.ID.ValueString())
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return true
		}
		diags.AddError("Unable to read the Instance", err.Error())
		return false
	}
	mapInstance(data, in)
	return false
}

// mapInstance writes the wire's detail fields into the model. Fields
// with no wire source stay as they are — env_vars never rides
// responses (hide_fields), so the configured value survives Read
// untouched. app_version_id fills from the wire when absent (the
// create/PATCH responses omit the FK ids; GetInstance asks for them).
func mapInstance(data *instanceModel, in client.Instance) {
	data.ID = types.StringValue(in.ID)
	data.Name = types.StringValue(in.Name)
	data.Slug = types.StringValue(in.Slug)
	data.Namespace = types.StringPointerValue(in.Namespace)
	data.Status = types.StringValue(in.Status)
	data.AlwaysOn = types.BoolValue(in.AlwaysOn)
	data.Exposure = types.StringValue(in.Exposure)
	data.Image = types.StringPointerValue(in.Image)
	data.Port = types.Int64PointerValue(in.Port)
	data.CPUCores = types.Float64PointerValue(in.CPUCores)
	data.MemoryMB = types.Int64PointerValue(in.MemoryMB)
	data.StorageGB = types.Int64PointerValue(in.StorageGB)
	data.DeployedAt = types.StringPointerValue(in.DeployedAt)
	data.OrganizationID = types.StringPointerValue(stringPointer(in.OrganizationID))
	if in.AppVersionID != "" {
		data.AppVersionID = types.StringValue(in.AppVersionID)
	}
}

func stringPointer(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// nullUnknownComputed clears computed fields still unknown at this
// point (a failed create never filled them) — unknown values cannot
// ride state.
func nullUnknownComputed(data *instanceModel) {
	if data.Slug.IsUnknown() {
		data.Slug = types.StringNull()
	}
	if data.Namespace.IsUnknown() {
		data.Namespace = types.StringNull()
	}
	if data.Status.IsUnknown() {
		data.Status = types.StringNull()
	}
	if data.AlwaysOn.IsUnknown() {
		data.AlwaysOn = types.BoolNull()
	}
	if data.Exposure.IsUnknown() {
		data.Exposure = types.StringNull()
	}
	if data.Port.IsUnknown() {
		data.Port = types.Int64Null()
	}
	if data.Image.IsUnknown() {
		data.Image = types.StringNull()
	}
	if data.DeployedAt.IsUnknown() {
		data.DeployedAt = types.StringNull()
	}
	if data.OrganizationID.IsUnknown() {
		data.OrganizationID = types.StringNull()
	}
	if data.AppVersionID.IsUnknown() {
		data.AppVersionID = types.StringNull()
	}
}

func (r *instanceResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state instanceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// The write-only map is null in the planned state; its values only
	// exist in the configuration at apply time.
	if req.Config.Schema != nil {
		var config instanceModel
		resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
		if resp.Diagnostics.HasError() {
			return
		}
		plan.SecretEnvVars = config.SecretEnvVars
	}

	pd := r.data(&resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	if updateInstance(ctx, pd, &plan, &state, &resp.Diagnostics) {
		// Deleted between refresh and apply — remove, don't report a
		// clean apply over a ghost.
		resp.State.RemoveResource(ctx)
		return
	}
	plan.SecretEnvVars = types.MapNull(types.StringType)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// updateInstance applies the in-place changes — the stopped-first
// resize (stop → resize → start, honoring the desired running
// posture), stop/start flips with their precondition GETs, the env
// merge, always-on, exposure — then refreshes from a fresh read.
// Only a set (non-null) plan value is managed; removing an attribute
// stops managing it. Returns true when the instance turned out to be
// gone (the caller removes it from state).
func updateInstance(ctx context.Context, pd *providerData, plan, state *instanceModel, diags *diag.Diagnostics) bool {
	id := plan.ID.ValueString()

	if allocationChanged(plan, state) {
		applyResize(ctx, pd, plan, state, id, diags)
		if diags.HasError() {
			return false
		}
	} else {
		applyRunningFlip(ctx, pd, plan, state, id, diags)
		if diags.HasError() {
			return false
		}
	}

	applyEnvVars(ctx, pd, plan, state, id, diags)
	if diags.HasError() {
		return false
	}

	applyAlwaysOn(ctx, pd, plan, state, id, diags)
	if diags.HasError() {
		return false
	}
	applyExposure(ctx, pd, plan, state, id, diags)
	if diags.HasError() {
		return false
	}

	return readInstance(ctx, pd, plan, diags)
}

// applyEnvVars diffs the state map against the plan map and sends one
// in-place merge PATCH (#731, ADR-0052): set is the keys added or
// changed, remove the state keys the plan no longer names. A null plan
// means "no user env" — every key this resource previously managed is
// removed. There is no status gate: the server owns the status matrix
// (pending/error/lost get a row-only write, running/starting also
// update the live Secret, deploying/restoring/deleting refuse).
//
// An unknown value cannot be diffed. This is an apply-time
// impossibility (the framework resolves the plan before Update), but
// the only honest answer is a warning plus a skip: never send "" for
// an unknown value and never remove the keys on the strength of an
// unknown.
func applyEnvVars(ctx context.Context, pd *providerData, plan, state *instanceModel, id string, diags *diag.Diagnostics) {
	const summary = "Instance environment not updated"

	if plan.EnvVars.IsUnknown() {
		diags.AddWarning(summary, fmt.Sprintf(
			"instance %s: env_vars is unknown at apply time — skipped the in-place environment update; the keys this resource manages are left untouched.", id))
		// The wrapper writes the plan back to state; nothing was sent,
		// so every env-related state field must keep its prior value —
		// including a bumped secret version (otherwise the rotation is
		// consumed without ever reaching the app).
		plan.EnvVars = state.EnvVars
		plan.SecretEnvVarsVersion = state.SecretEnvVarsVersion
		plan.SecretEnvVarsKeys = state.SecretEnvVarsKeys
		return
	}

	want := map[string]string{}
	if !plan.EnvVars.IsNull() {
		for key, value := range plan.EnvVars.Elements() {
			s, ok := value.(types.String)
			if !ok || s.IsUnknown() {
				diags.AddWarning(summary, fmt.Sprintf(
					"instance %s: env_vars[%q] is unknown at apply time — skipped the in-place environment update; the keys this resource manages are left untouched.", id, key))
				plan.EnvVars = state.EnvVars
				plan.SecretEnvVarsVersion = state.SecretEnvVarsVersion
				plan.SecretEnvVarsKeys = state.SecretEnvVarsKeys
				return
			}
			if s.IsNull() {
				diags.AddError(
					"Invalid env_vars",
					fmt.Sprintf("env_vars[%q] is null — remove the key instead; an empty string is a value", key),
				)
				return
			}
			want[key] = s.ValueString()
		}
	}

	// The version integer is the only visible rotation signal: the
	// write-only map itself never diffs. An unknown secret value at
	// apply time cannot be sent; keep the prior version and keys so
	// state stays truthful.
	if plan.SecretEnvVarsVersion.IsUnknown() || plan.SecretEnvVars.IsUnknown() {
		diags.AddWarning(summary, fmt.Sprintf(
			"instance %s: secret_env_vars is unknown at apply time — skipped the in-place environment update; the secret keys this resource manages are left untouched.", id))
		plan.EnvVars = state.EnvVars
		plan.SecretEnvVarsVersion = state.SecretEnvVarsVersion
		plan.SecretEnvVarsKeys = state.SecretEnvVarsKeys
		return
	}

	secretChanged := !plan.SecretEnvVarsVersion.Equal(state.SecretEnvVarsVersion)
	wantSecret := map[string]string{}
	desiredKeys := secretKeysFromList(ctx, state.SecretEnvVarsKeys, diags)
	if diags.HasError() {
		return
	}
	if secretChanged {
		diags.Append(secretEnvPairDiagnostics(plan.SecretEnvVars, plan.SecretEnvVarsVersion)...)
		if diags.HasError() {
			return
		}
		var d diag.Diagnostics
		wantSecret, d = secretEnvMap(ctx, plan.SecretEnvVars)
		diags.Append(d...)
		if diags.HasError() {
			return
		}
		desiredKeys = sortedKeys(wantSecret)
	} else {
		// The version is the only visible secret signal; a key-set
		// change without a bump would otherwise be consumed
		// inconsistently (a move from secret to env could later be
		// removed as a stale secret). Refuse it and name the keys.
		configKeys, d := secretEnvMap(ctx, plan.SecretEnvVars)
		diags.Append(d...)
		if diags.HasError() {
			return
		}
		configured := sortedKeys(configKeys)
		if !sameKeys(configured, desiredKeys) {
			diags.AddError(
				"secret_env_vars keys changed without a version bump",
				fmt.Sprintf("the managed secret key set changed (%s). Bump secret_env_vars_version so the provider can apply the key-set change; a write-only map cannot produce a diff on its own.",
					keyDelta(configured, desiredKeys)),
			)
			return
		}
	}

	// A key lives in exactly one of the two maps. ValidateConfig
	// refuses overlap at plan time; this guard also covers direct
	// callers and keeps a mistaken config from sending two writes for
	// one key.
	if overlap := mapOverlap(want, wantSecret); len(overlap) > 0 {
		diags.AddError(
			"Invalid environment variables",
			fmt.Sprintf("keys present in both env_vars and secret_env_vars: %s — put each key in exactly one map", strings.Join(overlap, ", ")),
		)
		return
	}

	have := map[string]string{}
	if !state.EnvVars.IsNull() && !state.EnvVars.IsUnknown() {
		for key, value := range state.EnvVars.Elements() {
			if s, ok := value.(types.String); ok && !s.IsUnknown() {
				have[key] = s.ValueString()
			}
		}
	}

	set := make(map[string]string)
	for key, value := range want {
		if old, ok := have[key]; !ok || old != value {
			set[key] = value
		}
	}
	remove := map[string]bool{}
	for key := range have {
		if _, ok := want[key]; !ok {
			remove[key] = true
		}
	}
	// Rotation re-sends every secret key. Key removal is the prior
	// secret key list minus the desired list.
	for key, value := range wantSecret {
		set[key] = value
	}
	desired := map[string]bool{}
	for _, key := range desiredKeys {
		desired[key] = true
	}
	for _, key := range secretKeysFromList(ctx, state.SecretEnvVarsKeys, diags) {
		// A key still present in env_vars is never removed — the user
		// moved it, not dropped it.
		if _, stillEnv := want[key]; stillEnv {
			continue
		}
		if !desired[key] {
			remove[key] = true
		}
	}
	if diags.HasError() {
		return
	}
	// Never remove a key in the same PATCH that sets it (a key moving
	// between the two maps is a set, not a remove).
	for key := range set {
		delete(remove, key)
	}
	removeList := make([]string, 0, len(remove))
	for key := range remove {
		removeList = append(removeList, key)
	}
	sort.Strings(removeList)

	plan.SecretEnvVarsKeys = secretKeysList(desiredKeys)
	if len(set) == 0 && len(removeList) == 0 {
		return
	}

	if _, err := pd.client.UpdateInstanceEnv(ctx, id, set, removeList); err != nil {
		diags.AddError("Unable to update the Instance environment", err.Error())
	}
}

// allocationChanged did the plan move any allocation dimension? All
// three are Required, so both sides are always known here.
func allocationChanged(plan, state *instanceModel) bool {
	return plan.CPUCores.ValueFloat64() != state.CPUCores.ValueFloat64() ||
		plan.MemoryMB.ValueInt64() != state.MemoryMB.ValueInt64() ||
		plan.StorageGB.ValueInt64() != state.StorageGB.ValueInt64()
}

// applyResize runs the stopped-first sequence the guard demands:
// running ⇒ stop → resize → start (when running is desired);
// stopped ⇒ resize (then start only on a requested flip); sleeping ⇒
// start, wait for running, stop, resize, start. Anything else can't
// reach stopped — a diagnostic naming the status; a pending row can
// NEVER be stopped, so its only fix is replace. The server enforces
// the rest (storage up-only, no-op, capability) — its 400 surfaces
// verbatim.
func applyResize(ctx context.Context, pd *providerData, plan, state *instanceModel, id string, diags *diag.Diagnostics) {
	status := currentStatus(ctx, pd, id, diags)
	if diags.HasError() {
		return
	}

	// Whether the row should be running when we're done.
	wantRunning := desiredRunning(plan.Running)

	stopForResize := func() bool {
		if _, err := pd.client.InstanceAction(ctx, id, "stop", map[string]any{}); err != nil {
			diags.AddError("Unable to stop the Instance for resize", err.Error())
			return false
		}
		return true
	}
	resizeNow := func() bool {
		if _, err := pd.client.ResizeInstance(ctx, id,
			plan.CPUCores.ValueFloat64(), plan.MemoryMB.ValueInt64(), plan.StorageGB.ValueInt64()); err != nil {
			diags.AddError("Unable to resize the Instance", err.Error())
			return false
		}
		return true
	}
	startAfterResize := func() bool {
		if !wantRunning {
			return true
		}
		if _, err := pd.client.InstanceAction(ctx, id, "start", map[string]any{}); err != nil {
			diags.AddError("Unable to start the Instance after resize", err.Error())
			return false
		}
		return true
	}

	switch status {
	case "running":
		if !stopForResize() || !resizeNow() {
			return
		}
		startAfterResize()
	case "stopped":
		if !resizeNow() {
			return
		}
		// Start only on a requested flip (plan running vs state not) — a
		// drifted-stopped row stays stopped; status surfaces the drift.
		if wantRunning && !desiredRunning(state.Running) {
			if _, err := pd.client.InstanceAction(ctx, id, "start", map[string]any{}); err != nil {
				diags.AddError("Unable to start the Instance after resize", err.Error())
			}
		}
	case "sleeping":
		// The idle sweep scaled it away; start is permitted from
		// sleeping — walk it up through running so stop is legal.
		if _, err := pd.client.InstanceAction(ctx, id, "start", map[string]any{}); err != nil {
			diags.AddError("Unable to start the Instance for resize", err.Error())
			return
		}
		if _, err := waitInstanceStatus(ctx, pd, id, "running"); err != nil {
			diags.AddError("Instance did not reach running before resize", fmt.Sprintf("instance %s: %v", id, err))
			return
		}
		if !stopForResize() || !resizeNow() {
			return
		}
		startAfterResize()
	case "pending":
		diags.AddError(
			"Unable to resize a never-deployed instance",
			fmt.Sprintf("instance %s is %q — the resize guard requires stopped, and a pending row can never be stopped. Replace it instead.", id, status),
		)
	default:
		diags.AddError(
			"Unable to resize the Instance",
			fmt.Sprintf("instance %s is %q — the resize guard requires stopped. Retry once it settles, or replace.", id, status),
		)
	}
}

// currentStatus reads the row's status for precondition checks.
func currentStatus(ctx context.Context, pd *providerData, id string, diags *diag.Diagnostics) string {
	in, err := pd.client.GetInstance(ctx, id)
	if err != nil {
		diags.AddError("Unable to read the Instance", err.Error())
		return ""
	}
	return in.Status
}

// applyRunningFlip moves the posture when the plan asks for a change,
// with the precondition GET first: the stop verb refuses anything but
// running (a platform-slept row is already satisfied — skipping is
// what un-wedges running=false after an idle sleep), the start verb
// refuses anything but stopped/sleeping.
func applyRunningFlip(ctx context.Context, pd *providerData, plan, state *instanceModel, id string, diags *diag.Diagnostics) {
	if !desiredRunning(plan.Running) && desiredRunning(state.Running) {
		status := currentStatus(ctx, pd, id, diags)
		if diags.HasError() {
			return
		}
		switch status {
		case "running":
			if _, err := pd.client.InstanceAction(ctx, id, "stop", map[string]any{}); err != nil {
				diags.AddError("Unable to stop the Instance", err.Error())
			}
		case "stopped", "sleeping":
			// Already at/below running — satisfied, no PATCH.
		default:
			diags.AddError(
				"Unable to stop the Instance",
				fmt.Sprintf("instance %s is %q — the API only stops a running row. Retry once it settles.", id, status),
			)
		}
	}
	if diags.HasError() {
		return
	}

	if desiredRunning(plan.Running) && !desiredRunning(state.Running) {
		status := currentStatus(ctx, pd, id, diags)
		if diags.HasError() {
			return
		}
		switch status {
		case "stopped", "sleeping":
			if _, err := pd.client.InstanceAction(ctx, id, "start", map[string]any{}); err != nil {
				diags.AddError("Unable to start the Instance", err.Error())
			}
		case "running", "starting":
			// Running, or already heading there — satisfied.
		default:
			diags.AddError(
				"Unable to start the Instance",
				fmt.Sprintf("instance %s is %q — the API only starts a stopped or sleeping row. Retry once it settles (an error row needs re-deploying first).", id, status),
			)
		}
	}
}

// applyAlwaysOn PATCHes always-on when the plan sets it and it differs
// from state. Attribute-only — no posture preconditions.
func applyAlwaysOn(ctx context.Context, pd *providerData, plan, state *instanceModel, id string, diags *diag.Diagnostics) {
	if !plan.AlwaysOn.IsNull() && !plan.AlwaysOn.IsUnknown() &&
		(state.AlwaysOn.IsNull() || plan.AlwaysOn.ValueBool() != state.AlwaysOn.ValueBool()) {
		if _, err := pd.client.InstanceAction(ctx, id, "always-on", map[string]any{"always_on": plan.AlwaysOn.ValueBool()}); err != nil {
			diags.AddError("Unable to set always_on", err.Error())
		}
	}
}

// applyExposure PATCHes exposure when the plan sets it and it differs
// from state. The server gates the cluster apply on a managed
// namespace; elsewhere it's an attribute write — no preconditions.
func applyExposure(ctx context.Context, pd *providerData, plan, state *instanceModel, id string, diags *diag.Diagnostics) {
	if !plan.Exposure.IsNull() && !plan.Exposure.IsUnknown() &&
		(state.Exposure.IsNull() || plan.Exposure.ValueString() != state.Exposure.ValueString()) {
		if _, err := pd.client.InstanceAction(ctx, id, "exposure", map[string]any{"exposure": plan.Exposure.ValueString()}); err != nil {
			diags.AddError("Unable to set exposure", err.Error())
		}
	}
}

func (r *instanceResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state instanceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	pd := r.data(&resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	deleteInstance(ctx, pd, state.ID, &resp.Diagnostics)
}

// deleteInstance DELETEs and waits out the async teardown — the row
// hard-deletes only when the K8s resources are gone, so "gone" is the
// 404, not the DELETE response. A 404 ON the DELETE is success too:
// idempotent destroy (no restore window — a panel delete between
// refresh and apply must not fail the plan).
func deleteInstance(ctx context.Context, pd *providerData, id types.String, diags *diag.Diagnostics) {
	err := pd.client.DeleteInstance(ctx, id.ValueString())
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return // already gone
		}
		diags.AddError("Unable to delete the Instance", err.Error())
		return
	}

	err = waitInstanceGone(ctx, pd, id.ValueString())
	if err != nil {
		diags.AddError(
			"Instance teardown did not finish",
			fmt.Sprintf("instance %s still exists after DELETE: %v", id.ValueString(), err),
		)
	}
}

// waitInstanceGone polls until the row 404s.
func waitInstanceGone(ctx context.Context, pd *providerData, id string) error {
	return pollWithDeadline(ctx, pd.pollInterval, pd.pollTimeout, func(ctx context.Context) (bool, error) {
		_, err := pd.client.GetInstance(ctx, id)
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return false, nil
	})
}

func (r *instanceResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// By instance id: terraform import fluxvale_instance.x <id>. Read
	// fills the wire-mirrored fields — including the FK ids
	// (organization_id, app_version_id, asked for via the sparse
	// fieldset). app/app_version (the slug pair) have no wire echo —
	// keep your config matching what you deployed. env_vars imports
	// null; the first apply that sets them sends a full set (in place).
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// resolveAppVersionID resolves the schema's two identity forms to a
// version id: the explicit app_version_id wins; app + app_version
// resolve through the catalog (slug → app, version string → row);
// neither form is an error. A lookup, not a rule — an unknown slug or
// version is the server's 404 surfaced verbatim.
func resolveAppVersionID(ctx context.Context, c *client.Client, data *instanceModel) (string, error) {
	if !data.AppVersionID.IsNull() && !data.AppVersionID.IsUnknown() {
		return data.AppVersionID.ValueString(), nil
	}

	hasApp := !data.App.IsNull() && !data.App.IsUnknown()
	hasVersion := !data.AppVersion.IsNull() && !data.AppVersion.IsUnknown()
	if !hasApp || !hasVersion {
		return "", errors.New("set app_version_id, or both app and app_version (a catalog slug and version string)")
	}

	app, err := c.GetAppBySlug(ctx, data.App.ValueString())
	if err != nil {
		return "", fmt.Errorf("looking up app %q: %w", data.App.ValueString(), err)
	}

	versions, err := c.ListAppVersions(ctx, app.ID)
	if err != nil {
		return "", fmt.Errorf("listing versions of app %q: %w", data.App.ValueString(), err)
	}
	version, ok := client.FindAppVersion(versions, data.AppVersion.ValueString())
	if !ok {
		available := make([]string, 0, len(versions))
		for _, v := range versions {
			available = append(available, v.Version)
		}
		return "", fmt.Errorf("app %q has no version %q (available: %s)",
			data.App.ValueString(), data.AppVersion.ValueString(), strings.Join(available, ", "))
	}
	return version.ID, nil
}

// envVarMap converts the schema's env_vars into the wire map. null ⇒
// nil (omit — the server merges the version's defaults).
func envVarMap(ctx context.Context, v types.Map) (map[string]string, diag.Diagnostics) {
	if v.IsNull() || v.IsUnknown() {
		return nil, diag.Diagnostics{}
	}
	var elems map[string]types.String
	diags := v.ElementsAs(ctx, &elems, false)
	if diags.HasError() {
		return nil, diags
	}
	out := make(map[string]string, len(elems))
	for k, val := range elems {
		if val.IsNull() {
			diags.AddError(
				"Invalid env_vars",
				fmt.Sprintf("env_vars[%q] is null — remove the key instead; an empty string is a value", k),
			)
			continue
		}
		out[k] = val.ValueString()
	}
	if diags.HasError() {
		return nil, diags
	}
	return out, diag.Diagnostics{}
}

// desiredRunning is the posture a running value asks for: null means
// the default (true).
func desiredRunning(v types.Bool) bool {
	return v.IsNull() || v.IsUnknown() || v.ValueBool()
}

func deployErr(id string, err error) string {
	return fmt.Sprintf("instance %s: %v", id, err)
}
