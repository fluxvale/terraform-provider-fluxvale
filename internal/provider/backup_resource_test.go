package provider

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// newSchemadBackups builds the backups resource with its schema, the
// way the proto6 unit-test pattern does.
func newSchemadBackups(ctx context.Context, t *testing.T) (*backupsResource, schema.Schema) {
	t.Helper()
	r := NewBackupsResource().(*backupsResource)
	schemaResp := &resource.SchemaResponse{}
	r.Schema(ctx, resource.SchemaRequest{}, schemaResp)
	if schemaResp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", schemaResp.Diagnostics)
	}
	return r, schemaResp.Schema
}

// backupsToRaw/rawToBackups round-trip a model through a State/Plan,
// the instance_crud_test helpers' shape.
func backupsToRaw(ctx context.Context, t *testing.T, sch schema.Schema, data *backupsModel) tftypes.Value {
	t.Helper()
	st := tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)}
	if diags := st.Set(ctx, data); diags.HasError() {
		t.Fatalf("State.Set: %v", diags)
	}
	return st.Raw
}

func rawToBackups(ctx context.Context, t *testing.T, sch schema.Schema, raw tftypes.Value) backupsModel {
	t.Helper()
	st := tfsdk.State{Schema: sch, Raw: raw}
	var data backupsModel
	if diags := st.Get(ctx, &data); diags.HasError() {
		t.Fatalf("State.Get: %v", diags)
	}
	return data
}

// Create over the wrapper: the save body's shape is the wire contract
// — everything rides data.attributes, instance_id rides the PATH (a
// body copy is refused outright by ash_json_api), the secret is a
// write-only attribute among the coordinates.
func TestBackupsCreateBodyShapeAndState(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadBackups(ctx, t)

	f := newFakeAPI(t)
	r.pd = f.testClient()

	planData := testBackups(func(d *backupsModel) {
		d.Prefix = types.StringValue("fluxvale/app")
		d.SecretAccessKey = types.StringValue("provider-secret-value")
	})
	req := resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: backupsToRaw(ctx, t, sch, &planData)},
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
	}
	r.Create(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Create: %v", resp.Diagnostics)
	}

	post := findOne(t, f.all(), http.MethodPost, "/api/v1/instances/inst-1/backup-setting")
	if post.auth != "Bearer fvpat_test" {
		t.Errorf("POST Authorization = %q, want the bearer token", post.auth)
	}
	attrs := attrsOf(t, post)
	if attrs["endpoint"] != "https://account.r2.cloudflarestorage.com" ||
		attrs["bucket"] != "fluxvale-kavita" ||
		attrs["access_key_id"] != "AKIAIOSFODNN7EXAMPLE" {
		t.Errorf("coordinates = %v, want endpoint/bucket/access_key_id in attributes", attrs)
	}
	if attrs["secret_access_key"] != "provider-secret-value" {
		t.Errorf("secret_access_key = %v, want the write-only argument inside attributes", attrs["secret_access_key"])
	}
	if attrs["prefix"] != "fluxvale/app" {
		t.Errorf("prefix = %v, want the set value", attrs["prefix"])
	}
	if _, ok := attrs["instance_id"]; ok {
		t.Error("instance_id in the body — the path is the only intake; the route refuses a body copy")
	}
	if _, ok := attrs["retention_keep"]; ok {
		t.Error("retention_keep sent at create with no config value — the server default (7) should apply")
	}
	if _, ok := attrs["enabled"]; ok {
		t.Error("enabled sent at create with no config value — the server default (true) should apply")
	}

	got := rawToBackups(ctx, t, sch, resp.State.Raw)
	if got.ID.ValueString() != "bs-1" {
		t.Errorf("id = %s, want the row id bs-1", got.ID.ValueString())
	}
	if got.RetentionKeep.ValueInt64() != 7 || !got.Enabled.ValueBool() {
		t.Errorf("retention/enabled = %d/%t, want the row's 7/true", got.RetentionKeep.ValueInt64(), got.Enabled.ValueBool())
	}
	if got.SecretAccessKey.ValueString() != "provider-secret-value" {
		t.Errorf("secret in state = %q, want the configured value kept verbatim", got.SecretAccessKey.ValueString())
	}
}

