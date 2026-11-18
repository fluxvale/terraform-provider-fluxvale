package provider

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fluxvale/terraform-provider-fluxvale/internal/client"
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

// A fresh create with NO credential at all is refused before any HTTP
// call — the wire takes exactly one of the three enrollment shapes
// (the OAuth client pair, api_token, auth_key) only at create and
// never echoes any of them, so there is no later save to supply one.
// The refusal names all three shapes — the pair first (preferred).
func TestGatewayCreateRequiresACredential(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	r.pd = f.testClient()

	planData := testGateway(func(d *gatewayModel) {
		d.AuthKey = types.StringNull()
		d.APIToken = types.StringNull()
		d.TSClientID = types.StringNull()
		d.TSClientSecret = types.StringNull()
	})
	req := resource.CreateRequest{
		Plan:   tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)},
		Config: gatewayConfigFor(ctx, t, sch, &planData),
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: nullObject(ctx, t, sch.Type().TerraformType(ctx))},
	}
	r.Create(ctx, req, resp)

	assertError(t, &resp.Diagnostics,
		"ts_client_id and ts_client_secret, api_token, or auth_key is required to enable a gateway",
		"ts_client_id and ts_client_secret, the preferred shape",
		"the platform mints the join key from it",
		"tailnet owner/admin account",
		"the legacy two-paste path",
		"import the existing gateway instead")
	if len(f.all()) != 0 {
		t.Errorf("requests = %+v, want none — the refusal is client-side, the POST needs a credential", f.all())
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

// The #836 mint path: no auth_key, the write-only token rides the
// CREATE POST (the platform mints and stores the join key from it), so
// the after-active token PATCH never runs — and neither credential
// value ever enters state.
func TestGatewayCreateMintsFromApiToken(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	// The wait for :active (provisioning, then active) plus the final
	// read — the mint path KEEPS the pre-attach wait; only the token
	// save's own wait/PATCH is skipped.
	f.gatewayGets = []scriptedResponse{
		script(200, gatewayProvisioningDoc),
		script(200, gatewayActiveDoc),
	}
	r.pd = f.testClient()

	planData := testGateway(func(d *gatewayModel) {
		d.AuthKey = types.StringNull()
		d.APIToken = types.StringValue("tskey-api-MINT-secret")
		d.RotateAPIToken = types.StringValue("v1")
		// An attachment forces the pre-attach wait — pinning that the
		// mint path did not gate it away (the hostile-review nit).
		d.Instances = setInput(t, "inst-a")
	})
	req := resource.CreateRequest{
		Plan:   tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)},
		Config: gatewayConfigFor(ctx, t, sch, &planData),
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: nullObject(ctx, t, sch.Type().TerraformType(ctx))},
	}
	r.Create(ctx, req, resp)
	assertNoErrors(t, &resp.Diagnostics)

	post := findOne(t, f.all(), http.MethodPost, "/api/v1/access-gateways")
	attrs := attrsOf(t, post)
	if attrs["api_token"] != "tskey-api-MINT-secret" {
		t.Errorf("create api_token = %v, want the configured token riding the POST", attrs["api_token"])
	}
	if attrs["auth_key"] != "" {
		t.Errorf("create auth_key = %v, want empty — the mint path sends no legacy key", attrs["auth_key"])
	}
	if hasRequest(f, http.MethodPatch, "/api/v1/access-gateways/gw-1/api-token") {
		t.Error("the mint path ran the after-active token PATCH — the platform stores the token at create")
	}

	// The pre-attach wait survives the mint path: the first singleton
	// GET (the wait's poll) precedes the first attach POST — the same
	// ordering pin TestGatewayCreateAttachesAfterActive carries, so a
	// future refactor gating the wait on !mint cannot pass silently.
	firstGet, firstAttach := -1, -1
	for i, req := range f.all() {
		if req.method == http.MethodGet && strings.HasSuffix(req.path, "/access-gateway") && firstGet < 0 {
			firstGet = i
		}
		if req.method == http.MethodPost && strings.HasSuffix(req.path, "/attachments") && firstAttach < 0 {
			firstAttach = i
		}
	}
	if firstGet < 0 || firstAttach < 0 || firstAttach < firstGet {
		t.Fatalf("mint sequence wrong: first GET %d, first attach %d — the wait before attach is part of the mint contract", firstGet, firstAttach)
	}

	got := rawToGateway(ctx, t, sch, resp.State.Raw)
	if !got.APIToken.IsNull() || !got.AuthKey.IsNull() {
		t.Errorf("api_token/auth_key in state = %v/%v, want null — no key material lands in state", got.APIToken, got.AuthKey)
	}
	if got.RotateAPIToken.ValueString() != "v1" {
		t.Errorf("rotate_api_token_on = %q, want the configured trigger restored in state", got.RotateAPIToken.ValueString())
	}
	if got.ID.ValueString() != "gw-1" {
		t.Errorf("id = %q, want the created gateway tracked", got.ID.ValueString())
	}
}

