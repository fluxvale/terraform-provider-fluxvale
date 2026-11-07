package provider

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// newSchemadGateway builds the gateway resource with its schema.
func newSchemadGateway(ctx context.Context, t *testing.T) (*gatewayResource, schema.Schema) {
	t.Helper()
	r := NewGatewayResource().(*gatewayResource)
	schemaResp := &resource.SchemaResponse{}
	r.Schema(ctx, resource.SchemaRequest{}, schemaResp)
	if schemaResp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", schemaResp.Diagnostics)
	}
	return r, schemaResp.Schema
}

func gatewayToRaw(ctx context.Context, t *testing.T, sch schema.Schema, data *gatewayModel) tftypes.Value {
	t.Helper()
	st := tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)}
	if diags := st.Set(ctx, data); diags.HasError() {
		t.Fatalf("State.Set: %v", diags)
	}
	return st.Raw
}

func rawToGateway(ctx context.Context, t *testing.T, sch schema.Schema, raw tftypes.Value) gatewayModel {
	t.Helper()
	st := tfsdk.State{Schema: sch, Raw: raw}
	var data gatewayModel
	if diags := st.Get(ctx, &data); diags.HasError() {
		t.Fatalf("State.Get: %v", diags)
	}
	return data
}

func soleOrgDoc() string {
	return `{"data": [{"id": "org-1", "type": "organization", "attributes": {"slug": "acme", "name": "Acme"}}]}`
}

// assertWarning fails the test unless the diagnostics carry a warning
// whose summary+detail contain every needle.
func assertWarning(t *testing.T, diags *diag.Diagnostics, needles ...string) {
	t.Helper()
	joined := ""
	for _, d := range diags.Warnings() {
		joined += d.Summary() + " | " + d.Detail() + "\n"
	}
	if joined == "" {
		t.Fatalf("expected a warning containing %v, got none (diags: %+v)", needles, diags)
	}
	for _, needle := range needles {
		if !strings.Contains(joined, needle) {
			t.Fatalf("warnings = %q, want something containing %q", joined, needle)
		}
	}
}

// Create over the wrapper: the body grounds the route test's shape —
// tailnet_domain + auth_key (write-only) in attributes, the resolved
// org riding organization_id, and NO monthly_cents (a wire caller must
// not price their own gateway — the create pins the config figure).
// The final read lands fresh state.
func TestGatewayCreateBodyShapeAndState(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.gatewayGets = []scriptedResponse{script(200, gatewayProvisioningDoc)}
	r.pd = f.testClient()

	planData := testGateway()
	req := resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)},
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
	}
	r.Create(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Create: %v", resp.Diagnostics)
	}

	post := findOne(t, f.all(), http.MethodPost, "/api/v1/access-gateways")
	attrs := attrsOf(t, post)
	if attrs["tailnet_domain"] != "example.ts.net" || attrs["auth_key"] != "tskey-test" {
		t.Errorf("create attributes = %v, want tailnet_domain + auth_key", attrs)
	}
	if attrs["organization_id"] != "org-1" {
		t.Errorf("organization_id = %v, want the sole org resolved", attrs["organization_id"])
	}
	if _, ok := attrs["monthly_cents"]; ok {
		t.Error("monthly_cents sent at create — the create pins the platform's config price")
	}

	got := rawToGateway(ctx, t, sch, resp.State.Raw)
	if got.ID.ValueString() != "gw-1" || got.State.ValueString() != "provisioning" {
		t.Errorf("id/state = %s/%s, want gw-1/provisioning (the observer hasn't promoted yet — say so honestly)",
			got.ID.ValueString(), got.State.ValueString())
	}
	// gatewayCreateDoc (the POST response) omits provisioned_at; the
	// final read's doc carries it — null here is the read's null.
	if !got.ProvisionedAt.IsNull() {
		t.Errorf("provisioned_at = %s, want null", got.ProvisionedAt.ValueString())
	}
	if got.AuthKey.ValueString() != "tskey-test" {
		t.Errorf("auth_key in state = %q, want the configured value kept verbatim", got.AuthKey.ValueString())
	}
	// The import-convergence seed: config left organization unset, the
	// resolution's org lands in state.
	if got.Organization.ValueString() != "org-1" {
		t.Errorf("organization = %q, want the resolved org seeded", got.Organization.ValueString())
	}
	if got.MonthlyCents.ValueInt64() != 500 {
		t.Errorf("monthly_cents = %d, want the wire's 500", got.MonthlyCents.ValueInt64())
	}
}

