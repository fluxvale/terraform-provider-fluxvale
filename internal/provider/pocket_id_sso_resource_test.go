package provider

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// newSchemadPocketIdSso builds the PocketID SSO resource with its
// schema.
func newSchemadPocketIdSso(ctx context.Context, t *testing.T) (*pocketIdSsoResource, schema.Schema) {
	t.Helper()
	r := NewPocketIdSsoResource().(*pocketIdSsoResource)
	schemaResp := &resource.SchemaResponse{}
	r.Schema(ctx, resource.SchemaRequest{}, schemaResp)
	if schemaResp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", schemaResp.Diagnostics)
	}
	return r, schemaResp.Schema
}

func pocketIdSsoToRaw(ctx context.Context, t *testing.T, sch schema.Schema, data *pocketIdSsoModel) tftypes.Value {
	t.Helper()
	st := tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)}
	if diags := st.Set(ctx, data); diags.HasError() {
		t.Fatalf("State.Set: %v", diags)
	}
	return st.Raw
}

func rawToPocketIdSso(ctx context.Context, t *testing.T, sch schema.Schema, raw tftypes.Value) pocketIdSsoModel {
	t.Helper()
	st := tfsdk.State{Schema: sch, Raw: raw}
	var data pocketIdSsoModel
	if diags := st.Get(ctx, &data); diags.HasError() {
		t.Fatalf("State.Get: %v", diags)
	}
	return data
}

// seededPocketIdSso is a tracked wiring: every wire field populated,
// the Terraform id riding the instance id.
func seededPocketIdSso(mutators ...func(*pocketIdSsoModel)) pocketIdSsoModel {
	return testPocketIdSso(append([]func(*pocketIdSsoModel){
		func(d *pocketIdSsoModel) {
			d.ID = types.StringValue("inst-1")
			d.PocketIDClientID = types.StringValue("client-1")
			d.PocketIDSecretID = types.StringValue("secret-1")
			d.WiredAt = types.StringValue("2026-11-12T10:00:00Z")
		},
	}, mutators...)...)
}

// runPocketIdSsoUpdate drives Update with the seeded response state and
// returns it for assertions.
func runPocketIdSsoUpdate(t *testing.T, r *pocketIdSsoResource, sch schema.Schema, planData, stateData pocketIdSsoModel) *resource.UpdateResponse {
	t.Helper()
	ctx := context.Background()
	resp := &resource.UpdateResponse{
		State: tfsdk.State{Schema: sch, Raw: pocketIdSsoToRaw(ctx, t, sch, &stateData)},
	}
	r.Update(ctx, resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: sch, Raw: pocketIdSsoToRaw(ctx, t, sch, &planData)},
		State: tfsdk.State{Schema: sch, Raw: pocketIdSsoToRaw(ctx, t, sch, &stateData)},
	}, resp)
	return resp
}

// Create over the wrapper: connect's request is the generic-action
// argument shape the platform pins (pocket_id_instance_id at the data
// level, NOT nested in data.attributes), the flat body maps into
// state, and the resource id is the INSTANCE id — never the wiring row
// id. Create does not read back: the flat body is the whole wiring.
func TestPocketIdSsoCreateConnectsAndIDsByInstance(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadPocketIdSso(ctx, t)

	f := newFakeAPI(t)
	r.pd = f.testClient()

	planData := testPocketIdSso()
	req := resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: pocketIdSsoToRaw(ctx, t, sch, &planData)},
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
	}
	r.Create(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Create: %v", resp.Diagnostics)
	}

	post := findOne(t, f.all(), http.MethodPost, "/api/v1/instances/inst-1/connect-pocketid")
	data, ok := post.body["data"].(map[string]any)
	if !ok {
		t.Fatalf("connect body has no data object: %+v", post.body)
	}
	if data["pocket_id_instance_id"] != "pid-1" {
		t.Errorf("pocket_id_instance_id = %v, want pid-1 at the data level", data["pocket_id_instance_id"])
	}
	if _, nested := data["attributes"]; nested {
		t.Errorf("connect body nested the action argument under attributes: %+v (the generic action takes it at the data level)", post.body)
	}

	got := rawToPocketIdSso(ctx, t, sch, resp.State.Raw)
	if got.ID.ValueString() != "inst-1" {
		t.Errorf("id = %q, want the instance id (the one-wiring key), not the wiring row id", got.ID.ValueString())
	}
	if got.InstanceID.ValueString() != "inst-1" || got.PocketIDInstanceID.ValueString() != "pid-1" {
		t.Errorf("instance_id/pocket_id_instance_id = %s/%s, want the wire's",
			got.InstanceID.ValueString(), got.PocketIDInstanceID.ValueString())
	}
	if got.PocketIDClientID.ValueString() != "client-1" || got.PocketIDSecretID.ValueString() != "secret-1" {
		t.Errorf("client/secret ids = %s/%s, want the flat body's",
			got.PocketIDClientID.ValueString(), got.PocketIDSecretID.ValueString())
	}
	if got.WiredAt.ValueString() != "2026-11-12T10:00:00Z" {
		t.Errorf("wired_at = %q, want the flat body's", got.WiredAt.ValueString())
	}
	if !got.RotateSecretOn.IsNull() {
		t.Error("rotate_secret_on in state = set, want null (the config left it unset)")
	}

	for _, req := range f.all() {
		if req.method == http.MethodGet {
			t.Errorf("unexpected GET %s — create maps the connect body, no read-back", req.path)
		}
	}
}

