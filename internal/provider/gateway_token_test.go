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
)

func gatewayConfigFor(ctx context.Context, t *testing.T, sch schema.Schema, data *gatewayModel) tfsdk.Config {
	t.Helper()
	return tfsdk.Config{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, data)}
}

// hasRequest reports whether the fake API saw a call, without the
// Fatalf findOne raises on a missing match — the negative assertions
// ("no PATCH") need this shape.
func hasRequest(f *fakeAPI, method, pathSuffix string) bool {
	for _, r := range f.all() {
		if r.method == method && strings.HasSuffix(r.path, pathSuffix) {
			return true
		}
	}
	return false
}

func TestGatewayTokenSchemaFlags(t *testing.T) {
	ctx := context.Background()
	_, sch := newSchemadGateway(ctx, t)

	token, ok := sch.Attributes["api_token"].(schema.StringAttribute)
	if !ok {
		t.Fatalf("api_token is %T, want schema.StringAttribute", sch.Attributes["api_token"])
	}
	if !token.Optional || token.Required || token.Computed {
		t.Errorf("api_token Optional/Required/Computed = %v/%v/%v, want optional input only",
			token.Optional, token.Required, token.Computed)
	}
	if !token.Sensitive || !token.WriteOnly {
		t.Errorf("api_token Sensitive/WriteOnly = %v/%v, want both (no state/plan value)",
			token.Sensitive, token.WriteOnly)
	}

	trigger, ok := sch.Attributes["rotate_api_token_on"].(schema.StringAttribute)
	if !ok {
		t.Fatalf("rotate_api_token_on is %T, want schema.StringAttribute", sch.Attributes["rotate_api_token_on"])
	}
	if !trigger.Optional || trigger.Sensitive || trigger.WriteOnly {
		t.Errorf("rotate_api_token_on Optional/Sensitive/WriteOnly = %v/%v/%v, want a plain optional trigger",
			trigger.Optional, trigger.Sensitive, trigger.WriteOnly)
	}

	// The lane-B pair (#836): the id is plain public identity; the
	// secret carries the api_token posture exactly (sensitive +
	// write-only); the trigger is a plain optional string.
	pairID, ok := sch.Attributes["ts_client_id"].(schema.StringAttribute)
	if !ok {
		t.Fatalf("ts_client_id is %T, want schema.StringAttribute", sch.Attributes["ts_client_id"])
	}
	if !pairID.Optional || pairID.Sensitive || pairID.WriteOnly {
		t.Errorf("ts_client_id Optional/Sensitive/WriteOnly = %v/%v/%v, want a plain optional input (public identity)",
			pairID.Optional, pairID.Sensitive, pairID.WriteOnly)
	}
	pairSecret, ok := sch.Attributes["ts_client_secret"].(schema.StringAttribute)
	if !ok {
		t.Fatalf("ts_client_secret is %T, want schema.StringAttribute", sch.Attributes["ts_client_secret"])
	}
	if !pairSecret.Optional || pairSecret.Required || pairSecret.Computed {
		t.Errorf("ts_client_secret Optional/Required/Computed = %v/%v/%v, want optional input only",
			pairSecret.Optional, pairSecret.Required, pairSecret.Computed)
	}
	if !pairSecret.Sensitive || !pairSecret.WriteOnly {
		t.Errorf("ts_client_secret Sensitive/WriteOnly = %v/%v, want both (no state/plan value)",
			pairSecret.Sensitive, pairSecret.WriteOnly)
	}
	pairTrigger, ok := sch.Attributes["rotate_ts_client_secret_on"].(schema.StringAttribute)
	if !ok {
		t.Fatalf("rotate_ts_client_secret_on is %T, want schema.StringAttribute", sch.Attributes["rotate_ts_client_secret_on"])
	}
	if !pairTrigger.Optional || pairTrigger.Sensitive || pairTrigger.WriteOnly {
		t.Errorf("rotate_ts_client_secret_on Optional/Sensitive/WriteOnly = %v/%v/%v, want a plain optional trigger",
			pairTrigger.Optional, pairTrigger.Sensitive, pairTrigger.WriteOnly)
	}

	stamp, ok := sch.Attributes["token_set_at"].(schema.StringAttribute)
	if !ok || !stamp.Computed {
		t.Error("token_set_at is not a computed string")
	}
	claimed, ok := sch.Attributes["claimed_domains"].(schema.ListAttribute)
	if !ok || !claimed.Computed {
		t.Error("claimed_domains is not a computed list")
	}
}