// A fresh create with NO auth_key is refused before any HTTP call —
// the wire takes the key only at create and never echoes it, so there
// is no later save to supply it.
func TestGatewayCreateRejectsNullAuthKey(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	r.pd = f.testClient()

	planData := testGateway(func(d *gatewayModel) { d.AuthKey = types.StringNull() })
	req := resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)},
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
	}
	r.Create(ctx, req, resp)

	assertError(t, &resp.Diagnostics, "auth_key is required", "import")
	if len(f.all()) != 0 {
		t.Errorf("requests = %+v, want none — the refusal is client-side, the POST needs a key", f.all())
	}
	if !resp.State.Raw.IsNull() {
		t.Errorf("state = %v, want null (nothing created)", resp.State.Raw)
	}
}

// A POST the org's singleton refuses (unique_organization) is probed:
// a row whose create-only field matches the config is ADOPTED with a
// warning, and the resource lands tracked.
func TestGatewayCreateAdoptsExistingGateway(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.gatewayPostStatus = http.StatusConflict
	f.gatewayPostDoc = `{"errors": [{"detail": "has already been taken"}]}`
	f.gatewayGets = []scriptedResponse{script(200, gatewayActiveDoc)} // tailnet matches the config
	r.pd = f.testClient()

	planData := testGateway()
	req := resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)},
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
	}
	r.Create(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Create errored: %v", resp.Diagnostics)
	}
	assertWarning(t, &resp.Diagnostics, "Adopted the organization's existing gateway", "fv-0a1b2c3d")

	got := rawToGateway(ctx, t, sch, resp.State.Raw)
	if got.ID.ValueString() != "gw-1" || got.State.ValueString() != "active" {
		t.Errorf("id/state = %s/%s, want the adopted row tracked", got.ID.ValueString(), got.State.ValueString())
	}
	if got.AuthKey.ValueString() != "tskey-test" {
		t.Errorf("auth_key = %q, want the config value kept (unverifiable — the warning says so)", got.AuthKey.ValueString())
	}
}

// A diverging row is named — both tailnets and the panel opt-out — and
// nothing lands in state (adopting a different gateway would be a
// silent re-point).
func TestGatewayCreateNamesDivergentGateway(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.gatewayPostStatus = http.StatusConflict
	f.gatewayPostDoc = `{"errors": [{"detail": "has already been taken"}]}`
	other := strings.ReplaceAll(gatewayActiveDoc, `"example.ts.net"`, `"other-org.ts.net"`)
	f.gatewayGets = []scriptedResponse{script(200, other)}
	r.pd = f.testClient()

	planData := testGateway()
	req := resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)},
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
	}
	r.Create(ctx, req, resp)

	assertError(t, &resp.Diagnostics, "already has a different access gateway",
		"other-org.ts.net", "example.ts.net", "panel")
	if !resp.State.Raw.IsNull() {
		t.Errorf("state = %v, want null — a diverging gateway must not be adopted", resp.State.Raw)
	}
}