// A 400 from connect (busy rollout / flag off / custody) is the
// platform's own wording, surfaced verbatim — and state stays empty:
// no half-write over a refused wire.
func TestPocketIdSsoCreateSurfacesBusyVerbatim(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadPocketIdSso(ctx, t)

	f := newFakeAPI(t)
	f.pocketIDConnectStatus = http.StatusBadRequest
	f.pocketIDConnectDoc = `{"errors": [{"title": "InvalidArgument", "detail": "the instance is busy: deploying owns the workload"}]}`
	r.pd = f.testClient()

	planData := testPocketIdSso()
	req := resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: pocketIdSsoToRaw(ctx, t, sch, &planData)},
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
	}
	r.Create(ctx, req, resp)

	assertError(t, &resp.Diagnostics, "busy", "deploying")
	if !resp.State.Raw.IsNull() {
		t.Errorf("state = %v, want null (the connect was refused — nothing was wired)", resp.State.Raw)
	}
	reqs := f.all()
	if len(reqs) != 1 || reqs[0].method != http.MethodPost {
		t.Errorf("requests = %+v, want exactly the one refused POST", reqs)
	}
}

// A wire echo that names a different instance/PocketID is a platform
// bug — Create refuses with a diagnostic and writes no state (a
// half-written state would be an inconsistent-result failure).
func TestPocketIdSsoCreateRejectsHostileEcho(t *testing.T) {
	cases := []struct {
		name   string
		doc    string
		needle string
	}{
		{
			name:   "different instance",
			doc:    strings.ReplaceAll(pocketIDWiringFlat, `"instance_id": "inst-1"`, `"instance_id": "inst-other"`),
			needle: "different instance",
		},
		{
			name:   "different pocket id instance",
			doc:    strings.ReplaceAll(pocketIDWiringFlat, `"pocket_id_instance_id": "pid-1"`, `"pocket_id_instance_id": "pid-other"`),
			needle: "different PocketID instance",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r, sch := newSchemadPocketIdSso(ctx, t)

			f := newFakeAPI(t)
			f.pocketIDConnectDoc = tc.doc
			r.pd = f.testClient()

			planData := testPocketIdSso()
			req := resource.CreateRequest{
				Plan: tfsdk.Plan{Schema: sch, Raw: pocketIdSsoToRaw(ctx, t, sch, &planData)},
			}
			resp := &resource.CreateResponse{
				State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
			}
			r.Create(ctx, req, resp)

			assertError(t, &resp.Diagnostics, tc.needle, "Nothing was written to state")
			if !resp.State.Raw.IsNull() {
				t.Errorf("state = %v, want null (a hostile echo must not half-write state)", resp.State.Raw)
			}
		})
	}
}

