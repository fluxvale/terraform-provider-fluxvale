package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/fluxvale/fluxvale/apps/tfprovider/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ resource.ResourceWithImportState = (*backupsResource)(nil)

// backupsResource manages fluxvale_instance_backups — an instance's
// backup destination (R2 first). A skin over the routed saves: every
// business rule (endpoint/bucket/prefix validation, retention bounds,
// paywall, the cluster converge — secret keep-or-rotate, repo URL
// recompute, quota margin) is the server's; its 400s (attribute/guard
// refusals), 403s (scope/policy), and 404s surface verbatim — not
// something to pre-empt client-side.
type backupsResource struct {
	pd *providerData
}

// NewBackupsResource returns the resource factory provider.go
// registers.
func NewBackupsResource() resource.Resource {
	return &backupsResource{}
}

func (r *backupsResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_instance_backups"
}

// backupsModel mirrors the schema. secret_access_key is write-only —
// never overwritten from the wire; id is the settings ROW's id (the
// PATCH/DELETE path key), which reads refresh because they go by
// instance.
type backupsModel struct {
	InstanceID      types.String `tfsdk:"instance_id"`
	Endpoint        types.String `tfsdk:"endpoint"`
	Bucket          types.String `tfsdk:"bucket"`
	Prefix          types.String `tfsdk:"prefix"`
	AccessKeyID     types.String `tfsdk:"access_key_id"`
	RetentionKeep   types.Int64  `tfsdk:"retention_keep"`
	Enabled         types.Bool   `tfsdk:"enabled"`
	SecretAccessKey types.String `tfsdk:"secret_access_key"`
	ID              types.String `tfsdk:"id"`
}

func (r *backupsResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "An instance's backup destination — the S3-compatible coordinates (R2 first) its restic backups run against. One settings row per instance; every routed save converges the cluster server-side (the credential Secret, the restic repo URL, the backup quota margin).",
		Attributes: map[string]schema.Attribute{
			"instance_id": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The fluxvale_instance.id the settings ride on — Terraform interpolation is the point: " +
					"R2 outputs feed the credential fields. Changing it replaces the resource (the API has no move route). " +
					"Import takes THIS id (reads are instance-scoped, not row-scoped).",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"endpoint": schema.StringAttribute{
				Required:    true,
				Description: "S3-compatible endpoint — scheme + host (and optional port), no path. For R2: https://<account_id>.r2.cloudflarestorage.com. Bounds and shape enforce server-side (a 400 is the surface).",
			},
			"bucket": schema.StringAttribute{
				Required:    true,
				Description: "The bucket name (S3 naming rules enforce server-side).",
			},
			"access_key_id": schema.StringAttribute{
				Required:    true,
				Description: "The S3 access key ID — an identifier that pairs with secret_access_key; it authenticates nothing alone.",
			},
			"prefix": schema.StringAttribute{
				Optional:    true,
				Description: "Optional key prefix inside the bucket — path segments with no leading/trailing slash (the composer owns the separators). Clearing it clears the stored prefix on update.",
			},
			"retention_keep": schema.Int64Attribute{
				Optional:    true,
				Computed:    true,
				Description: "restic forget --keep-last N. Defaults to 7 server-side; the 2–30 bounds enforce server-side (a 400 is the surface, not a client-side check). The state carries the server's value.",
			},
			"enabled": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Description: "The pause switch — defaults to true. A pause is a plain settings update server-side: it touches nothing cluster-side (the Secret and quota persist so re-enabling reuses the same restic repo). The state carries the server's value.",
			},
			"secret_access_key": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "The S3 secret key — write-only: the API never echoes it. Set (non-null) ⇒ applied with the save, " +
					"rotating any stored credential; unset ⇒ the stored credential is kept. A FIRST create without it lands " +
					"quota-only (the API's permissive shape) and backups won't run until a later save supplies it. " +
					"The configured value is kept verbatim in state.",
			},
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "The backup_setting row id — the PATCH/DELETE path key. Refreshed on read (reads go by instance_id, so a row re-created out-of-band re-points the resource).",
			},
		},
	}
}