// The mint path's adoption: a refused POST over a matching singleton
// adopts with the mint warning — the platform holds the enrollment
// credential (minted from the create-time token), so the copy makes no
// "cannot be verified" auth_key claim. No token save runs either.
func TestGatewayCreateAdoptsMintedGateway(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.gatewayPostStatus = http.StatusConflict
	f.gatewayPostDoc = `{"errors": [{"detail": "has already been taken"}]}`
	f.gatewayGets = []scriptedResponse{script(200, gatewayActiveDoc)} // tailnet matches the config
	r.pd = f.testClient()

	planData := testGateway(func(d *gatewayModel) {
		d.AuthKey = types.StringNull()
		d.APIToken = types.StringValue("tskey-api-MINT-secret")
	})
	req := resource.CreateRequest{
		Plan:   tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)},
		Config: gatewayConfigFor(ctx, t, sch, &planData),
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: nullObject(ctx, t, sch.Type().TerraformType(ctx))},
	}
	r.Create(ctx, req, resp)
	assertNoErrors(t, &resp.Diagnostics)
	assertWarning(t, &resp.Diagnostics, "Adopted the organization's existing gateway", "Terraform now manages it")

	joined := ""
	for _, d := range resp.Diagnostics.Warnings() {
		joined += d.Summary() + " | " + d.Detail() + "\n"
	}
	if strings.Contains(joined, "cannot be verified") {
		t.Errorf("mint-path adoption warning claims an unverifiable auth_key: %q", joined)
	}
	if hasRequest(f, http.MethodPatch, "/api/v1/access-gateways/gw-1/api-token") {
		t.Error("the mint adoption saved the token out-of-band — the platform already holds it")
	}

	got := rawToGateway(ctx, t, sch, resp.State.Raw)
	if got.ID.ValueString() != "gw-1" || got.State.ValueString() != "active" {
		t.Errorf("id/state = %s/%s, want the adopted row tracked", got.ID.ValueString(), got.State.ValueString())
	}
}

// The API's 400 mint refusals surface verbatim — the pinned sentence
// rides the POST's errors body and lands in the diagnostic exactly as
// the platform pinned it; the provider maps nothing locally.
func TestGatewayCreateSurfacesMintRefusalVerbatim(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.gatewayPostStatus = http.StatusBadRequest
	f.gatewayPostDoc = `{"errors": [{"code": "invalid_argument",
		"detail": "` + gatewayRefusalMissingScope + `",
		"source": {"pointer": "/data/attributes/auth_key"}}]}`
	f.gatewayGets = []scriptedResponse{script(http.StatusNotFound, `{"errors": [{"status": "404"}]}`)}
	r.pd = f.testClient()

	planData := testGateway(func(d *gatewayModel) {
		d.AuthKey = types.StringNull()
		d.APIToken = types.StringValue("tskey-api-NO-SCOPE")
	})
	req := resource.CreateRequest{
		Plan:   tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)},
		Config: gatewayConfigFor(ctx, t, sch, &planData),
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: nullObject(ctx, t, sch.Type().TerraformType(ctx))},
	}
	r.Create(ctx, req, resp)

	assertError(t, &resp.Diagnostics, "Unable to create the access gateway", gatewayRefusalMissingScope)
	if !resp.State.Raw.IsNull() {
		t.Errorf("state = %v, want null — the 400 is a pre-create refusal, nothing was created", resp.State.Raw)
	}
}