// Ash's uuid cast lowercases on the wire: a case-fold echo of the
// configured identity keeps the CONFIGURED spelling in state — the
// next plan is clean, not a replacement.
func TestPocketIdSsoCreateCaseFoldKeepsConfiguredSpelling(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadPocketIdSso(ctx, t)

	f := newFakeAPI(t)
	f.pocketIDConnectDoc = pocketIDWiringFlat // lowercase echo
	r.pd = f.testClient()

	planData := testPocketIdSso(func(d *pocketIdSsoModel) {
		d.InstanceID = types.StringValue("INST-1")
		d.PocketIDInstanceID = types.StringValue("PID-1")
	})
	req := resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: pocketIdSsoToRaw(ctx, t, sch, &planData)},
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
	}
	r.Create(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Create: %v", resp.Diagnostics)
	}

	findOne(t, f.all(), http.MethodPost, "/api/v1/instances/INST-1/connect-pocketid")

	got := rawToPocketIdSso(ctx, t, sch, resp.State.Raw)
	if got.ID.ValueString() != "INST-1" || got.InstanceID.ValueString() != "INST-1" {
		t.Errorf("id/instance_id = %s/%s, want the configured spelling kept",
			got.ID.ValueString(), got.InstanceID.ValueString())
	}
	if got.PocketIDInstanceID.ValueString() != "PID-1" {
		t.Errorf("pocket_id_instance_id = %q, want the configured spelling kept", got.PocketIDInstanceID.ValueString())
	}
	for _, name := range []string{"instance_id", "pocket_id_instance_id"} {
		planValue := got.InstanceID
		if name == "pocket_id_instance_id" {
			planValue = got.PocketIDInstanceID
		}
		planResp := &planmodifier.StringResponse{PlanValue: planValue}
		sch.Attributes[name].(schema.StringAttribute).PlanModifiers[0].PlanModifyString(ctx, planmodifier.StringRequest{
			Path:       path.Root(name),
			PlanValue:  planValue,
			StateValue: planValue,
			State:      tfsdk.State{Schema: sch, Raw: resp.State.Raw},
			Plan:       tfsdk.Plan{Schema: sch, Raw: resp.State.Raw},
		}, planResp)
		if planResp.RequiresReplace {
			t.Errorf("%s: the case-fold state must not plan a replacement", name)
		}
	}
}

// Read maps the wiring's five fields. A case-fold echo names the same
// UUID (Ash's cast lowercases), so the STATE spelling wins — a
// lowercase-normalized echo must never plan a replacement, and the
// resource id stays the instance id even though the document's data.id
// is the wiring ROW id.
func TestPocketIdSsoReadCaseFoldKeepsConfiguredSpelling(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadPocketIdSso(ctx, t)

	f := newFakeAPI(t)
	f.pocketIDWiringGets = []scriptedResponse{script(200, pocketIDWiringDoc)} // lowercase echo
	r.pd = f.testClient()

	stateData := seededPocketIdSso(func(d *pocketIdSsoModel) {
		d.InstanceID = types.StringValue("INST-1")
		d.PocketIDInstanceID = types.StringValue("PID-1")
		d.ID = types.StringValue("INST-1")
		d.RotateSecretOn = types.StringValue("v1")
		d.PocketIDSecretID = types.StringValue("secret-old")
	})
	req := resource.ReadRequest{
		State: tfsdk.State{Schema: sch, Raw: pocketIdSsoToRaw(ctx, t, sch, &stateData)},
	}
	resp := &resource.ReadResponse{
		State: tfsdk.State{Schema: sch, Raw: pocketIdSsoToRaw(ctx, t, sch, &stateData)},
	}
	r.Read(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", resp.Diagnostics)
	}

	get := findOne(t, f.all(), http.MethodGet, "/api/v1/instances/INST-1/pocket-id-wiring")
	if len(get.query) != 0 {
		t.Errorf("read query = %v, want none (the path is the whole lookup)", get.query)
	}

	got := rawToPocketIdSso(ctx, t, sch, resp.State.Raw)
	if got.ID.ValueString() != "INST-1" || got.InstanceID.ValueString() != "INST-1" {
		t.Errorf("id/instance_id = %s/%s, want the state spelling kept (case-fold echo)",
			got.ID.ValueString(), got.InstanceID.ValueString())
	}
	if got.PocketIDInstanceID.ValueString() != "PID-1" {
		t.Errorf("pocket_id_instance_id = %q, want the state spelling kept (case-fold echo)", got.PocketIDInstanceID.ValueString())
	}
	if got.PocketIDClientID.ValueString() != "client-1" || got.PocketIDSecretID.ValueString() != "secret-1" {
		t.Errorf("client/secret ids = %s/%s, want the wire's",
			got.PocketIDClientID.ValueString(), got.PocketIDSecretID.ValueString())
	}
	if got.WiredAt.ValueString() != "2026-11-12T10:00:00Z" {
		t.Errorf("wired_at = %q, want the wire's", got.WiredAt.ValueString())
	}
	if got.RotateSecretOn.ValueString() != "v1" {
		t.Error("rotate_secret_on lost from state — the wire has no echo, the config value survives")
	}

	// The plan over this state is clean: identical configured spellings
	// never replace.
	for _, name := range []string{"instance_id", "pocket_id_instance_id"} {
		modifiers := sch.Attributes[name].(schema.StringAttribute).PlanModifiers
		planValue := got.InstanceID
		if name == "pocket_id_instance_id" {
			planValue = got.PocketIDInstanceID
		}
		planResp := &planmodifier.StringResponse{PlanValue: planValue}
		modifiers[0].PlanModifyString(ctx, planmodifier.StringRequest{
			Path:       path.Root(name),
			PlanValue:  planValue,
			StateValue: planValue,
			State:      tfsdk.State{Schema: sch, Raw: resp.State.Raw},
			Plan:       tfsdk.Plan{Schema: sch, Raw: resp.State.Raw},
		}, planResp)
		if planResp.RequiresReplace {
			t.Errorf("%s: the case-fold read state must not plan a replacement", name)
		}
	}
}