// A create with a write-only token sends it once, after the gateway
// serves, and never writes the value into state.
func TestGatewayCreatePatchesWriteOnlyToken(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.gatewayGets = []scriptedResponse{
		script(200, gatewayActiveDoc),    // waitGatewayActive
		script(200, gatewayTokenFullDoc), // singleton read after the PATCH
		script(200, gatewayTokenFullDoc), // final read
	}
	r.pd = f.testClient()

	planData := testGateway(func(d *gatewayModel) {
		d.APIToken = types.StringValue("tskey-api-TEST-secret")
		d.RotateAPIToken = types.StringValue("1")
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

	patch := findOne(t, f.all(), http.MethodPatch, "/api/v1/access-gateways/gw-1/api-token")
	if patch.path == "" {
		t.Fatal("no token PATCH on create")
	}
	attrs := attrsOf(t, patch)
	if attrs["api_token"] != "tskey-api-TEST-secret" {
		t.Errorf("PATCH api_token = %v, want the configured token", attrs["api_token"])
	}

	// Both credentials set keeps the legacy sequencing: the POST
	// carries auth_key only — the token never rides the create body
	// (the wire takes exactly one credential and would refuse both).
	post := findOne(t, f.all(), http.MethodPost, "/api/v1/access-gateways")
	if postAttrs := attrsOf(t, post); postAttrs["api_token"] != nil {
		t.Errorf("POST api_token = %v, want absent — a both-set create sends the token only via the after-active PATCH",
			postAttrs["api_token"])
	}

	var got gatewayModel
	if diags := resp.State.Get(ctx, &got); diags.HasError() {
		t.Fatalf("State.Get: %v", diags)
	}
	if !got.APIToken.IsNull() {
		t.Errorf("api_token in state = %q, want null (write-only)", got.APIToken.ValueString())
	}
	if got.TokenSetAt.ValueString() != "2026-11-15T10:00:00Z" {
		t.Errorf("token_set_at = %q, want the wire's stamp", got.TokenSetAt.ValueString())
	}
	var claimed []string
	if diags := got.ClaimedDomains.ElementsAs(ctx, &claimed, false); diags.HasError() {
		t.Fatalf("claimed_domains: %v", diags)
	}
	if len(claimed) != 1 || claimed[0] != "apps.customer.com" {
		t.Errorf("claimed_domains = %v, want [apps.customer.com]", claimed)
	}
}

func TestGatewayCreateWithoutTokenDoesNotPatch(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.gatewayGets = []scriptedResponse{script(200, gatewayProvisioningDoc)}
	r.pd = f.testClient()

	planData := testGateway()
	req := resource.CreateRequest{
		Plan:   tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)},
		Config: gatewayConfigFor(ctx, t, sch, &planData),
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: nullObject(ctx, t, sch.Type().TerraformType(ctx))},
	}
	r.Create(ctx, req, resp)
	assertNoErrors(t, &resp.Diagnostics)

	if hasRequest(f, http.MethodPatch, "/api/v1/access-gateways/gw-1/api-token") {
		t.Error("a create without api_token sent the token PATCH")
	}
}