// No row behind the refusal ⇒ nothing to adopt: the POST's own error
// surfaces and state stays empty (the pre-POST class — nothing was
// created).
func TestGatewayCreateSurfacesOriginalErrorWhenNoRow(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.gatewayPostStatus = http.StatusForbidden
	f.gatewayPostDoc = `{"errors": [{"detail": "wallet has never paid"}]}`
	f.gatewayGets = []scriptedResponse{script(http.StatusNotFound, `{"errors": [{"status": "404"}]}`)}
	r.pd = f.testClient()

	planData := testGateway()
	req := resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)},
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
	}
	r.Create(ctx, req, resp)

	assertError(t, &resp.Diagnostics, "wallet has never paid")
	if !resp.State.Raw.IsNull() {
		t.Errorf("state = %v, want null (nothing was created)", resp.State.Raw)
	}
}

// Every post-POST failure leaves TAINTED TRACKED state — a stranded
// gateway must stay a resource the next apply updates or forgets.
func TestGatewayCreateTaintedStateOnWaitTimeout(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	// The observer never promotes.
	f.gatewayGets = []scriptedResponse{script(200, gatewayProvisioningDoc)}
	r.pd = f.testClient()
	r.pd.pollTimeout = 20 * time.Millisecond

	planData := testGateway(func(d *gatewayModel) { d.Instances = setInput(t, "inst-a") })
	req := resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)},
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
	}
	r.Create(ctx, req, resp)

	assertError(t, &resp.Diagnostics, "did not become active", "timed out")
	if resp.State.Raw.IsNull() {
		t.Fatal("state is null after a post-POST failure — the live gateway would be untracked")
	}
	got := rawToGateway(ctx, t, sch, resp.State.Raw)
	if got.ID.ValueString() != "gw-1" {
		t.Errorf("state id = %s, want the created gateway tracked", got.ID.ValueString())
	}
}

// Create with instances configured: the attach sequence waits the
// gateway to :active (create lands :provisioning; the observer is the
// only promoter) then POSTs one attachment per id — and the final read
// lands active state.
func TestGatewayCreateAttachesAfterActive(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.gatewayGets = []scriptedResponse{
		script(200, gatewayProvisioningDoc),
		script(200, gatewayActiveDoc),
	}
	r.pd = f.testClient()

	planData := testGateway(func(d *gatewayModel) {
		d.Instances = setInput(t, "inst-a", "inst-b")
	})
	req := resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)},
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
	}
	r.Create(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Create: %v", resp.Diagnostics)
	}

	reqs := f.all()

	// Order is the semantics: create → the active-wait GET(s) → the
	// attach POSTs → the final read GET. The first GET must come before
	// any attach — the wait is real, not skipped.
	firstGet := -1
	firstAttach := -1
	for i, req := range reqs {
		if req.method == http.MethodGet && strings.HasSuffix(req.path, "/access-gateway") && firstGet < 0 {
			firstGet = i
		}
		if req.method == http.MethodPost && strings.HasSuffix(req.path, "/attachments") && firstAttach < 0 {
			firstAttach = i
		}
	}
	if firstGet < 0 || firstAttach < 0 || firstAttach < firstGet {
		t.Fatalf("sequence wrong: first GET %d, first attach %d over %+v", firstGet, firstAttach, reqs)
	}

	// Both attachments, data-level instance_id (the generic action's shape).
	var seen []string
	for _, req := range reqs {
		if req.method != http.MethodPost || !strings.HasSuffix(req.path, "/attachments") {
			continue
		}
		data, ok := req.body["data"].(map[string]any)
		if !ok {
			t.Fatalf("attach body has no data object: %+v", req.body)
		}
		seen = append(seen, data["instance_id"].(string))
	}
	if len(seen) != 2 || seen[0] != "inst-a" || seen[1] != "inst-b" {
		t.Errorf("attachments = %v, want inst-a then inst-b at the data level", seen)
	}

	got := rawToGateway(ctx, t, sch, resp.State.Raw)
	if got.State.ValueString() != "active" || got.ProvisionedAt.IsNull() {
		t.Errorf("state/provisioned_at = %s/%v, want the final read's fresh active values",
			got.State.ValueString(), got.ProvisionedAt)
	}
}