// A materially different echo is the wire's truth — an out-of-band
// re-wire — and Read is allowed to drift state so the next plan shows
// the replacement.
func TestPocketIdSsoReadMaterialEchoDriftsState(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadPocketIdSso(ctx, t)

	f := newFakeAPI(t)
	drifted := strings.ReplaceAll(pocketIDWiringDoc, `"instance_id": "inst-1"`, `"instance_id": "inst-other"`)
	drifted = strings.ReplaceAll(drifted, `"pocket_id_instance_id": "pid-1"`, `"pocket_id_instance_id": "pid-other"`)
	f.pocketIDWiringGets = []scriptedResponse{script(200, drifted)}
	r.pd = f.testClient()

	stateData := seededPocketIdSso()
	req := resource.ReadRequest{
		State: tfsdk.State{Schema: sch, Raw: pocketIdSsoToRaw(ctx, t, sch, &stateData)},
	}
	resp := &resource.ReadResponse{
		State: tfsdk.State{Schema: sch, Raw: pocketIdSsoToRaw(ctx, t, sch, &stateData)},
	}
	r.Read(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", resp.Diagnostics)
	}

	got := rawToPocketIdSso(ctx, t, sch, resp.State.Raw)
	if got.InstanceID.ValueString() != "inst-other" || got.ID.ValueString() != "inst-other" {
		t.Errorf("id/instance_id = %s/%s, want the wire's materially different identity",
			got.ID.ValueString(), got.InstanceID.ValueString())
	}
	if got.PocketIDInstanceID.ValueString() != "pid-other" {
		t.Errorf("pocket_id_instance_id = %q, want the wire's materially different identity", got.PocketIDInstanceID.ValueString())
	}
}

// A null/unknown/empty lookup key is invalid state, not a reason to
// forget the resource: no request is issued (an empty path 404 would
// remove a live wiring from state).
func TestPocketIdSsoReadGuardsEmptyKey(t *testing.T) {
	cases := []struct {
		name string
		key  types.String
	}{
		{"null", types.StringNull()},
		{"unknown", types.StringUnknown()},
		{"empty", types.StringValue("")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r, sch := newSchemadPocketIdSso(ctx, t)

			f := newFakeAPI(t)
			r.pd = f.testClient()

			stateData := seededPocketIdSso(func(d *pocketIdSsoModel) { d.InstanceID = tc.key })
			req := resource.ReadRequest{
				State: tfsdk.State{Schema: sch, Raw: pocketIdSsoToRaw(ctx, t, sch, &stateData)},
			}
			resp := &resource.ReadResponse{
				State: tfsdk.State{Schema: sch, Raw: pocketIdSsoToRaw(ctx, t, sch, &stateData)},
			}
			r.Read(ctx, req, resp)

			assertError(t, &resp.Diagnostics, "Invalid PocketID SSO state", "instance_id", "empty key")
			if reqs := f.all(); len(reqs) != 0 {
				t.Errorf("requests = %+v, want none — an empty lookup key must not reach the wire", reqs)
			}
		})
	}
}