// The fake enforces the #836 one-credential contract the way the
// platform does, and the client surfaces the pinned sentences verbatim:
// neither credential → 400 on auth_key, both → 400 on api_token, the
// pair arms per the lane-B sentences (mixed shapes on ts_client_id, a
// lone half on its sibling), and a complete pair alone → 201.
func TestGatewayCreateClientCredentialContract(t *testing.T) {
	ctx := context.Background()
	f := newFakeAPI(t)
	c := f.testClient().client

	_, err := c.CreateGateway(ctx, client.CreateGatewayInput{TailnetDomain: "example.ts.net"})
	if err == nil || !strings.Contains(err.Error(), gatewayRefusalNeitherCredential) {
		t.Errorf("err = %v, want the neither-credential sentence verbatim", err)
	}

	_, err = c.CreateGateway(ctx, client.CreateGatewayInput{
		TailnetDomain: "example.ts.net",
		AuthKey:       "tskey-test",
		ApiToken:      "tskey-api-secret",
	})
	if err == nil || !strings.Contains(err.Error(), gatewayRefusalBothCredentials) {
		t.Errorf("err = %v, want the both-credentials sentence verbatim", err)
	}

	// The complete pair beside either other credential: the
	// mixed-shape sentence, field-scoped on ts_client_id.
	for _, extra := range []struct {
		name string
		in   client.CreateGatewayInput
	}{
		{"api_token", client.CreateGatewayInput{
			TailnetDomain: "example.ts.net", ApiToken: "tskey-api-secret",
			TsClientID: "k1", TsClientSecret: "s1",
		}},
		{"auth_key", client.CreateGatewayInput{
			TailnetDomain: "example.ts.net", AuthKey: "tskey-test",
			TsClientID: "k1", TsClientSecret: "s1",
		}},
	} {
		_, err = c.CreateGateway(ctx, extra.in)
		if err == nil || !strings.Contains(err.Error(), gatewayRefusalMixedCredential) {
			t.Errorf("pair beside %s: err = %v, want the mixed-shape sentence verbatim", extra.name, err)
		}
	}

	// A lone pair half wants its sibling — the sentence rides the
	// sibling's field.
	_, err = c.CreateGateway(ctx, client.CreateGatewayInput{TailnetDomain: "example.ts.net", TsClientID: "k1"})
	if err == nil || !strings.Contains(err.Error(), gatewayRefusalPairHalf) {
		t.Errorf("err = %v, want the pair-half sentence verbatim", err)
	}
	_, err = c.CreateGateway(ctx, client.CreateGatewayInput{TailnetDomain: "example.ts.net", TsClientSecret: "s1"})
	if err == nil || !strings.Contains(err.Error(), gatewayRefusalPairHalf) {
		t.Errorf("err = %v, want the pair-half sentence verbatim", err)
	}

	// The pair alone is the accepted preferred shape: 201, no error.
	if _, err := c.CreateGateway(ctx, client.CreateGatewayInput{
		TailnetDomain: "example.ts.net", TsClientID: "k1", TsClientSecret: "s1",
	}); err != nil {
		t.Errorf("pair-only create: %v, want the 201", err)
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

// An auth_key change in either direction still plans a replacement,
// and replacements stay rejected — the write-only key cannot be
// verified or migrated, and the create half would conflict regardless
// of which credential the replacement carries.
func TestGatewayModifyPlanRejectsAuthKeyChange(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t) // no HTTP — ModifyPlan is wire-free
	r.pd = f.testClient()

	deployed := testGateway(func(d *gatewayModel) { d.ID = types.StringValue("gw-1") })
	imported := testGateway(func(d *gatewayModel) {
		d.ID = types.StringValue("gw-1")
		d.AuthKey = types.StringNull()
	})
	cases := []struct {
		name      string
		stateData gatewayModel
		planMut   func(d *gatewayModel)
	}{
		{"added over an imported null", imported, func(d *gatewayModel) { d.AuthKey = types.StringValue("tskey-new") }},
		{"changed", deployed, func(d *gatewayModel) { d.AuthKey = types.StringValue("tskey-other") }},
		{"removed", deployed, func(d *gatewayModel) { d.AuthKey = types.StringNull() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			planData := tc.stateData
			tc.planMut(&planData)
			req := resource.ModifyPlanRequest{
				Plan:  tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)},
				State: tfsdk.State{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &tc.stateData)},
			}
			resp := &resource.ModifyPlanResponse{Plan: tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)}}
			r.ModifyPlan(ctx, req, resp)
			assertError(t, &resp.Diagnostics, "rejected", "auth_key")
		})
	}
}