// The instances diff: one call per changed id — attach the addition,
// detach the removal, never touch the unchanged.
func TestGatewayUpdateAttachmentDiff(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.gatewayGets = []scriptedResponse{script(200, gatewayActiveDoc)}
	r.pd = f.testClient()

	planData := testGateway(func(d *gatewayModel) {
		d.ID = types.StringValue("gw-1")
		d.Instances = setInput(t, "inst-a", "inst-c")
	})
	stateData := testGateway(func(d *gatewayModel) {
		d.ID = types.StringValue("gw-1")
		d.Instances = setInput(t, "inst-a", "inst-b")
	})
	req := resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)},
		State: tfsdk.State{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &stateData)},
	}
	resp := &resource.UpdateResponse{
		State: tfsdk.State{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)},
	}
	r.Update(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", resp.Diagnostics)
	}

	reqs := f.all()
	var attached, detached []string
	for _, req := range reqs {
		switch {
		case req.method == http.MethodPost && strings.HasSuffix(req.path, "/attachments"):
			attached = append(attached, req.body["data"].(map[string]any)["instance_id"].(string))
		case req.method == http.MethodDelete && strings.Contains(req.path, "/attachments/"):
			detached = append(detached, strings.TrimPrefix(req.path, "/api/v1/access-gateways/gw-1/attachments/"))
		}
	}
	if len(attached) != 1 || attached[0] != "inst-c" {
		t.Errorf("attached = %v, want exactly [inst-c]", attached)
	}
	if len(detached) != 1 || detached[0] != "inst-b" {
		t.Errorf("detached = %v, want exactly [inst-b]", detached)
	}
	for _, req := range reqs {
		if strings.Contains(req.path, "inst-a") {
			t.Error("inst-a is unchanged — the diff must not re-attach it")
		}
	}

	// State refreshes from the final read; the configured set survives.
	got := rawToGateway(ctx, t, sch, resp.State.Raw)
	if got.State.ValueString() != "active" {
		t.Errorf("state = %s, want the fresh read's active", got.State.ValueString())
	}
	if got.AuthKey.ValueString() != "tskey-test" || got.Instances.IsNull() {
		t.Errorf("auth_key/instances = %q/%v, want the configured values kept", got.AuthKey.ValueString(), got.Instances)
	}
}

// A detach that 404s is already-gone (the platform re-reads the
// instance first) — the diff completes clean.
func TestGatewayUpdateToleratesDetach404(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.gatewayGets = []scriptedResponse{script(200, gatewayActiveDoc)}
	f.detachStatus = http.StatusNotFound
	r.pd = f.testClient()

	planData := testGateway(func(d *gatewayModel) {
		d.ID = types.StringValue("gw-1")
		d.Instances = setInput(t, "inst-a")
	})
	stateData := testGateway(func(d *gatewayModel) {
		d.ID = types.StringValue("gw-1")
		d.Instances = setInput(t, "inst-a", "inst-vanished")
	})
	req := resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)},
		State: tfsdk.State{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &stateData)},
	}
	resp := &resource.UpdateResponse{
		State: tfsdk.State{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)},
	}
	r.Update(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update: %v — a detach 404 is already-gone", resp.Diagnostics)
	}
	findOne(t, f.all(), http.MethodDelete, "/api/v1/access-gateways/gw-1/attachments/inst-vanished")
}

// No instance change ⇒ no attach/detach calls (the reads still happen).
func TestGatewayUpdateNoDiffSendsNoAttachmentCalls(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.gatewayGets = []scriptedResponse{script(200, gatewayActiveDoc)}
	r.pd = f.testClient()

	planData := testGateway(func(d *gatewayModel) {
		d.ID = types.StringValue("gw-1")
		d.Instances = setInput(t, "inst-a")
	})
	req := resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)},
		State: tfsdk.State{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)},
	}
	resp := &resource.UpdateResponse{
		State: tfsdk.State{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)},
	}
	r.Update(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", resp.Diagnostics)
	}

	for _, req := range f.all() {
		if strings.Contains(req.path, "/attachments") {
			t.Errorf("unexpected %s %s with no set change", req.method, req.path)
		}
	}
}