// No wiring = gone: the read's 404 removes the resource from state —
// never plan-over-ghost.
func TestPocketIdSsoRead404Removes(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadPocketIdSso(ctx, t)

	f := newFakeAPI(t)
	f.pocketIDWiringGets = []scriptedResponse{script(http.StatusNotFound, `{"errors": [{"status": "404"}]}`)}
	r.pd = f.testClient()

	stateData := seededPocketIdSso()
	req := resource.ReadRequest{
		State: tfsdk.State{Schema: sch, Raw: pocketIdSsoToRaw(ctx, t, sch, &stateData)},
	}
	resp := &resource.ReadResponse{
		State: tfsdk.State{Schema: sch, Raw: pocketIdSsoToRaw(ctx, t, sch, &stateData)},
	}
	r.Read(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsNull() {
		t.Errorf("state after 404 = %v, want null (no wiring is gone)", resp.State.Raw)
	}
}

// A changed rotate_secret_on is the in-place rotation verb: one POST,
// the new secret id lands, the client id and the instance identity
// never move — no replacement, no connect.
func TestPocketIdSsoUpdateRotatesSecret(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadPocketIdSso(ctx, t)

	f := newFakeAPI(t)
	r.pd = f.testClient()

	stateData := seededPocketIdSso(func(d *pocketIdSsoModel) { d.RotateSecretOn = types.StringValue("v1") })
	// Core's shape when the trigger changes: computed fields may be
	// unknown in the plan; the rotate response settles them.
	planData := seededPocketIdSso(func(d *pocketIdSsoModel) {
		d.RotateSecretOn = types.StringValue("v2")
		d.ID = types.StringUnknown()
		d.PocketIDClientID = types.StringUnknown()
		d.PocketIDSecretID = types.StringUnknown()
		d.WiredAt = types.StringUnknown()
	})
	req := resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: sch, Raw: pocketIdSsoToRaw(ctx, t, sch, &planData)},
		State: tfsdk.State{Schema: sch, Raw: pocketIdSsoToRaw(ctx, t, sch, &stateData)},
	}
	resp := &resource.UpdateResponse{
		State: tfsdk.State{Schema: sch, Raw: pocketIdSsoToRaw(ctx, t, sch, &stateData)},
	}
	r.Update(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", resp.Diagnostics)
	}

	findOne(t, f.all(), http.MethodPost, "/api/v1/instances/inst-1/rotate-pocketid-secret")
	for _, req := range f.all() {
		if strings.HasSuffix(req.path, "/connect-pocketid") || req.method == http.MethodGet {
			t.Errorf("unexpected %s %s — an in-place rotation calls only the rotate verb", req.method, req.path)
		}
	}

	got := rawToPocketIdSso(ctx, t, sch, resp.State.Raw)
	if got.ID.ValueString() != "inst-1" {
		t.Errorf("id = %q, want the instance id unchanged by rotation", got.ID.ValueString())
	}
	if got.PocketIDSecretID.ValueString() != "secret-2" {
		t.Errorf("pocket_id_secret_id = %q, want the rotated id (secret-2)", got.PocketIDSecretID.ValueString())
	}
	if got.PocketIDClientID.ValueString() != "client-1" {
		t.Errorf("pocket_id_client_id = %q, want client-1 — rotation must not mint a new client", got.PocketIDClientID.ValueString())
	}
	if got.RotateSecretOn.ValueString() != "v2" {
		t.Errorf("rotate_secret_on = %q, want the plan's new trigger", got.RotateSecretOn.ValueString())
	}
}

// An equal trigger never rotates: the defensive update arm re-reads
// and settles state (nothing in place can change but the trigger).
func TestPocketIdSsoUpdateWithoutTriggerReReads(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadPocketIdSso(ctx, t)

	f := newFakeAPI(t)
	f.pocketIDWiringGets = []scriptedResponse{script(200, pocketIDWiringDoc)}
	r.pd = f.testClient()

	seeded := seededPocketIdSso(func(d *pocketIdSsoModel) { d.RotateSecretOn = types.StringValue("v1") })
	req := resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: sch, Raw: pocketIdSsoToRaw(ctx, t, sch, &seeded)},
		State: tfsdk.State{Schema: sch, Raw: pocketIdSsoToRaw(ctx, t, sch, &seeded)},
	}
	resp := &resource.UpdateResponse{
		State: tfsdk.State{Schema: sch, Raw: pocketIdSsoToRaw(ctx, t, sch, &seeded)},
	}
	r.Update(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", resp.Diagnostics)
	}

	for _, req := range f.all() {
		if req.method != http.MethodGet {
			t.Errorf("unexpected %s %s — an unchanged trigger must not rotate", req.method, req.path)
		}
	}
}