// The pair secret's presence change is rejected the same way: the
// write-only secret is null in every real plan and state (the
// framework nullifies it), so the comparison is a no-op in production
// — but a plan that ever does carry a different secret presence than
// state must plan the replacement, and the replacement stays
// rejected. (The in-process harness crafts the raw values, so the
// policy pin is exercisable.)
func TestGatewayModifyPlanRejectsSecretPresenceChange(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t) // no HTTP — ModifyPlan is wire-free
	r.pd = f.testClient()

	deployed := testGateway(func(d *gatewayModel) { d.ID = types.StringValue("gw-1") })
	cases := []struct {
		name     string
		planMut  func(d *gatewayModel)
		stateMut func(d *gatewayModel)
	}{
		{"added over an imported null", func(d *gatewayModel) { d.TSClientSecret = types.StringValue("tskey-client-NEW") }, nil},
		{"changed", func(d *gatewayModel) { d.TSClientSecret = types.StringValue("tskey-client-OTHER") }, nil},
		// The removed direction — the auth_key precedent's third case
		// (#19 review): a state that carried the secret against a plan
		// that drops it plans the same rejected replacement.
		{
			"removed",
			func(d *gatewayModel) { d.TSClientSecret = types.StringNull() },
			func(d *gatewayModel) { d.TSClientSecret = types.StringValue("tskey-client-OLD") },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The crafted plan differs from the state only in the
			// write-only secret's presence (the stateMut shapes the
			// carried-secret side; default state carries null).
			stateData := testGateway(func(d *gatewayModel) { d.ID = types.StringValue("gw-1") })
			if tc.stateMut != nil {
				tc.stateMut(&stateData)
			}

			planData := testGateway(func(d *gatewayModel) { d.ID = types.StringValue("gw-1") })
			tc.planMut(&planData)
			req := resource.ModifyPlanRequest{
				Plan:  tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)},
				State: tfsdk.State{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &stateData)},
			}
			resp := &resource.ModifyPlanResponse{Plan: tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)}}
			r.ModifyPlan(ctx, req, resp)
			assertError(t, &resp.Diagnostics, "rejected", "ts_client_secret")
		})
	}

	// The real-plan shape: the secret is null on BOTH sides (the
	// framework nullification) — no rejection, the rotation trigger
	// is the in-place path.
	planData := testGateway(func(d *gatewayModel) { d.ID = types.StringValue("gw-1") })
	req := resource.ModifyPlanRequest{
		Plan:  tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)},
		State: tfsdk.State{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &deployed)},
	}
	resp := &resource.ModifyPlanResponse{Plan: tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)}}
	r.ModifyPlan(ctx, req, resp)
	assertNoErrors(t, &resp.Diagnostics)
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