// Update over a gateway deleted between refresh and apply removes it
// from state instead of diffing against a ghost.
func TestGatewayUpdateRemovesOn404(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.gatewayGets = []scriptedResponse{script(http.StatusNotFound, `{"errors": [{"status": "404"}]}`)}
	r.pd = f.testClient()

	planData := testGateway(func(d *gatewayModel) {
		d.ID = types.StringValue("gw-1")
		d.Instances = setInput(t, "inst-a")
	})
	req := resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)},
		State: tfsdk.State{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)},
	}
	resp := &resource.UpdateResponse{
		State: tfsdk.State{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)},
	}
	r.Update(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsNull() {
		t.Errorf("state after a 404 update = %v, want null (removed)", resp.State.Raw)
	}
}

// Read: the singleton GET resolved through the org (state org unset ⇒
// sole-org fallback, seeded back into state), wire fields mapped,
// write-only/config-only values kept — even a hostile auth_key key
// can't leak.
func TestGatewayReadMapsRowAndKeepsConfig(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	hostile := strings.ReplaceAll(gatewayActiveDoc, `"state": "active"`,
		`"state": "active", "auth_key": "hostile", "credential_secret_ref": "nope"`)
	f.gatewayGets = []scriptedResponse{script(200, hostile)}
	r.pd = f.testClient()

	stateData := testGateway(func(d *gatewayModel) {
		d.Instances = setInput(t, "inst-a")
	})
	req := resource.ReadRequest{
		State: tfsdk.State{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &stateData)},
	}
	resp := &resource.ReadResponse{
		State: tfsdk.State{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &stateData)},
	}
	r.Read(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", resp.Diagnostics)
	}

	get := findOne(t, f.all(), http.MethodGet, "/api/v1/organizations/org-1/access-gateway")
	_ = get

	got := rawToGateway(ctx, t, sch, resp.State.Raw)
	if got.Hostname.ValueString() != "fv-0a1b2c3d" || got.State.ValueString() != "active" {
		t.Errorf("hostname/state = %s/%s, want the wire's", got.Hostname.ValueString(), got.State.ValueString())
	}
	if got.OrganizationID.ValueString() != "org-1" {
		t.Errorf("organization_id = %s, want the wire's echo", got.OrganizationID.ValueString())
	}
	if got.Organization.ValueString() != "org-1" {
		t.Errorf("organization = %q, want the resolution's org seeded (config was null)", got.Organization.ValueString())
	}
	if got.AuthKey.ValueString() != "tskey-test" {
		t.Errorf("auth_key in state = %q, want the configured value kept", got.AuthKey.ValueString())
	}
	if got.Instances.IsNull() {
		t.Error("instances lost from state — the configured set is config-authoritative")
	}
}

// A 404 on the singleton read means the org's gateway is gone.
func TestGatewayRead404Removes(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.gatewayGets = []scriptedResponse{script(http.StatusNotFound, `{"errors": [{"status": "404"}]}`)}
	r.pd = f.testClient()

	stateData := testGateway()
	req := resource.ReadRequest{
		State: tfsdk.State{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &stateData)},
	}
	resp := &resource.ReadResponse{
		State: tfsdk.State{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &stateData)},
	}
	r.Read(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsNull() {
		t.Errorf("state after 404 = %v, want null (removed)", resp.State.Raw)
	}
}