// A changed rotation trigger re-sends the write-only token; an
// unchanged one never does, even though the value is in the config.
func TestGatewayUpdateRotationTrigger(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.gatewayGets = []scriptedResponse{
		script(200, gatewayActiveDoc),    // pre-read
		script(200, gatewayTokenFullDoc), // singleton read after the PATCH
		script(200, gatewayTokenFullDoc), // final read
	}
	r.pd = f.testClient()

	state := testGateway(func(d *gatewayModel) {
		d.ID = types.StringValue("gw-1")
		d.Organization = types.StringValue("org-1")
		d.OrganizationID = types.StringValue("org-1")
		d.Hostname = types.StringValue("fv-0a1b2c3d")
		d.AccessProvider = types.StringValue("tailscale")
		d.State = types.StringValue("active")
		d.MonthlyCents = types.Int64Value(500)
		d.RotateAPIToken = types.StringValue("1")
		d.Instances = types.SetNull(types.StringType)
	})

	plan := state
	plan.RotateAPIToken = types.StringValue("2")
	plan.APIToken = types.StringValue("tskey-api-ROTATED-secret")

	req := resource.UpdateRequest{
		Plan:   tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &plan)},
		State:  tfsdk.State{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &state)},
		Config: gatewayConfigFor(ctx, t, sch, &plan),
	}
	resp := &resource.UpdateResponse{
		State: tfsdk.State{Schema: sch, Raw: nullObject(ctx, t, sch.Type().TerraformType(ctx))},
	}
	r.Update(ctx, req, resp)
	assertNoErrors(t, &resp.Diagnostics)

	patch := findOne(t, f.all(), http.MethodPatch, "/api/v1/access-gateways/gw-1/api-token")
	if patch.path == "" {
		t.Fatal("changed rotate_api_token_on did not PATCH")
	}
	if attrs := attrsOf(t, patch); attrs["api_token"] != "tskey-api-ROTATED-secret" {
		t.Errorf("PATCH api_token = %v, want the rotated token", attrs["api_token"])
	}
}

func TestGatewayUpdateUnchangedTriggerDoesNotPatch(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.gatewayGets = []scriptedResponse{
		script(200, gatewayActiveDoc),
		script(200, gatewayActiveDoc),
	}
	r.pd = f.testClient()

	state := testGateway(func(d *gatewayModel) {
		d.ID = types.StringValue("gw-1")
		d.Organization = types.StringValue("org-1")
		d.OrganizationID = types.StringValue("org-1")
		d.Hostname = types.StringValue("fv-0a1b2c3d")
		d.AccessProvider = types.StringValue("tailscale")
		d.State = types.StringValue("active")
		d.MonthlyCents = types.Int64Value(500)
		d.RotateAPIToken = types.StringValue("1")
	})

	plan := state
	plan.APIToken = types.StringValue("tskey-api-SAME-secret")

	req := resource.UpdateRequest{
		Plan:   tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &plan)},
		State:  tfsdk.State{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &state)},
		Config: gatewayConfigFor(ctx, t, sch, &plan),
	}
	resp := &resource.UpdateResponse{
		State: tfsdk.State{Schema: sch, Raw: nullObject(ctx, t, sch.Type().TerraformType(ctx))},
	}
	r.Update(ctx, req, resp)
	assertNoErrors(t, &resp.Diagnostics)

	if hasRequest(f, http.MethodPatch, "/api/v1/access-gateways/gw-1/api-token") {
		t.Error("an unchanged rotation trigger re-sent the token")
	}
}

func TestGatewayUpdateRotationWithoutTokenIsRefused(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.gatewayGets = []scriptedResponse{script(200, gatewayActiveDoc)}
	r.pd = f.testClient()

	state := testGateway(func(d *gatewayModel) {
		d.ID = types.StringValue("gw-1")
		d.Organization = types.StringValue("org-1")
		d.OrganizationID = types.StringValue("org-1")
		d.Hostname = types.StringValue("fv-0a1b2c3d")
		d.AccessProvider = types.StringValue("tailscale")
		d.State = types.StringValue("active")
		d.MonthlyCents = types.Int64Value(500)
		d.RotateAPIToken = types.StringValue("1")
	})

	plan := state
	plan.RotateAPIToken = types.StringValue("2")

	req := resource.UpdateRequest{
		Plan:   tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &plan)},
		State:  tfsdk.State{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &state)},
		Config: gatewayConfigFor(ctx, t, sch, &plan),
	}
	resp := &resource.UpdateResponse{
		State: tfsdk.State{Schema: sch, Raw: nullObject(ctx, t, sch.Type().TerraformType(ctx))},
	}
	r.Update(ctx, req, resp)
	assertError(t, &resp.Diagnostics, "api_token is required to rotate")

	if hasRequest(f, http.MethodPatch, "/api/v1/access-gateways/gw-1/api-token") {
		t.Error("a rotation without a token still PATCHed")
	}
}