func (r *backupsResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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
func (r *backupsResource) data(diags *diag.Diagnostics) *providerData {
	if r.pd == nil {
		diags.AddError(
			"Provider not configured",
			"The provider was not configured before this resource was used — please report this bug.",
		)
	}
	return r.pd
}

func (r *backupsResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data backupsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	pd := r.data(&resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	row, err := pd.client.SaveBackupSetting(ctx, data.InstanceID.ValueString(), backupsInput(data))
	if err != nil {
		// A post-2xx parse drift still leaves the row on the platform —
		// recover through the instance-scoped read before failing.
		recovered, ok := recoverBackupRow(ctx, pd, data.InstanceID.ValueString(), err, &resp.Diagnostics)
		if !ok {
			return
		}
		row = recovered
	}
	mapBackups(&data, row)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// recoverBackupRow re-reads the row by instance after a failed save.
// A row that reads back cleanly means the save landed (its own
// response just didn't parse) — a warning says so, the row is the
// truth. Returns false when the caller must stop (the original error
// stands: nothing landed, or the recovery read failed too).
func recoverBackupRow(ctx context.Context, pd *providerData, instanceID string, saveErr error, diags *diag.Diagnostics) (client.BackupSetting, bool) {
	row, err := pd.client.GetBackupSetting(ctx, instanceID)
	if err != nil {
		diags.AddError("Unable to save the backup settings", saveErr.Error())
		return client.BackupSetting{}, false
	}
	diags.AddWarning(
		"Recovered the backup settings from the instance read",
		"The save's response did not parse, but the row reads back cleanly by instance — Terraform carries the "+
			"re-read row in state. If this repeats, the API's response shape has drifted.",
	)
	return row, true
}

// backupsInput maps the model onto the save body. A null secret is ""
// — omitted, which the server reads as keep-what's-stored (a first
// save without one lands the API's permissive quota-only shape).
func backupsInput(data backupsModel) client.BackupSettingInput {
	in := client.BackupSettingInput{
		Endpoint:    data.Endpoint.ValueString(),
		Bucket:      data.Bucket.ValueString(),
		AccessKeyID: data.AccessKeyID.ValueString(),
	}
	if !data.Prefix.IsNull() && !data.Prefix.IsUnknown() {
		prefix := data.Prefix.ValueString()
		in.Prefix = &prefix
	}
	if !data.RetentionKeep.IsNull() && !data.RetentionKeep.IsUnknown() {
		retention := data.RetentionKeep.ValueInt64()
		in.RetentionKeep = &retention
	}
	if !data.Enabled.IsNull() && !data.Enabled.IsUnknown() {
		enabled := data.Enabled.ValueBool()
		in.Enabled = &enabled
	}
	if !data.SecretAccessKey.IsNull() && !data.SecretAccessKey.IsUnknown() {
		in.SecretAccessKey = data.SecretAccessKey.ValueString()
	}
	return in
}

func (r *backupsResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data backupsModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	pd := r.data(&resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	row, err := pd.client.GetBackupSetting(ctx, data.InstanceID.ValueString())
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Unable to read the backup settings", err.Error())
		return
	}
	mapBackups(&data, row)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// mapBackups writes the wire's fields into the model. secret_access_key
// has no wire source — the configured value survives Read untouched,
// like the instance's env_vars.
func mapBackups(data *backupsModel, row client.BackupSetting) {
	data.ID = types.StringValue(row.ID)
	data.InstanceID = types.StringValue(row.InstanceID)
	data.Endpoint = types.StringValue(row.Endpoint)
	data.Bucket = types.StringValue(row.Bucket)
	data.Prefix = types.StringPointerValue(row.Prefix)
	data.AccessKeyID = types.StringValue(row.AccessKeyID)
	data.RetentionKeep = types.Int64Value(row.RetentionKeep)
	data.Enabled = types.BoolValue(row.Enabled)
}

func (r *backupsResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan backupsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	pd := r.data(&resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	// The full save: all fields ride; a set secret rotates, an absent
	// one keeps the stored credential. The row id comes from the plan
	// (carried from state; reads keep it current).
	row, err := pd.client.UpdateBackupSetting(ctx, plan.ID.ValueString(), backupsInput(plan))
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			// The row is gone (a panel delete won the race) — removed,
			// never plan-over-ghost.
			resp.State.RemoveResource(ctx)
			return
		}
		// Same post-2xx drift as create: recover through the
		// instance-scoped read before failing.
		recovered, ok := recoverBackupRow(ctx, pd, plan.InstanceID.ValueString(), err, &resp.Diagnostics)
		if !ok {
			return
		}
		row = recovered
	}
	mapBackups(&plan, row)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *backupsResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state backupsModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	pd := r.data(&resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	// The "stop backing up entirely" verb: a plain row delete — no
	// cluster state (the namespace Secret persists by design, so
	// nothing re-provisions a repo behind the user's back). A 404 is
	// already-gone: idempotent destroy.
	if err := pd.client.DeleteBackupSetting(ctx, state.ID.ValueString()); err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return
		}
		resp.Diagnostics.AddError("Unable to delete the backup settings", err.Error())
	}
}

func (r *backupsResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// By INSTANCE id — the path key every read goes through: terraform
	// import fluxvale_instance_backups.x <instance_id>. Read fills the
	// row's fields; secret_access_key stays null (the API never echoes
	// it) — your config's value (or its absence) is the rotation
	// decision on the next apply.
	resource.ImportStatePassthroughID(ctx, path.Root("instance_id"), req, resp)
}