// The quota-only first create: no secret configured ⇒ the body omits
// the argument (the API's permissive shape — backups won't run until a
// later save supplies the credential; that story is the schema's).
func TestBackupsCreateOmitsUnsetSecret(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadBackups(ctx, t)

	f := newFakeAPI(t)
	r.pd = f.testClient()

	planData := testBackups()
	req := resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: backupsToRaw(ctx, t, sch, &planData)},
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
	}
	r.Create(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Create: %v", resp.Diagnostics)
	}

	attrs := attrsOf(t, findOne(t, f.all(), http.MethodPost, "/backup-setting"))
	if _, ok := attrs["secret_access_key"]; ok {
		t.Errorf("secret_access_key = %v, want omitted (absent keeps/stays nothing-stored)", attrs["secret_access_key"])
	}
}

// Update: the full save rides; a NULL secret is absent — keep the
// stored credential (P1's blank-means-keep, minus the blank).
func TestBackupsUpdateKeepsStoredSecretByOmitting(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadBackups(ctx, t)

	f := newFakeAPI(t)
	r.pd = f.testClient()

	planData := testBackups(func(d *backupsModel) {
		d.RetentionKeep = types.Int64Value(14)
		d.Enabled = types.BoolValue(true)
		d.ID = types.StringValue("bs-1")
	})
	req := resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: sch, Raw: backupsToRaw(ctx, t, sch, &planData)},
		State: tfsdk.State{Schema: sch, Raw: backupsToRaw(ctx, t, sch, &planData)},
	}
	resp := &resource.UpdateResponse{
		State: tfsdk.State{Schema: sch, Raw: backupsToRaw(ctx, t, sch, &planData)},
	}
	r.Update(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", resp.Diagnostics)
	}

	patched := attrsOf(t, findOne(t, f.all(), http.MethodPatch, "/api/v1/backup-settings/bs-1"))
	for _, field := range []string{"endpoint", "bucket", "prefix", "access_key_id", "retention_keep", "enabled"} {
		if _, ok := patched[field]; !ok {
			t.Errorf("PATCH body missing %s — the update is a full save", field)
		}
	}
	if _, ok := patched["secret_access_key"]; ok {
		t.Errorf("secret_access_key = %v, want omitted — a null secret keeps the stored credential", patched["secret_access_key"])
	}
	if patched["retention_keep"] != 14.0 {
		t.Errorf("retention_keep = %v, want 14", patched["retention_keep"])
	}
}

// Update with a set secret rotates (the value rides the PATCH).
func TestBackupsUpdateRotatesSecret(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadBackups(ctx, t)

	f := newFakeAPI(t)
	r.pd = f.testClient()

	planData := testBackups(func(d *backupsModel) {
		d.SecretAccessKey = types.StringValue("rotated-secret")
		d.ID = types.StringValue("bs-1")
	})
	req := resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: sch, Raw: backupsToRaw(ctx, t, sch, &planData)},
		State: tfsdk.State{Schema: sch, Raw: backupsToRaw(ctx, t, sch, &planData)},
	}
	resp := &resource.UpdateResponse{
		State: tfsdk.State{Schema: sch, Raw: backupsToRaw(ctx, t, sch, &planData)},
	}
	r.Update(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", resp.Diagnostics)
	}

	attrs := attrsOf(t, findOne(t, f.all(), http.MethodPatch, "/backup-settings/bs-1"))
	if attrs["secret_access_key"] != "rotated-secret" {
		t.Errorf("secret_access_key = %v, want the rotation value", attrs["secret_access_key"])
	}
}