// A failed token save must not consume the rotation trigger: the early
// tracked state keeps it null, so the next apply sees a change and
// retries the write-only value. The gateway row stays tracked either
// way (the adoption stance).
func TestGatewayCreateTokenSaveFailureKeepsTriggerRetryable(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.gatewayGets = []scriptedResponse{script(200, gatewayActiveDoc)} // the wait
	f.gatewayTokenStatus = http.StatusInternalServerError
	f.gatewayTokenDoc = `{"errors": [{"detail": "boom"}]}`
	r.pd = f.testClient()

	planData := testGateway(func(d *gatewayModel) {
		d.APIToken = types.StringValue("tskey-api-RETRY-secret")
		d.RotateAPIToken = types.StringValue("1")
	})
	req := resource.CreateRequest{
		Plan:   tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)},
		Config: gatewayConfigFor(ctx, t, sch, &planData),
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: nullObject(ctx, t, sch.Type().TerraformType(ctx))},
	}
	r.Create(ctx, req, resp)
	assertError(t, &resp.Diagnostics, "Unable to save the Tailscale API token")

	var got gatewayModel
	if diags := resp.State.Get(ctx, &got); diags.HasError() {
		t.Fatalf("State.Get: %v", diags)
	}
	if !got.RotateAPIToken.IsNull() {
		t.Errorf("rotate_api_token_on in tracked state = %q, want null so the retry sees a change",
			got.RotateAPIToken.ValueString())
	}
}

// Removing the rotation trigger is not a rotation: it drops the
// declarative claim (a null trigger is documented as valid) instead of
// demanding a token that is no longer configured.
func TestGatewayUpdateRemovingTriggerDoesNotPatch(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.gatewayGets = []scriptedResponse{
		script(200, gatewayActiveDoc), // pre-read
		script(200, gatewayActiveDoc), // final read
	}
	r.pd = f.testClient()

	state := testGateway(func(d *gatewayModel) {
		d.ID = types.StringValue("gw-1")
		d.Organization = types.StringValue("org-1")
		d.OrganizationID = types.StringValue("org-1")
		d.Hostname = types.StringValue("fv-0a1b2c3d")
		d.AccessProvider = types.StringValue("tailscale")
		d.State = types.StringValue("active")
		d.MonthlyCents = types.Int64Value(500)
		d.RotateAPIToken = types.StringValue("1")
	})

	plan := state
	plan.RotateAPIToken = types.StringNull()

	req := resource.UpdateRequest{
		Plan:   tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &plan)},
		State:  tfsdk.State{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &state)},
		Config: gatewayConfigFor(ctx, t, sch, &plan),
	}
	resp := &resource.UpdateResponse{
		State: tfsdk.State{Schema: sch, Raw: nullObject(ctx, t, sch.Type().TerraformType(ctx))},
	}
	r.Update(ctx, req, resp)
	assertNoErrors(t, &resp.Diagnostics)

	if hasRequest(f, http.MethodPatch, "/api/v1/access-gateways/gw-1/api-token") {
		t.Error("removing rotate_api_token_on sent a token PATCH")
	}

	var got gatewayModel
	if diags := resp.State.Get(ctx, &got); diags.HasError() {
		t.Fatalf("State.Get: %v", diags)
	}
	if !got.RotateAPIToken.IsNull() {
		t.Errorf("rotate_api_token_on = %q, want the removed null", got.RotateAPIToken.ValueString())
	}
}

// An unknown write-only token cannot be skipped: create must refuse it
// rather than succeed without ever storing a token and leave no diff
// to retry.
func TestGatewayCreateUnknownTokenIsRefused(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	r.pd = f.testClient()

	planData := testGateway(func(d *gatewayModel) {
		d.APIToken = types.StringUnknown()
		d.RotateAPIToken = types.StringValue("1")
	})
	req := resource.CreateRequest{
		Plan:   tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &planData)},
		Config: gatewayConfigFor(ctx, t, sch, &planData),
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: nullObject(ctx, t, sch.Type().TerraformType(ctx))},
	}
	r.Create(ctx, req, resp)
	assertError(t, &resp.Diagnostics, "api_token must be known at apply time")

	if hasRequest(f, http.MethodPost, "/api/v1/access-gateways") {
		t.Error("an unknown token still POSTed the gateway")
	}
}