// The honest delete: no gateway-delete route exists on the wire —
// zero HTTP calls, a warning diagnostic, and the resource leaves state.
func TestGatewayDeleteHonesty(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	r.pd = f.testClient()

	stateData := testGateway(func(d *gatewayModel) { d.ID = types.StringValue("gw-1") })
	req := resource.DeleteRequest{
		State: tfsdk.State{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &stateData)},
	}
	resp := &resource.DeleteResponse{
		State: tfsdk.State{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &stateData)},
	}
	r.Delete(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Delete errored: %v", resp.Diagnostics)
	}

	if len(f.all()) != 0 {
		t.Errorf("delete made HTTP calls: %+v — the API has no gateway-delete route", f.all())
	}
	assertWarning(t, &resp.Diagnostics, "kept by the platform")
	if !resp.State.Raw.IsNull() {
		t.Errorf("state after delete = %v, want null (removed)", resp.State.Raw)
	}
}

// Import by org id, then Read: the wire mirror lands, organization
// stays the import arg, and a matching config PLAN produces no
// replacement — the import converges instead of planning a doomed
// replace.
func TestGatewayImportConverges(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.gatewayGets = []scriptedResponse{script(200, gatewayActiveDoc)}
	r.pd = f.testClient()

	// Import → state carries organization = the import arg.
	impReq := resource.ImportStateRequest{ID: "org-1"}
	impResp := &resource.ImportStateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
	}
	r.ImportState(ctx, impReq, impResp)
	if impResp.Diagnostics.HasError() {
		t.Fatalf("ImportState: %v", impResp.Diagnostics)
	}

	// Read over the imported state fills the wire mirror.
	readReq := resource.ReadRequest{State: impResp.State}
	readResp := &resource.ReadResponse{State: impResp.State}
	r.Read(ctx, readReq, readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", readResp.Diagnostics)
	}

	imported := rawToGateway(ctx, t, sch, readResp.State.Raw)
	if imported.Organization.ValueString() != "org-1" || imported.ID.ValueString() != "gw-1" {
		t.Fatalf("imported state = %s/%s, want organization + id filled",
			imported.Organization.ValueString(), imported.ID.ValueString())
	}

	// The plan the converging config produces over this state: auth_key
	// and instances unset (imports manage attachments with auth_key
	// null — the wire never echoes it), every echoed field identical ⇒
	// ModifyPlan stays silent — no replacement, the import converges.
	planData := testGateway(func(d *gatewayModel) {
		d.AuthKey = types.StringNull()
		d.ID = imported.ID
		d.Organization = imported.Organization
		d.OrganizationID = imported.OrganizationID
		d.TailnetDomain = imported.TailnetDomain
		d.Hostname = imported.Hostname
		d.AccessProvider = imported.AccessProvider
		d.State = imported.State
		d.StatusMessage = imported.StatusMessage
		d.MonthlyCents = imported.MonthlyCents
		d.ProvisionedAt = imported.ProvisionedAt
	})
	mpReq := resource.ModifyPlanRequest{
		Plan:  tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)},
		State: tfsdk.State{Schema: sch, Raw: readResp.State.Raw},
	}
	mpResp := &resource.ModifyPlanResponse{Plan: tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)}}
	r.ModifyPlan(ctx, mpReq, mpResp)
	if mpResp.Diagnostics.HasError() {
		t.Fatalf("ModifyPlan rejected a converging plan: %v", mpResp.Diagnostics)
	}
}

// ModifyPlan rejects any planned replacement — the destroy half only
// forgets state (no API delete), so the create half is guaranteed to
// conflict with the row that keeps running. The error names the real
// path.
func TestGatewayModifyPlanRejectsReplacement(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t) // no HTTP — ModifyPlan is wire-free
	r.pd = f.testClient()

	stateData := testGateway(func(d *gatewayModel) { d.ID = types.StringValue("gw-1") })
	planData := testGateway(func(d *gatewayModel) {
		d.ID = types.StringValue("gw-1")
		d.TailnetDomain = types.StringValue("other.ts.net")
	})
	req := resource.ModifyPlanRequest{
		Plan:  tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)},
		State: tfsdk.State{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &stateData)},
	}
	resp := &resource.ModifyPlanResponse{Plan: tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)}}
	r.ModifyPlan(ctx, req, resp)

	assertError(t, &resp.Diagnostics, "rejected", "cannot succeed", "tailnet_domain", "panel", "-replace")
}