// Clearing the prefix sends an explicit null — an omitted key would
// silently keep the stored prefix.
func TestBackupsUpdateClearsPrefixWithNull(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadBackups(ctx, t)

	f := newFakeAPI(t)
	r.pd = f.testClient()

	planData := testBackups(func(d *backupsModel) { d.ID = types.StringValue("bs-1") }) // prefix null
	req := resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: sch, Raw: backupsToRaw(ctx, t, sch, &planData)},
		State: tfsdk.State{Schema: sch, Raw: backupsToRaw(ctx, t, sch, &planData)},
	}
	resp := &resource.UpdateResponse{
		State: tfsdk.State{Schema: sch, Raw: backupsToRaw(ctx, t, sch, &planData)},
	}
	r.Update(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", resp.Diagnostics)
	}

	attrs := attrsOf(t, findOne(t, f.all(), http.MethodPatch, "/backup-settings/bs-1"))
	v, ok := attrs["prefix"]
	if !ok {
		t.Fatal("prefix missing from the PATCH body — clearing needs the explicit null")
	}
	if v != nil {
		t.Errorf("prefix = %v, want null", v)
	}
}

// Read is by INSTANCE id (the path key), maps the row, and the
// write-only secret survives — even a doc carrying a hostile
// secret_access_key can't leak it into the model.
func TestBackupsReadMapsRowAndKeepsSecret(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadBackups(ctx, t)

	f := newFakeAPI(t)
	// A hostile secret key the platform would never send; the parser
	// has no field for it.
	hostile := strings.ReplaceAll(backupRowDoc, `"enabled": true`,
		`"enabled": true, "secret_access_key": "hostile"`)
	f.backupGets = []scriptedResponse{script(200, hostile)}
	r.pd = f.testClient()

	stateData := testBackups(func(d *backupsModel) {
		d.SecretAccessKey = types.StringValue("configured-secret")
	})
	req := resource.ReadRequest{
		State: tfsdk.State{Schema: sch, Raw: backupsToRaw(ctx, t, sch, &stateData)},
	}
	resp := &resource.ReadResponse{
		State: tfsdk.State{Schema: sch, Raw: backupsToRaw(ctx, t, sch, &stateData)},
	}
	r.Read(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", resp.Diagnostics)
	}

	get := findOne(t, f.all(), http.MethodGet, "/api/v1/instances/inst-1/backup-setting")
	if get.method != http.MethodGet || !strings.HasSuffix(get.path, "/instances/inst-1/backup-setting") {
		t.Fatalf("read hit %s %s, want GET by instance id", get.method, get.path)
	}

	got := rawToBackups(ctx, t, sch, resp.State.Raw)
	if got.ID.ValueString() != "bs-1" || got.Bucket.ValueString() != "fluxvale-kavita" {
		t.Errorf("id/bucket = %s/%s, want the row's", got.ID.ValueString(), got.Bucket.ValueString())
	}
	if got.SecretAccessKey.ValueString() != "configured-secret" {
		t.Errorf("secret in state = %q, want the configured value kept", got.SecretAccessKey.ValueString())
	}
}