// The lane-B pair's rotation: a changed rotate_ts_client_secret_on
// re-sends BOTH halves in place through the credential PATCH — the
// client id from the plan, the current write-only secret from the
// config. An unchanged trigger never re-sends, and a changed trigger
// without the secret is refused before the PATCH (the write-only
// value cannot be read back).
func TestGatewayUpdateClientPairRotationTrigger(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.gatewayGets = []scriptedResponse{
		script(200, gatewayActiveDoc),    // pre-read
		script(200, gatewayTokenFullDoc), // singleton read after the PATCH
		script(200, gatewayTokenFullDoc), // final read
	}
	r.pd = f.testClient()

	state := testGateway(func(d *gatewayModel) {
		d.AuthKey = types.StringNull()
		d.ID = types.StringValue("gw-1")
		d.Organization = types.StringValue("org-1")
		d.OrganizationID = types.StringValue("org-1")
		d.Hostname = types.StringValue("fv-0a1b2c3d")
		d.AccessProvider = types.StringValue("tailscale")
		d.State = types.StringValue("active")
		d.MonthlyCents = types.Int64Value(500)
		d.TSClientID = types.StringValue("k1234567890abcdef")
		d.RotateTSClientSecret = types.StringValue("r1")
	})

	plan := state
	plan.RotateTSClientSecret = types.StringValue("r2")

	req := resource.UpdateRequest{
		Plan:   tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &plan)},
		State:  tfsdk.State{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &state)},
		Config: gatewayConfigFor(ctx, t, sch, &plan),
	}
	// The write-only secret rides the config only.
	req.Config.Raw = gatewayToRaw(ctx, t, sch, func() *gatewayModel {
		d := plan
		d.TSClientSecret = types.StringValue("tskey-client-ROTATED")
		return &d
	}())

	resp := &resource.UpdateResponse{
		State: tfsdk.State{Schema: sch, Raw: nullObject(ctx, t, sch.Type().TerraformType(ctx))},
	}
	r.Update(ctx, req, resp)
	assertNoErrors(t, &resp.Diagnostics)

	patch := findOne(t, f.all(), http.MethodPatch, "/api/v1/access-gateways/gw-1/api-token")
	attrs := attrsOf(t, patch)
	if attrs["ts_client_id"] != "k1234567890abcdef" || attrs["ts_client_secret"] != "tskey-client-ROTATED" {
		t.Errorf("PATCH pair = %v, want both halves re-sent in place", attrs)
	}
	if _, ok := attrs["api_token"]; ok {
		t.Errorf("PATCH api_token = %v, want absent — one shape per save", attrs["api_token"])
	}

	// The write-only secret never lands in state.
	got := rawToGateway(ctx, t, sch, resp.State.Raw)
	if !got.TSClientSecret.IsNull() {
		t.Errorf("ts_client_secret in state = %q, want null (write-only)", got.TSClientSecret.ValueString())
	}
	if got.RotateTSClientSecret.ValueString() != "r2" {
		t.Errorf("rotate_ts_client_secret_on = %q, want the changed trigger", got.RotateTSClientSecret.ValueString())
	}
}

func TestGatewayUpdateClientPairUnchangedTriggerDoesNotPatch(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.gatewayGets = []scriptedResponse{
		script(200, gatewayActiveDoc),
		script(200, gatewayActiveDoc),
	}
	r.pd = f.testClient()

	state := testGateway(func(d *gatewayModel) {
		d.AuthKey = types.StringNull()
		d.ID = types.StringValue("gw-1")
		d.Organization = types.StringValue("org-1")
		d.OrganizationID = types.StringValue("org-1")
		d.Hostname = types.StringValue("fv-0a1b2c3d")
		d.AccessProvider = types.StringValue("tailscale")
		d.State = types.StringValue("active")
		d.MonthlyCents = types.Int64Value(500)
		d.TSClientID = types.StringValue("k1234567890abcdef")
		d.RotateTSClientSecret = types.StringValue("r1")
	})

	plan := state
	req := resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &plan)},
		State: tfsdk.State{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &state)},
		Config: gatewayConfigFor(ctx, t, sch, func() *gatewayModel {
			d := plan
			d.TSClientSecret = types.StringValue("tskey-client-SAME")
			return &d
		}()),
	}
	resp := &resource.UpdateResponse{
		State: tfsdk.State{Schema: sch, Raw: nullObject(ctx, t, sch.Type().TerraformType(ctx))},
	}
	r.Update(ctx, req, resp)
	assertNoErrors(t, &resp.Diagnostics)

	if hasRequest(f, http.MethodPatch, "/api/v1/access-gateways/gw-1/api-token") {
		t.Error("an unchanged pair rotation trigger re-sent the secret")
	}
}