// A rotate 400 (the platform's not-yet-deployed / lost refusal) is
// surfaced verbatim; the prior state survives, not a half-rotated one.
func TestPocketIdSsoUpdateRotate400SurfacesVerbatim(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadPocketIdSso(ctx, t)

	f := newFakeAPI(t)
	f.pocketIDRotateStatus = http.StatusBadRequest
	f.pocketIDRotateDoc = `{"errors": [{"title": "InvalidArgument", "detail": "the sign-in wiring is not deployed right now"}]}`
	r.pd = f.testClient()

	stateData := seededPocketIdSso(func(d *pocketIdSsoModel) { d.RotateSecretOn = types.StringValue("v1") })
	planData := seededPocketIdSso(func(d *pocketIdSsoModel) { d.RotateSecretOn = types.StringValue("v2") })
	resp := runPocketIdSsoUpdate(t, r, sch, planData, stateData)

	assertError(t, &resp.Diagnostics, "not deployed right now")
	if resp.State.Raw.IsNull() {
		t.Fatal("state is null after a refused rotate — Update must preserve the prior state")
	}
	got := rawToPocketIdSso(ctx, t, sch, resp.State.Raw)
	if got.PocketIDSecretID.ValueString() != "secret-1" || got.RotateSecretOn.ValueString() != "v1" {
		t.Errorf("secret/trigger = %s/%s, want the prior state (secret-1/v1)",
			got.PocketIDSecretID.ValueString(), got.RotateSecretOn.ValueString())
	}
}

// A rotate 404 is a gone wiring: Update cannot remove state (the
// framework rejects a null state), so it names the refresh that will.
func TestPocketIdSsoUpdateRotate404SignalsRefresh(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadPocketIdSso(ctx, t)

	f := newFakeAPI(t)
	f.pocketIDRotateStatus = http.StatusNotFound
	f.pocketIDRotateDoc = `{"errors": [{"status": "404"}]}`
	r.pd = f.testClient()

	stateData := seededPocketIdSso(func(d *pocketIdSsoModel) { d.RotateSecretOn = types.StringValue("v1") })
	planData := seededPocketIdSso(func(d *pocketIdSsoModel) { d.RotateSecretOn = types.StringValue("v2") })
	resp := runPocketIdSsoUpdate(t, r, sch, planData, stateData)

	assertError(t, &resp.Diagnostics, "The PocketID sign-in wiring is gone", "refresh and apply again", "remove it from state")
	if resp.State.Raw.IsNull() {
		t.Fatal("state is null after a 404 rotate — Update must return a non-null state")
	}
	got := rawToPocketIdSso(ctx, t, sch, resp.State.Raw)
	if got.PocketIDSecretID.ValueString() != "secret-1" || got.ID.ValueString() != "inst-1" {
		t.Errorf("secret/id = %s/%s, want the prior state preserved",
			got.PocketIDSecretID.ValueString(), got.ID.ValueString())
	}
}

// The same 404 stance on Update's equal-trigger read arm.
func TestPocketIdSsoUpdateRead404SignalsRefresh(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadPocketIdSso(ctx, t)

	f := newFakeAPI(t)
	f.pocketIDWiringGets = []scriptedResponse{script(http.StatusNotFound, `{"errors": [{"status": "404"}]}`)}
	r.pd = f.testClient()

	seeded := seededPocketIdSso(func(d *pocketIdSsoModel) { d.RotateSecretOn = types.StringValue("v1") })
	resp := runPocketIdSsoUpdate(t, r, sch, seeded, seeded)

	assertError(t, &resp.Diagnostics, "The PocketID sign-in wiring is gone", "refresh and apply again")
	if resp.State.Raw.IsNull() {
		t.Fatal("state is null after a 404 read in Update — Update must return a non-null state")
	}
	if got := rawToPocketIdSso(ctx, t, sch, resp.State.Raw); got.PocketIDSecretID.ValueString() != "secret-1" {
		t.Errorf("secret = %q, want the prior state preserved", got.PocketIDSecretID.ValueString())
	}
}