// The lane-B pair arm: no auth_key, no api_token — the OAuth client
// pair rides the CREATE POST (both halves; the platform exchanges it
// and stores the credential at create), so no after-active PATCH ever
// runs. The write-only secret never lands in state; the client id —
// public identity — does, verbatim, and the trigger is restored.
func TestGatewayCreateMintsFromClientPair(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	// The wait for :active (provisioning, then active) plus the final
	// read — the pair mint path KEEPS the pre-attach wait, like the
	// token mint path.
	f.gatewayGets = []scriptedResponse{
		script(200, gatewayProvisioningDoc),
		script(200, gatewayActiveDoc),
	}
	r.pd = f.testClient()

	planData := testGateway(func(d *gatewayModel) {
		d.AuthKey = types.StringNull()
		d.APIToken = types.StringNull()
		d.TSClientID = types.StringValue("k1234567890abcdef")
		d.TSClientSecret = types.StringValue("tskey-client-SECRET")
		d.RotateTSClientSecret = types.StringValue("v1")
		d.Instances = setInput(t, "inst-a")
	})
	req := resource.CreateRequest{
		Plan:   tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)},
		Config: gatewayConfigFor(ctx, t, sch, &planData),
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: nullObject(ctx, t, sch.Type().TerraformType(ctx))},
	}
	r.Create(ctx, req, resp)
	assertNoErrors(t, &resp.Diagnostics)

	post := findOne(t, f.all(), http.MethodPost, "/api/v1/access-gateways")
	attrs := attrsOf(t, post)
	if attrs["ts_client_id"] != "k1234567890abcdef" || attrs["ts_client_secret"] != "tskey-client-SECRET" {
		t.Errorf("create pair = %v, want both halves riding the POST", attrs)
	}
	if _, ok := attrs["api_token"]; ok {
		t.Errorf("create api_token = %v, want absent — the pair is the only credential", attrs["api_token"])
	}
	if attrs["auth_key"] != "" {
		t.Errorf("create auth_key = %v, want empty — the pair path sends no legacy key", attrs["auth_key"])
	}
	if hasRequest(f, http.MethodPatch, "/api/v1/access-gateways/gw-1/api-token") {
		t.Error("the pair path ran the after-active PATCH — the platform stores the pair at create")
	}

	got := rawToGateway(ctx, t, sch, resp.State.Raw)
	// No secret material in state; the client id IS in state (it is
	// not a secret — drift clarity is the point), pinned verbatim.
	if !got.TSClientSecret.IsNull() || !got.APIToken.IsNull() || !got.AuthKey.IsNull() {
		t.Errorf("secret material in state: ts_client_secret/api_token/auth_key = %v/%v/%v, want all null",
			got.TSClientSecret, got.APIToken, got.AuthKey)
	}
	if got.TSClientID.ValueString() != "k1234567890abcdef" {
		t.Errorf("ts_client_id in state = %q, want the configured id kept verbatim", got.TSClientID.ValueString())
	}
	if got.RotateTSClientSecret.ValueString() != "v1" {
		t.Errorf("rotate_ts_client_secret_on = %q, want the configured trigger restored in state",
			got.RotateTSClientSecret.ValueString())
	}
	if got.ID.ValueString() != "gw-1" {
		t.Errorf("id = %q, want the created gateway tracked", got.ID.ValueString())
	}
}