// Equal plan and state (and the create/destroy shapes, where there is
// nothing to compare) plan clean.
func TestGatewayModifyPlanAllowsNonReplacements(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	r.pd = f.testClient()

	same := testGateway(func(d *gatewayModel) { d.ID = types.StringValue("gw-1") })
	raw := gatewayToRaw(ctx, t, sch, &same)

	t.Run("identical plan and state", func(t *testing.T) {
		req := resource.ModifyPlanRequest{
			Plan:  tfsdk.Plan{Schema: sch, Raw: raw},
			State: tfsdk.State{Schema: sch, Raw: raw},
		}
		resp := &resource.ModifyPlanResponse{Plan: tfsdk.Plan{Schema: sch, Raw: raw}}
		r.ModifyPlan(ctx, req, resp)
		assertNoErrors(t, &resp.Diagnostics)
	})

	t.Run("create planning (null state) is skipped", func(t *testing.T) {
		req := resource.ModifyPlanRequest{
			Plan:  tfsdk.Plan{Schema: sch, Raw: raw},
			State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
		}
		resp := &resource.ModifyPlanResponse{Plan: tfsdk.Plan{Schema: sch, Raw: raw}}
		r.ModifyPlan(ctx, req, resp)
		assertNoErrors(t, &resp.Diagnostics)
	})

	t.Run("destroy planning (null plan) is skipped", func(t *testing.T) {
		req := resource.ModifyPlanRequest{
			Plan:  tfsdk.Plan{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
			State: tfsdk.State{Schema: sch, Raw: raw},
		}
		resp := &resource.ModifyPlanResponse{}
		r.ModifyPlan(ctx, req, resp)
		assertNoErrors(t, &resp.Diagnostics)
	})
}

// The wait fails fast on the ladder's suspension, naming the
// remediation (the provider deliberately does not expose resume).
func TestGatewayWaitSurfacesSuspended(t *testing.T) {
	f := newFakeAPI(t)
	suspended := strings.ReplaceAll(gatewayProvisioningDoc, `"state": "provisioning"`, `"state": "suspended"`)
	f.gatewayGets = []scriptedResponse{script(200, suspended)}

	pd := f.testClient()
	diags := &diag.Diagnostics{}
	waitGatewayActive(context.Background(), pd, "org-1", diags)
	assertError(t, diags, "did not become active", "suspended", "resume", "resume_gateway")
}

func TestGatewayWaitSurfacesDestroying(t *testing.T) {
	f := newFakeAPI(t)
	destroying := strings.ReplaceAll(gatewayProvisioningDoc, `"state": "provisioning"`, `"state": "destroying"`)
	f.gatewayGets = []scriptedResponse{script(200, destroying)}

	pd := f.testClient()
	diags := &diag.Diagnostics{}
	waitGatewayActive(context.Background(), pd, "org-1", diags)
	assertError(t, diags, "did not become active", "destroying", "torn down")
}

// The wait surfaces the observer's failure state with its message.
func TestGatewayWaitSurfacesNeedsAttention(t *testing.T) {
	f := newFakeAPI(t)
	needsAttention := strings.ReplaceAll(gatewayProvisioningDoc, `"state": "provisioning"`, `"state": "needs_attention"`)
	needsAttention = strings.ReplaceAll(needsAttention, `"status_message": null`, `"status_message": "Access setup failed"`)
	f.gatewayGets = []scriptedResponse{script(200, needsAttention)}

	pd := f.testClient()
	diags := &diag.Diagnostics{}
	waitGatewayActive(context.Background(), pd, "org-1", diags)
	assertError(t, diags, "did not become active", "needs_attention", "Access setup failed")
}

func TestGatewayWaitTimeout(t *testing.T) {
	f := newFakeAPI(t)
	f.gatewayGets = []scriptedResponse{script(200, gatewayProvisioningDoc)} // never activates

	pd := f.testClient()
	pd.pollTimeout = 20 * time.Millisecond
	diags := &diag.Diagnostics{}
	waitGatewayActive(context.Background(), pd, "org-1", diags)
	assertError(t, diags, "did not become active", "timed out", `"provisioning"`)
}

// The diff helper: deterministic order on both sides.
func TestInstanceSetDiff(t *testing.T) {
	added, removed := instanceSetDiff(
		[]string{"a", "b", "c"},
		[]string{"c", "d", "a"},
	)
	if strings.Join(added, ",") != "d" {
		t.Errorf("added = %v, want [d]", added)
	}
	if strings.Join(removed, ",") != "b" {
		t.Errorf("removed = %v, want [b]", removed)
	}

	added, removed = instanceSetDiff(nil, []string{"x"})
	if strings.Join(added, ",") != "x" || len(removed) != 0 {
		t.Errorf("from empty current: added = %v, removed = %v, want [x]/[]", added, removed)
	}

	added, removed = instanceSetDiff([]string{"x"}, nil)
	if len(added) != 0 || strings.Join(removed, ",") != "x" {
		t.Errorf("to empty configured: added = %v, removed = %v, want []/[x]", added, removed)
	}
}

// The schema contract: the identity trio replaces (create-only on the
// wire, with replacements rejected at plan time); auth_key is Optional
// (Required made every import plan a doomed replacement — the create
// carries the require-at-enable check instead); organization is
// Optional+Computed (the import-convergence seed); instances updates
// live via the pairing verbs; auth_key is sensitive.
func TestGatewaySchemaContract(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	want := []string{
		"organization", "tailnet_domain", "auth_key", "instances",
		"id", "organization_id", "hostname", "access_provider", "state",
		"status_message", "monthly_cents", "provisioned_at",
	}
	if len(sch.Attributes) != len(want) {
		t.Fatalf("schema attributes = %d, want exactly %d (%v)", len(sch.Attributes), len(want), want)
	}
	for _, name := range want {
		if _, ok := sch.Attributes[name]; !ok {
			t.Errorf("schema missing attribute %q", name)
		}
	}

	if !sch.Attributes["tailnet_domain"].IsRequired() {
		t.Error("tailnet_domain is not Required")
	}
	if sch.Attributes["auth_key"].IsRequired() {
		t.Error("auth_key must be Optional — Required made every import plan a doomed replacement")
	}
	if !sch.Attributes["auth_key"].IsOptional() || !sch.Attributes["organization"].IsOptional() {
		t.Error("auth_key/organization are not Optional")
	}
	if !sch.Attributes["organization"].IsComputed() {
		t.Error("organization is not Computed — imports cannot converge without the seed")
	}
	for _, name := range []string{"organization", "tailnet_domain", "auth_key"} {
		if len(sch.Attributes[name].(schema.StringAttribute).PlanModifiers) == 0 {
			t.Errorf("%s has no plan modifiers (want RequiresReplace — create-only on the wire)", name)
		}
	}
	if len(sch.Attributes["instances"].(schema.SetAttribute).PlanModifiers) != 0 {
		t.Error("instances must update in place (the attach/detach verbs)")
	}
	if !sch.Attributes["auth_key"].IsSensitive() {
		t.Error("auth_key is not sensitive")
	}

	md := &resource.MetadataResponse{}
	r.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: "fluxvale"}, md)
	if md.TypeName != "fluxvale_access_gateway" {
		t.Errorf("TypeName = %q, want fluxvale_access_gateway", md.TypeName)
	}
}