// A 404 on read means the settings row is gone — removed-from-terraform.
func TestBackupsRead404Removes(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadBackups(ctx, t)

	f := newFakeAPI(t)
	f.backupGets = []scriptedResponse{script(http.StatusNotFound, `{"errors": [{"status": "404"}]}`)}
	r.pd = f.testClient()

	stateData := testBackups()
	req := resource.ReadRequest{
		State: tfsdk.State{Schema: sch, Raw: backupsToRaw(ctx, t, sch, &stateData)},
	}
	resp := &resource.ReadResponse{
		State: tfsdk.State{Schema: sch, Raw: backupsToRaw(ctx, t, sch, &stateData)},
	}
	r.Read(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsNull() {
		t.Errorf("state after 404 = %v, want null (removed)", resp.State.Raw)
	}
}

// Delete goes by the ROW id (the settings route's path key).
func TestBackupsDeleteByRowID(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadBackups(ctx, t)

	f := newFakeAPI(t)
	r.pd = f.testClient()

	stateData := testBackups(func(d *backupsModel) { d.ID = types.StringValue("bs-9") })
	req := resource.DeleteRequest{
		State: tfsdk.State{Schema: sch, Raw: backupsToRaw(ctx, t, sch, &stateData)},
	}
	resp := &resource.DeleteResponse{}
	r.Delete(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Delete: %v", resp.Diagnostics)
	}
	findOne(t, f.all(), http.MethodDelete, "/api/v1/backup-settings/bs-9")
}

// Import takes the INSTANCE id — the path key every read goes through.
func TestBackupsImportByInstance(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadBackups(ctx, t)

	req := resource.ImportStateRequest{ID: "inst-42"}
	resp := &resource.ImportStateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
	}
	r.ImportState(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("ImportState: %v", resp.Diagnostics)
	}

	got := rawToBackups(ctx, t, sch, resp.State.Raw)
	if got.InstanceID.ValueString() != "inst-42" {
		t.Errorf("instance_id = %s, want the import argument", got.InstanceID.ValueString())
	}
}

// The schema contract: instance_id is the only replaceable field
// (no move route); everything else updates in place — that's the
// resource's whole value. The secret is sensitive.
func TestBackupsSchemaContract(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadBackups(ctx, t)

	want := []string{
		"instance_id", "endpoint", "bucket", "prefix", "access_key_id",
		"retention_keep", "enabled", "secret_access_key", "id",
	}
	if len(sch.Attributes) != len(want) {
		t.Fatalf("schema attributes = %d, want exactly %d (%v)", len(sch.Attributes), len(want), want)
	}
	for _, name := range want {
		if _, ok := sch.Attributes[name]; !ok {
			t.Errorf("schema missing attribute %q", name)
		}
	}

	if !sch.Attributes["instance_id"].(schema.StringAttribute).IsRequired() {
		t.Error("instance_id is not Required")
	}
	for _, name := range []string{"endpoint", "bucket", "access_key_id"} {
		if !sch.Attributes[name].IsRequired() {
			t.Errorf("%s is not Required", name)
		}
	}
	if !sch.Attributes["secret_access_key"].IsSensitive() {
		t.Error("secret_access_key is not sensitive")
	}

	// The in-place set: only instance_id replaces.
	for _, name := range []string{"endpoint", "bucket", "prefix", "retention_keep", "enabled", "secret_access_key"} {
		attr := sch.Attributes[name]
		if attr.IsRequired() && name != "endpoint" && name != "bucket" && name != "access_key_id" {
			t.Errorf("%s unexpectedly Required", name)
		}
	}
	if len(sch.Attributes["instance_id"].(schema.StringAttribute).PlanModifiers) == 0 {
		t.Error("instance_id has no plan modifiers (want RequiresReplace — no move route)")
	}
	if len(sch.Attributes["endpoint"].(schema.StringAttribute).PlanModifiers) != 0 {
		t.Error("endpoint must update in place (PATCH save_settings)")
	}

	md := &resource.MetadataResponse{}
	r.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: "fluxvale"}, md)
	if md.TypeName != "fluxvale_instance_backups" {
		t.Errorf("TypeName = %q, want fluxvale_instance_backups", md.TypeName)
	}
}

// A save whose 2xx response doesn't parse still landed its row — the
// recovery read by instance carries it, with a warning saying why.
func TestBackupsCreateRecoversDriftedSaveResponse(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadBackups(ctx, t)

	f := newFakeAPI(t)
	// retention_keep as a string: the parser's shape-drift error.
	f.backupPostDoc = strings.ReplaceAll(backupRowDoc, `"retention_keep": 7`, `"retention_keep": "seven"`)
	f.backupGets = []scriptedResponse{script(200, backupRowDoc)}
	r.pd = f.testClient()

	planData := testBackups()
	req := resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: backupsToRaw(ctx, t, sch, &planData)},
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
	}
	r.Create(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Create errored: %v — the row exists, the recovery read carries it", resp.Diagnostics)
	}
	assertWarning(t, &resp.Diagnostics, "Recovered the backup settings", "instance")

	got := rawToBackups(ctx, t, sch, resp.State.Raw)
	if got.ID.ValueString() != "bs-1" || got.RetentionKeep.ValueInt64() != 7 {
		t.Errorf("id/retention = %s/%d, want the recovered row", got.ID.ValueString(), got.RetentionKeep.ValueInt64())
	}
}