// A rotate echo of a different pocket_id_instance_id is pinned to the
// plan's identity: the configured attribute never takes a wire spelling
// mid-Update (the next refresh decides whether it is real drift).
func TestPocketIdSsoUpdatePinsPocketIDInstanceEcho(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadPocketIdSso(ctx, t)

	f := newFakeAPI(t)
	f.pocketIDRotateDoc = strings.ReplaceAll(pocketIDRotatedFlat,
		`"pocket_id_instance_id": "pid-1"`, `"pocket_id_instance_id": "pid-2"`)
	r.pd = f.testClient()

	stateData := seededPocketIdSso(func(d *pocketIdSsoModel) { d.RotateSecretOn = types.StringValue("v1") })
	planData := seededPocketIdSso(func(d *pocketIdSsoModel) { d.RotateSecretOn = types.StringValue("v2") })
	resp := runPocketIdSsoUpdate(t, r, sch, planData, stateData)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update: %v — a differing PocketID echo must not fail the apply", resp.Diagnostics)
	}

	got := rawToPocketIdSso(ctx, t, sch, resp.State.Raw)
	if got.PocketIDInstanceID.ValueString() != "pid-1" {
		t.Errorf("pocket_id_instance_id = %q, want the plan's pid-1 pinned", got.PocketIDInstanceID.ValueString())
	}
	if got.PocketIDSecretID.ValueString() != "secret-2" {
		t.Errorf("pocket_id_secret_id = %q, want the rotate's secret-2", got.PocketIDSecretID.ValueString())
	}
}