// Mixed credential shapes go to the wire and the API's field-scoped
// 400s surface verbatim — the provider maps nothing locally. Pair +
// api_token and pair + auth_key both hit the mixed-shape sentence; a
// lone pair half hits the sibling sentence.
func TestGatewayCreateSurfacesMixedPairRefusalsVerbatim(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.gatewayGets = []scriptedResponse{script(http.StatusNotFound, `{"errors": [{"status": "404"}]}`)}
	r.pd = f.testClient()

	cases := []struct {
		name    string
		mut     func(d *gatewayModel)
		needles []string
	}{
		{
			"pair beside api_token",
			func(d *gatewayModel) {
				d.AuthKey = types.StringNull()
				d.APIToken = types.StringValue("tskey-api-ALSO-SET")
				d.TSClientID = types.StringValue("k1234567890abcdef")
				d.TSClientSecret = types.StringValue("tskey-client-SECRET")
			},
			[]string{"Unable to create the access gateway", gatewayRefusalMixedCredential},
		},
		{
			"pair beside auth_key",
			func(d *gatewayModel) {
				d.AuthKey = types.StringValue("tskey-test")
				d.TSClientID = types.StringValue("k1234567890abcdef")
				d.TSClientSecret = types.StringValue("tskey-client-SECRET")
			},
			[]string{"Unable to create the access gateway", gatewayRefusalMixedCredential},
		},
		{
			"lone client id",
			func(d *gatewayModel) {
				d.AuthKey = types.StringNull()
				d.TSClientID = types.StringValue("k1234567890abcdef")
			},
			[]string{"Unable to create the access gateway", gatewayRefusalPairHalf},
		},
		{
			"lone client secret",
			func(d *gatewayModel) {
				d.AuthKey = types.StringNull()
				d.TSClientSecret = types.StringValue("tskey-client-SECRET")
			},
			[]string{"Unable to create the access gateway", gatewayRefusalPairHalf},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f.requests = nil
			planData := testGateway(tc.mut)
			req := resource.CreateRequest{
				Plan:   tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)},
				Config: gatewayConfigFor(ctx, t, sch, &planData),
			}
			resp := &resource.CreateResponse{
				State: tfsdk.State{Schema: sch, Raw: nullObject(ctx, t, sch.Type().TerraformType(ctx))},
			}
			r.Create(ctx, req, resp)
			assertError(t, &resp.Diagnostics, tc.needles...)
			if !resp.State.Raw.IsNull() {
				t.Errorf("state = %v, want null — the 400 is a pre-create refusal, nothing was created", resp.State.Raw)
			}
		})
	}
}

// The #19 review's masking case: the org ALREADY has a matching row
// (the normal state after this resource's state-only destroy), and the
// config carries a mixed shape. The 400 must surface verbatim — the
// adoption probe is for the singleton 409, and an adoption warning over
// a refused create would claim a credential was stored that never was.
func TestGatewayCreateMixedRefusalBeatsAdoption(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	// The row EXISTS: a matching singleton would be adoptable if the
	// POST had been refused for the identity conflict instead. The
	// fake's own contract logic 400s the mixed body — no scripted
	// status needed.
	f.gatewayGets = []scriptedResponse{script(200, gatewayActiveDoc)}
	r.pd = f.testClient()

	planData := testGateway(func(d *gatewayModel) {
		d.AuthKey = types.StringNull()
		d.APIToken = types.StringValue("tskey-api-ALSO-SET")
		d.TSClientID = types.StringValue("k1234567890abcdef")
		d.TSClientSecret = types.StringValue("tskey-client-SECRET")
	})
	req := resource.CreateRequest{
		Plan:   tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)},
		Config: gatewayConfigFor(ctx, t, sch, &planData),
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: nullObject(ctx, t, sch.Type().TerraformType(ctx))},
	}
	r.Create(ctx, req, resp)

	assertError(t, &resp.Diagnostics, "Unable to create the access gateway", gatewayRefusalMixedCredential)
	for _, d := range resp.Diagnostics {
		if strings.Contains(d.Detail(), "adopt") || strings.Contains(d.Summary(), "Adopted") {
			t.Errorf("adoption warning over a refused create: %v", d)
		}
	}
	if !resp.State.Raw.IsNull() {
		t.Errorf("state = %v, want null — the 400 is a pre-create refusal", resp.State.Raw)
	}
}