// The same recovery on update: a drifted PATCH response still leaves
// the row readable by instance.
func TestBackupsUpdateRecoversDriftedResponse(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadBackups(ctx, t)

	f := newFakeAPI(t)
	f.backupPatchDoc = strings.ReplaceAll(backupRowDoc, `"bucket": "fluxvale-kavita"`, `"bucket": 7`)
	f.backupGets = []scriptedResponse{script(200, backupRowDoc)}
	r.pd = f.testClient()

	planData := testBackups(func(d *backupsModel) {
		d.ID = types.StringValue("bs-1")
		d.Enabled = types.BoolValue(true)
		d.RetentionKeep = types.Int64Value(14)
	})
	req := resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: sch, Raw: backupsToRaw(ctx, t, sch, &planData)},
		State: tfsdk.State{Schema: sch, Raw: backupsToRaw(ctx, t, sch, &planData)},
	}
	resp := &resource.UpdateResponse{
		State: tfsdk.State{Schema: sch, Raw: backupsToRaw(ctx, t, sch, &planData)},
	}
	r.Update(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update errored: %v — the row exists, the recovery read carries it", resp.Diagnostics)
	}
	assertWarning(t, &resp.Diagnostics, "Recovered the backup settings")

	got := rawToBackups(ctx, t, sch, resp.State.Raw)
	if got.Bucket.ValueString() != "fluxvale-kavita" {
		t.Errorf("bucket = %s, want the recovered row's", got.Bucket.ValueString())
	}
}

// PATCH 404: the row is gone (a panel delete won the race) — removed,
// never plan-over-ghost.
func TestBackupsUpdate404Removes(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadBackups(ctx, t)

	f := newFakeAPI(t)
	f.backupPatchStatus = http.StatusNotFound
	f.backupPatchDoc = `{"errors": [{"status": "404"}]}`
	r.pd = f.testClient()

	planData := testBackups(func(d *backupsModel) {
		d.ID = types.StringValue("bs-1")
		d.Enabled = types.BoolValue(true)
		d.RetentionKeep = types.Int64Value(7)
	})
	req := resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: sch, Raw: backupsToRaw(ctx, t, sch, &planData)},
		State: tfsdk.State{Schema: sch, Raw: backupsToRaw(ctx, t, sch, &planData)},
	}
	resp := &resource.UpdateResponse{
		State: tfsdk.State{Schema: sch, Raw: backupsToRaw(ctx, t, sch, &planData)},
	}
	r.Update(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update errored: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsNull() {
		t.Errorf("state after a 404 update = %v, want null (removed)", resp.State.Raw)
	}
}

// DELETE 404 is already-gone: idempotent destroy, clean diagnostics.
func TestBackupsDelete404IsSuccess(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadBackups(ctx, t)

	f := newFakeAPI(t)
	f.backupDeleteStatus = http.StatusNotFound
	r.pd = f.testClient()

	stateData := testBackups(func(d *backupsModel) { d.ID = types.StringValue("bs-9") })
	req := resource.DeleteRequest{
		State: tfsdk.State{Schema: sch, Raw: backupsToRaw(ctx, t, sch, &stateData)},
	}
	resp := &resource.DeleteResponse{}
	r.Delete(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Delete errored: %v — a 404 is already-gone", resp.Diagnostics)
	}
	findOne(t, f.all(), http.MethodDelete, "/api/v1/backup-settings/bs-9")
}