func TestGatewayUpdateClientPairRotationWithoutSecretIsRefused(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.gatewayGets = []scriptedResponse{script(200, gatewayActiveDoc)}
	r.pd = f.testClient()

	state := testGateway(func(d *gatewayModel) {
		d.AuthKey = types.StringNull()
		d.ID = types.StringValue("gw-1")
		d.Organization = types.StringValue("org-1")
		d.OrganizationID = types.StringValue("org-1")
		d.Hostname = types.StringValue("fv-0a1b2c3d")
		d.AccessProvider = types.StringValue("tailscale")
		d.State = types.StringValue("active")
		d.MonthlyCents = types.Int64Value(500)
		d.TSClientID = types.StringValue("k1234567890abcdef")
		d.RotateTSClientSecret = types.StringValue("r1")
	})

	plan := state
	plan.RotateTSClientSecret = types.StringValue("r2")

	req := resource.UpdateRequest{
		Plan:   tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &plan)},
		State:  tfsdk.State{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &state)},
		Config: gatewayConfigFor(ctx, t, sch, &plan),
	}
	resp := &resource.UpdateResponse{
		State: tfsdk.State{Schema: sch, Raw: nullObject(ctx, t, sch.Type().TerraformType(ctx))},
	}
	r.Update(ctx, req, resp)
	assertError(t, &resp.Diagnostics, "ts_client_secret is required to rotate")

	if hasRequest(f, http.MethodPatch, "/api/v1/access-gateways/gw-1/api-token") {
		t.Error("a pair rotation without the secret still PATCHed")
	}
}

// A rotation whose config carries no client id falls back to the
// state's id — "re-send the current pair" means the stored identity
// with the fresh secret; both halves ride the PATCH.
func TestGatewayUpdateClientPairRotationUsesStateIDFallback(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadGateway(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.gatewayGets = []scriptedResponse{
		script(200, gatewayActiveDoc),    // pre-read
		script(200, gatewayTokenFullDoc), // singleton read after the PATCH
		script(200, gatewayTokenFullDoc), // final read
	}
	r.pd = f.testClient()

	state := testGateway(func(d *gatewayModel) {
		d.AuthKey = types.StringNull()
		d.ID = types.StringValue("gw-1")
		d.Organization = types.StringValue("org-1")
		d.OrganizationID = types.StringValue("org-1")
		d.Hostname = types.StringValue("fv-0a1b2c3d")
		d.AccessProvider = types.StringValue("tailscale")
		d.State = types.StringValue("active")
		d.MonthlyCents = types.Int64Value(500)
		d.TSClientID = types.StringValue("k1234567890abcdef")
		d.RotateTSClientSecret = types.StringValue("r1")
	})

	plan := state
	plan.TSClientID = types.StringNull() // the config left the id off
	plan.RotateTSClientSecret = types.StringValue("r2")

	req := resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &plan)},
		State: tfsdk.State{Schema: sch, Raw: gatewayToRaw(ctx, t, sch, &state)},
		Config: gatewayConfigFor(ctx, t, sch, func() *gatewayModel {
			d := plan
			d.TSClientSecret = types.StringValue("tskey-client-ROTATED")
			return &d
		}()),
	}
	resp := &resource.UpdateResponse{
		State: tfsdk.State{Schema: sch, Raw: nullObject(ctx, t, sch.Type().TerraformType(ctx))},
	}
	r.Update(ctx, req, resp)
	assertNoErrors(t, &resp.Diagnostics)

	if attrs := attrsOf(t, findOne(t, f.all(), http.MethodPatch, "/api/v1/access-gateways/gw-1/api-token")); attrs["ts_client_id"] != "k1234567890abcdef" {
		t.Errorf("PATCH ts_client_id = %v, want the state's id riding the rotation", attrs["ts_client_id"])
	}
}