// Delete is one idempotent disconnect: a live wiring deletes the
// client, the no-wiring arm answers client_deleted false, and a 404 is
// already-gone — all three are a success for Terraform.
func TestPocketIdSsoDeleteIsIdempotent(t *testing.T) {
	cases := []struct {
		name   string
		status int
		doc    string
	}{
		{"live wiring", http.StatusCreated, pocketIDDisconnectDoc},
		{"no wiring (client_deleted false)", http.StatusCreated, pocketIDDisconnectNoWiringDoc},
		{"already gone (404)", http.StatusNotFound, `{"errors": [{"status": "404"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r, sch := newSchemadPocketIdSso(ctx, t)

			f := newFakeAPI(t)
			f.pocketIDDisconnectStatus = tc.status
			f.pocketIDDisconnectDoc = tc.doc
			r.pd = f.testClient()

			stateData := seededPocketIdSso()
			req := resource.DeleteRequest{
				State: tfsdk.State{Schema: sch, Raw: pocketIdSsoToRaw(ctx, t, sch, &stateData)},
			}
			resp := &resource.DeleteResponse{
				State: tfsdk.State{Schema: sch, Raw: pocketIdSsoToRaw(ctx, t, sch, &stateData)},
			}
			r.Delete(ctx, req, resp)
			if resp.Diagnostics.HasError() {
				t.Fatalf("Delete: %v", resp.Diagnostics)
			}

			post := findOne(t, f.all(), http.MethodPost, "/api/v1/instances/inst-1/disconnect-pocketid")
			if len(post.body) != 0 {
				t.Errorf("disconnect body = %v, want none (the route takes no input)", post.body)
			}
			if !resp.State.Raw.IsNull() {
				t.Errorf("state after delete = %v, want null (removed)", resp.State.Raw)
			}
		})
	}

	t.Run("a real refusal surfaces and keeps state", func(t *testing.T) {
		ctx := context.Background()
		r, sch := newSchemadPocketIdSso(ctx, t)

		f := newFakeAPI(t)
		f.pocketIDDisconnectStatus = http.StatusBadRequest
		f.pocketIDDisconnectDoc = `{"errors": [{"detail": "the live environment is gone — redeploy to repair"}]}`
		r.pd = f.testClient()

		stateData := seededPocketIdSso()
		req := resource.DeleteRequest{
			State: tfsdk.State{Schema: sch, Raw: pocketIdSsoToRaw(ctx, t, sch, &stateData)},
		}
		resp := &resource.DeleteResponse{
			State: tfsdk.State{Schema: sch, Raw: pocketIdSsoToRaw(ctx, t, sch, &stateData)},
		}
		r.Delete(ctx, req, resp)

		assertError(t, &resp.Diagnostics, "live environment is gone", "redeploy")
		if resp.State.Raw.IsNull() {
			t.Error("state removed on a refused disconnect — the wiring still stands server-side")
		}
	})
}

// The schema/plan contract: the identity pair is Required +
// RequiresReplace (a different instance/PocketID is a different
// wiring); rotate_secret_on is Optional and carries NO plan modifier
// (it must never replace — replacement would lose the client id); the
// three wire fields are Computed; id is Computed and carries the
// instance id.
func TestPocketIdSsoSchemaAndReplacementContract(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadPocketIdSso(ctx, t)

	want := []string{
		"instance_id", "pocket_id_instance_id", "rotate_secret_on",
		"id", "pocket_id_client_id", "pocket_id_secret_id", "wired_at",
	}
	if len(sch.Attributes) != len(want) {
		t.Fatalf("schema attributes = %d, want exactly %d (%v)", len(sch.Attributes), len(want), want)
	}
	for _, name := range want {
		if _, ok := sch.Attributes[name]; !ok {
			t.Errorf("schema missing attribute %q", name)
		}
	}

	for _, name := range []string{"instance_id", "pocket_id_instance_id"} {
		attr := sch.Attributes[name].(schema.StringAttribute)
		if !attr.IsRequired() {
			t.Errorf("%s is not Required", name)
		}
		modifiers := attr.PlanModifiers
		if len(modifiers) != 1 {
			t.Fatalf("%s plan modifiers = %d, want exactly RequiresReplace", name, len(modifiers))
		}
		stateData := seededPocketIdSso()
		planData := seededPocketIdSso()
		resp := &planmodifier.StringResponse{PlanValue: types.StringValue("other")}
		modifiers[0].PlanModifyString(ctx, planmodifier.StringRequest{
			Path:       path.Root(name),
			PlanValue:  types.StringValue("other"),
			StateValue: types.StringValue("inst-1"),
			State:      tfsdk.State{Schema: sch, Raw: pocketIdSsoToRaw(ctx, t, sch, &stateData)},
			Plan:       tfsdk.Plan{Schema: sch, Raw: pocketIdSsoToRaw(ctx, t, sch, &planData)},
		}, resp)
		if !resp.RequiresReplace {
			t.Errorf("%s: a changed value must plan a replacement", name)
		}
	}

	rotate := sch.Attributes["rotate_secret_on"].(schema.StringAttribute)
	if !rotate.IsOptional() || rotate.IsRequired() || rotate.IsComputed() {
		t.Error("rotate_secret_on must be Optional-only (config posture, no wire echo)")
	}
	if len(rotate.PlanModifiers) != 0 {
		t.Errorf("rotate_secret_on plan modifiers = %v, want none — a rotation is Update, never replace", rotate.PlanModifiers)
	}

	for _, name := range []string{"id", "pocket_id_client_id", "pocket_id_secret_id", "wired_at"} {
		if !sch.Attributes[name].IsComputed() {
			t.Errorf("%s is not Computed", name)
		}
	}
	idModifiers := sch.Attributes["id"].(schema.StringAttribute).PlanModifiers
	if len(idModifiers) != 1 {
		t.Fatalf("id plan modifiers = %d, want exactly UseStateForUnknown", len(idModifiers))
	}
	const useStateDescription = "Once set, the value of this attribute in state will not change."
	if got := idModifiers[0].Description(ctx); got != useStateDescription {
		t.Errorf("id modifier description = %q, want the UseStateForUnknown description", got)
	}
	if got := fmt.Sprintf("%T", idModifiers[0]); !strings.Contains(got, "useStateForUnknown") {
		t.Errorf("id modifier type = %s, want a UseStateForUnknown modifier", got)
	}
	stateData := seededPocketIdSso()
	planData := seededPocketIdSso()
	keepResp := &planmodifier.StringResponse{PlanValue: types.StringUnknown()}
	idModifiers[0].PlanModifyString(ctx, planmodifier.StringRequest{
		Path:        path.Root("id"),
		ConfigValue: types.StringNull(),
		PlanValue:   types.StringUnknown(),
		StateValue:  types.StringValue("inst-1"),
		State:       tfsdk.State{Schema: sch, Raw: pocketIdSsoToRaw(ctx, t, sch, &stateData)},
		Plan:        tfsdk.Plan{Schema: sch, Raw: pocketIdSsoToRaw(ctx, t, sch, &planData)},
	}, keepResp)
	if keepResp.PlanValue.ValueString() != "inst-1" {
		t.Errorf("id plan = %q, want the prior state carried across an in-place update", keepResp.PlanValue.ValueString())
	}

	md := &resource.MetadataResponse{}
	r.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: "fluxvale"}, md)
	if md.TypeName != "fluxvale_pocket_id_sso" {
		t.Errorf("TypeName = %q, want fluxvale_pocket_id_sso", md.TypeName)
	}
}