// The unknown write-only client secret is refused before the POST,
// like the token's posture: skipping an unknown silently would leave
// the gateway without its credential and no diff to retry.
func TestGatewayCreateUnknownClientSecretIsRefused(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	r.pd = f.testClient()

	planData := testGateway(func(d *gatewayModel) {
		d.AuthKey = types.StringNull()
		d.TSClientID = types.StringValue("k1234567890abcdef")
		d.TSClientSecret = types.StringUnknown()
		d.RotateTSClientSecret = types.StringValue("v1")
	})
	req := resource.CreateRequest{
		Plan:   tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)},
		Config: gatewayConfigFor(ctx, t, sch, &planData),
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: nullObject(ctx, t, sch.Type().TerraformType(ctx))},
	}
	r.Create(ctx, req, resp)
	assertError(t, &resp.Diagnostics, "ts_client_secret must be known at apply time")

	if hasRequest(f, http.MethodPost, "/api/v1/access-gateways") {
		t.Error("an unknown client secret still POSTed the gateway")
	}
}

// The schema contract: the identity trio replaces (create-only on the
// wire, with replacements rejected at plan time); auth_key is Optional
// (Required made every import plan a doomed replacement — the create
// carries the require-at-enable check instead); organization is
// Optional+Computed (the import-convergence seed); instances updates
// live via the pairing verbs; auth_key is sensitive; the client pair
// carries no plan modifiers (pair rotation is an in-place PATCH — a
// replacement cannot succeed and must never be planned).
func TestGatewaySchemaContract(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	want := []string{
		"organization", "tailnet_domain", "auth_key", "api_token", "rotate_api_token_on",
		"ts_client_id", "ts_client_secret", "rotate_ts_client_secret_on",
		"instances", "id", "organization_id", "hostname", "access_provider", "state",
		"status_message", "monthly_cents", "provisioned_at", "token_set_at", "claimed_domains",
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
	for _, name := range []string{"ts_client_id", "ts_client_secret", "rotate_ts_client_secret_on"} {
		if len(sch.Attributes[name].(schema.StringAttribute).PlanModifiers) != 0 {
			t.Errorf("%s has plan modifiers — pair rotation is the in-place PATCH, never a replacement", name)
		}
	}
	if len(sch.Attributes["instances"].(schema.SetAttribute).PlanModifiers) != 0 {
		t.Error("instances must update in place (the attach/detach verbs)")
	}
	if !sch.Attributes["auth_key"].IsSensitive() {
		t.Error("auth_key is not sensitive")
	}
	if sch.Attributes["ts_client_id"].IsSensitive() {
		t.Error("ts_client_id is sensitive — it is public identity, state-visible for drift clarity")
	}

	md := &resource.MetadataResponse{}
	r.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: "fluxvale"}, md)
	if md.TypeName != "fluxvale_access_gateway" {
		t.Errorf("TypeName = %q, want fluxvale_access_gateway", md.TypeName)
	}
}

// The seed must treat UNKNOWN as unset: a real CLI plans an
// Optional+Computed attribute the config leaves off as unknown, and an
// unknown value cannot ride state out of apply (the real-tofu gate
// caught exactly this — the in-process create tests craft nulls).
func TestGatewaySeedOrganizationUnknown(t *testing.T) {
	cases := []struct {
		name string
		in   types.String
		want string
	}{
		{"null seeds", types.StringNull(), "org-1"},
		{"unknown seeds", types.StringUnknown(), "org-1"},
		{"config value kept verbatim", types.StringValue("acme"), "acme"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := testGateway()
			data.Organization = tc.in
			seedOrganization(&data, "org-1")
			if data.Organization.ValueString() != tc.want {
				t.Errorf("organization = %q, want %q", data.Organization.ValueString(), tc.want)
			}
		})
	}
}
