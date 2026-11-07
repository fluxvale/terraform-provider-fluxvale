package provider

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// newSchemadDomain builds the custom-domain resource with its schema.
func newSchemadDomain(ctx context.Context, t *testing.T) (*customDomainResource, schema.Schema) {
	t.Helper()
	r := NewCustomDomainResource().(*customDomainResource)
	schemaResp := &resource.SchemaResponse{}
	r.Schema(ctx, resource.SchemaRequest{}, schemaResp)
	if schemaResp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", schemaResp.Diagnostics)
	}
	return r, schemaResp.Schema
}

func domainToRaw(ctx context.Context, t *testing.T, sch schema.Schema, data *customDomainModel) tftypes.Value {
	t.Helper()
	st := tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)}
	if diags := st.Set(ctx, data); diags.HasError() {
		t.Fatalf("State.Set: %v", diags)
	}
	return st.Raw
}

func rawToDomain(ctx context.Context, t *testing.T, sch schema.Schema, raw tftypes.Value) customDomainModel {
	t.Helper()
	st := tfsdk.State{Schema: sch, Raw: raw}
	var data customDomainModel
	if diags := st.Get(ctx, &data); diags.HasError() {
		t.Fatalf("State.Get: %v", diags)
	}
	return data
}

// Create over the wrapper: the body grounds the route test's shape —
// domain (the config name; canonicalization is the server's) plus the
// resolved org riding organization_id — and the default create (no
// wait) returns at `pending` with every computed field mapped from the
// wire, including the import-convergence seed.
func TestCustomDomainCreateBodyShapeAndPendingState(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadDomain(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.domainGets = []scriptedResponse{script(200, domainPendingDoc)}
	r.pd = f.testClient()

	planData := testDomain()
	req := resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: domainToRaw(ctx, t, sch, &planData)},
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
	}
	r.Create(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Create: %v", resp.Diagnostics)
	}

	post := findOne(t, f.all(), http.MethodPost, "/api/v1/access-domains")
	attrs := attrsOf(t, post)
	if attrs["domain"] != "apps.customer.com" {
		t.Errorf("domain = %v, want the config name (canonicalization is the server's)", attrs["domain"])
	}
	if attrs["organization_id"] != "org-1" {
		t.Errorf("organization_id = %v, want the sole org resolved", attrs["organization_id"])
	}

	got := rawToDomain(ctx, t, sch, resp.State.Raw)
	if got.ID.ValueString() != "dom-1" || got.State.ValueString() != "pending" {
		t.Errorf("id/state = %s/%s, want dom-1/pending (the default create returns at pending — the janitor verifies)",
			got.ID.ValueString(), got.State.ValueString())
	}
	if got.Kind.ValueString() != "custom" || got.OrganizationID.ValueString() != "org-1" {
		t.Errorf("kind/organization_id = %s/%s, want the wire's echo",
			got.Kind.ValueString(), got.OrganizationID.ValueString())
	}
	if got.Organization.ValueString() != "org-1" {
		t.Errorf("organization = %q, want the resolved org seeded", got.Organization.ValueString())
	}
	if !got.WaitForVerification.IsNull() {
		t.Error("wait_for_verification in state = set, want null (the config had it unset)")
	}
}

// wait_for_verification = true: create polls until the observer
// promotes pending → verified across polls, then the final read lands.
func TestCustomDomainCreateWaitVerified(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadDomain(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.domainGets = []scriptedResponse{
		script(200, domainPendingDoc),
		script(200, domainVerifiedDoc),
	}
	r.pd = f.testClient()

	planData := testDomain(func(d *customDomainModel) { d.WaitForVerification = types.BoolValue(true) })
	req := resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: domainToRaw(ctx, t, sch, &planData)},
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
	}
	r.Create(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Create: %v", resp.Diagnostics)
	}

	// The wait really polled: post→pending→verified→final-read, three
	// id GETs after the POST.
	gets := 0
	postSeen := false
	for _, req := range f.all() {
		switch {
		case req.method == http.MethodPost && strings.HasSuffix(req.path, "/access-domains"):
			postSeen = true
		case req.method == http.MethodGet && strings.Contains(req.path, "/access-domains/dom-1"):
			if !postSeen {
				t.Fatal("a domain GET ran before the register POST")
			}
			gets++
		}
	}
	if gets < 3 {
		t.Errorf("domain GETs after the POST = %d, want ≥ 3 (two polls + the final read)", gets)
	}

	got := rawToDomain(ctx, t, sch, resp.State.Raw)
	if got.State.ValueString() != "verified" {
		t.Errorf("state = %s, want verified (the wait saw the promotion)", got.State.ValueString())
	}
	if !got.WaitForVerification.ValueBool() {
		t.Error("wait_for_verification lost from state — the config value is kept verbatim")
	}
}

// The failed arm: the observer failed the row PAST its issuance window
// (pending with no Ready Certificate when the window lapses) — the
// wait fails fast with the platform's story (terminal: remove and
// re-register) and the row stays TAINTED TRACKED.
func TestCustomDomainCreateWaitFailed(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadDomain(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.domainGets = []scriptedResponse{
		script(200, domainPendingDoc),
		script(200, strings.ReplaceAll(domainPendingDoc, `"state": "pending"`, `"state": "failed"`)),
	}
	r.pd = f.testClient()

	planData := testDomain(func(d *customDomainModel) { d.WaitForVerification = types.BoolValue(true) })
	req := resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: domainToRaw(ctx, t, sch, &planData)},
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
	}
	r.Create(ctx, req, resp)

	assertError(t, &resp.Diagnostics, "did not verify", "failed", "re-register")
	// The row exists server-side (Certificate minted in the register's
	// transaction): the tracked leak beats an untracked one.
	if resp.State.Raw.IsNull() {
		t.Fatal("state is null after the failed arm — the registered domain would be untracked")
	}
	got := rawToDomain(ctx, t, sch, resp.State.Raw)
	if got.ID.ValueString() != "dom-1" {
		t.Errorf("state id = %s, want the registered row tracked", got.ID.ValueString())
	}
}

// A wait that never sees verified leaves tainted state — the domain
// keeps verifying server-side (the janitor's cadence is slower than
// the deadline here) and the next apply/refresh resolves it.
func TestCustomDomainCreateWaitTimeout(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadDomain(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.domainGets = []scriptedResponse{script(200, domainPendingDoc)} // never verifies
	r.pd = f.testClient()

	planData := testDomain(func(d *customDomainModel) {
		d.WaitForVerification = types.BoolValue(true)
		d.Timeouts = timeoutsWith("20ms")
	})
	req := resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: domainToRaw(ctx, t, sch, &planData)},
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
	}
	r.Create(ctx, req, resp)

	assertError(t, &resp.Diagnostics, "did not verify", "timed out", `"pending"`)
	if resp.State.Raw.IsNull() {
		t.Fatal("state is null after a wait timeout — the verifying domain would be untracked")
	}
	got := rawToDomain(ctx, t, sch, resp.State.Raw)
	if got.ID.ValueString() != "dom-1" || got.State.ValueString() != "pending" {
		t.Errorf("id/state = %s/%s, want the registered row tracked at its pending truth",
			got.ID.ValueString(), got.State.ValueString())
	}
}

// A post-POST failure (the final read here) leaves the registered row
// tracked: tainted state beats an untracked leak.
func TestCustomDomainCreatePostPOSTFailureTracks(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadDomain(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.domainGets = []scriptedResponse{script(500, `{"errors": [{"title": "boom"}]}`)}
	r.pd = f.testClient()

	planData := testDomain()
	req := resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: domainToRaw(ctx, t, sch, &planData)},
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
	}
	r.Create(ctx, req, resp)

	assertError(t, &resp.Diagnostics, "Unable to read the custom domain", "boom")
	if resp.State.Raw.IsNull() {
		t.Fatal("state is null after a post-POST failure — the registered domain would be untracked")
	}
	got := rawToDomain(ctx, t, sch, resp.State.Raw)
	if got.ID.ValueString() != "dom-1" || got.State.ValueString() != "pending" {
		t.Errorf("id/state = %s/%s, want the registered row tracked", got.ID.ValueString(), got.State.ValueString())
	}
}

// The duplicate 400 surfaces verbatim, with the platform-wide context
// — no adoption probe: the registered name may belong to ANOTHER org,
// and nothing was created (state stays null).
func TestCustomDomainCreateDuplicateSurfacesVerbatim(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadDomain(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.domainPostStatus = http.StatusBadRequest
	f.domainPostDoc = `{"errors": [{"detail": "is already registered"}]}`
	r.pd = f.testClient()

	planData := testDomain()
	req := resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: domainToRaw(ctx, t, sch, &planData)},
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
	}
	r.Create(ctx, req, resp)

	assertError(t, &resp.Diagnostics, "is already registered", "across all organizations")
	if !resp.State.Raw.IsNull() {
		t.Errorf("state = %v, want null (nothing was created)", resp.State.Raw)
	}
	for _, req := range f.all() {
		if req.method == http.MethodGet && strings.Contains(req.path, "/access-domains/") {
			t.Errorf("GET %s ran — the duplicate refusal gets NO adoption probe", req.path)
		}
	}
}

// A 403 paywall refusal rides the wire's own wording — the register's
// entitlement gate, surfaced verbatim. The platform-wide gloss is the
// DUPLICATE refusal's context only.
func TestCustomDomainCreateSurfacesPaywall(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadDomain(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.domainPostStatus = http.StatusForbidden
	f.domainPostDoc = `{"errors": [{"detail": "account has never paid"}]}`
	r.pd = f.testClient()

	planData := testDomain()
	req := resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: domainToRaw(ctx, t, sch, &planData)},
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
	}
	r.Create(ctx, req, resp)

	assertError(t, &resp.Diagnostics, "account has never paid")
	assertErrorLacks(t, &resp.Diagnostics, "across all organizations")
	if !resp.State.Raw.IsNull() {
		t.Errorf("state = %v, want null (nothing was created)", resp.State.Raw)
	}
}

// assertErrorLacks fails when any error diagnostic's text carries the
// needle — the negative twin of assertError.
func assertErrorLacks(t *testing.T, diags *diag.Diagnostics, needle string) {
	t.Helper()
	for _, d := range diags.Errors() {
		if strings.Contains(d.Summary()+" | "+d.Detail(), needle) {
			t.Fatalf("diagnostics unexpectedly contain %q: %+v", needle, diags.Errors())
		}
	}
}

// Read maps the wire mirror and seeds organization from the WIRE's
// organization_id (config left it null) — with no orgs LIST: the read
// goes by id, so a multi-org token with no default imports fine.
func TestCustomDomainReadMapsRowAndSeeds(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadDomain(ctx, t)

	f := newFakeAPI(t)
	f.domainGets = []scriptedResponse{script(200, domainVerifiedDoc)}
	r.pd = f.testClient()

	stateData := testDomain(func(d *customDomainModel) { d.ID = types.StringValue("dom-1") })
	req := resource.ReadRequest{
		State: tfsdk.State{Schema: sch, Raw: domainToRaw(ctx, t, sch, &stateData)},
	}
	resp := &resource.ReadResponse{
		State: tfsdk.State{Schema: sch, Raw: domainToRaw(ctx, t, sch, &stateData)},
	}
	r.Read(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", resp.Diagnostics)
	}

	findOne(t, f.all(), http.MethodGet, "/api/v1/access-domains/dom-1")

	got := rawToDomain(ctx, t, sch, resp.State.Raw)
	if got.Domain.ValueString() != "apps.customer.com" || got.State.ValueString() != "verified" {
		t.Errorf("domain/state = %s/%s, want the wire's", got.Domain.ValueString(), got.State.ValueString())
	}
	if got.Kind.ValueString() != "custom" || got.OrganizationID.ValueString() != "org-1" {
		t.Errorf("kind/organization_id = %s/%s, want the wire's echo",
			got.Kind.ValueString(), got.OrganizationID.ValueString())
	}
	if got.Organization.ValueString() != "org-1" {
		t.Errorf("organization = %q, want the wire's org seeded (config was null)", got.Organization.ValueString())
	}
	for _, req := range f.all() {
		if strings.Contains(req.path, "/organizations") {
			t.Errorf("GET %s ran — the read goes by id, no orgs LIST", req.path)
		}
	}
}

func TestCustomDomainRead404Removes(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadDomain(ctx, t)

	f := newFakeAPI(t)
	f.domainGets = []scriptedResponse{script(http.StatusNotFound, `{"errors": [{"status": "404"}]}`)}
	r.pd = f.testClient()

	stateData := testDomain(func(d *customDomainModel) { d.ID = types.StringValue("dom-1") })
	req := resource.ReadRequest{
		State: tfsdk.State{Schema: sch, Raw: domainToRaw(ctx, t, sch, &stateData)},
	}
	resp := &resource.ReadResponse{
		State: tfsdk.State{Schema: sch, Raw: domainToRaw(ctx, t, sch, &stateData)},
	}
	r.Read(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsNull() {
		t.Errorf("state after 404 = %v, want null (removed)", resp.State.Raw)
	}
}

// Delete is one synchronous DELETE — nothing to poll — and the 404 is
// already-gone: idempotent destroy.
func TestCustomDomainDeleteSynchronous(t *testing.T) {
	t.Run("200 lands", func(t *testing.T) {
		ctx := context.Background()
		r, sch := newSchemadDomain(ctx, t)

		f := newFakeAPI(t)
		r.pd = f.testClient()

		stateData := testDomain(func(d *customDomainModel) { d.ID = types.StringValue("dom-1") })
		req := resource.DeleteRequest{
			State: tfsdk.State{Schema: sch, Raw: domainToRaw(ctx, t, sch, &stateData)},
		}
		resp := &resource.DeleteResponse{
			State: tfsdk.State{Schema: sch, Raw: domainToRaw(ctx, t, sch, &stateData)},
		}
		r.Delete(ctx, req, resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("Delete: %v", resp.Diagnostics)
		}

		// The teardown is the platform's whole operation behind one
		// DELETE — no polls, no org resolution, nothing else.
		reqs := f.all()
		if len(reqs) != 1 || reqs[0].method != http.MethodDelete {
			t.Errorf("requests = %+v, want exactly the one DELETE", reqs)
		}
		if !resp.State.Raw.IsNull() {
			t.Errorf("state after delete = %v, want null (removed)", resp.State.Raw)
		}
	})

	t.Run("404 is idempotent success", func(t *testing.T) {
		ctx := context.Background()
		r, sch := newSchemadDomain(ctx, t)

		f := newFakeAPI(t)
		f.domainDeleteStatus = http.StatusNotFound
		r.pd = f.testClient()

		stateData := testDomain(func(d *customDomainModel) { d.ID = types.StringValue("dom-1") })
		req := resource.DeleteRequest{
			State: tfsdk.State{Schema: sch, Raw: domainToRaw(ctx, t, sch, &stateData)},
		}
		resp := &resource.DeleteResponse{
			State: tfsdk.State{Schema: sch, Raw: domainToRaw(ctx, t, sch, &stateData)},
		}
		r.Delete(ctx, req, resp)
		assertNoErrors(t, &resp.Diagnostics)
		if !resp.State.Raw.IsNull() {
			t.Errorf("state after a 404 delete = %v, want null (removed)", resp.State.Raw)
		}
	})

	// The platform-kind refusal is unreachable for provider-created
	// rows (register forces :custom) — but surfaces verbatim if ever
	// seen (e.g. an id re-pointed by hand).
	t.Run("platform-kind 400 surfaces verbatim", func(t *testing.T) {
		ctx := context.Background()
		r, sch := newSchemadDomain(ctx, t)

		f := newFakeAPI(t)
		f.domainDeleteStatus = http.StatusBadRequest
		f.domainDeleteDoc = `{"errors": [{"detail": "platform domains are not removable"}]}`
		r.pd = f.testClient()

		stateData := testDomain(func(d *customDomainModel) { d.ID = types.StringValue("dom-1") })
		req := resource.DeleteRequest{
			State: tfsdk.State{Schema: sch, Raw: domainToRaw(ctx, t, sch, &stateData)},
		}
		resp := &resource.DeleteResponse{
			State: tfsdk.State{Schema: sch, Raw: domainToRaw(ctx, t, sch, &stateData)},
		}
		r.Delete(ctx, req, resp)
		assertError(t, &resp.Diagnostics, "platform domains are not removable")
	})
}

// Import by id, then Read: the wire mirror lands and organization
// seeds from the resolution — a config that leaves it unset converges.
func TestCustomDomainImportConverges(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadDomain(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.domainGets = []scriptedResponse{script(200, domainVerifiedDoc)}
	r.pd = f.testClient()

	impReq := resource.ImportStateRequest{ID: "dom-1"}
	impResp := &resource.ImportStateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
	}
	r.ImportState(ctx, impReq, impResp)
	if impResp.Diagnostics.HasError() {
		t.Fatalf("ImportState: %v", impResp.Diagnostics)
	}

	readReq := resource.ReadRequest{State: impResp.State}
	readResp := &resource.ReadResponse{State: impResp.State}
	r.Read(ctx, readReq, readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", readResp.Diagnostics)
	}

	imported := rawToDomain(ctx, t, sch, readResp.State.Raw)
	if imported.ID.ValueString() != "dom-1" || imported.Domain.ValueString() != "apps.customer.com" {
		t.Fatalf("imported id/domain = %s/%s, want the wire's", imported.ID.ValueString(), imported.Domain.ValueString())
	}
	if imported.Kind.ValueString() != "custom" || imported.State.ValueString() != "verified" ||
		imported.OrganizationID.ValueString() != "org-1" {
		t.Errorf("kind/state/organization_id = %s/%s/%s, want fully populated",
			imported.Kind.ValueString(), imported.State.ValueString(), imported.OrganizationID.ValueString())
	}
	if imported.Organization.ValueString() != "org-1" {
		t.Errorf("organization = %q, want the resolution's org seeded", imported.Organization.ValueString())
	}
}

// Update is the re-read-and-set no-op: nothing is PATCHable, so the
// only traffic is the fresh read — no POST, no PATCH, no DELETE, and
// no orgs LIST.
func TestCustomDomainUpdateReReads(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadDomain(ctx, t)

	f := newFakeAPI(t)
	f.domainGets = []scriptedResponse{script(200, domainVerifiedDoc)}
	r.pd = f.testClient()

	seeded := testDomain(func(d *customDomainModel) {
		d.ID = types.StringValue("dom-1")
		d.WaitForVerification = types.BoolValue(true)
		d.Organization = types.StringValue("org-1")
		d.Kind = types.StringValue("custom")
		d.State = types.StringValue("pending")
		d.OrganizationID = types.StringValue("org-1")
	})
	req := resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: sch, Raw: domainToRaw(ctx, t, sch, &seeded)},
		State: tfsdk.State{Schema: sch, Raw: domainToRaw(ctx, t, sch, &seeded)},
	}
	resp := &resource.UpdateResponse{
		State: tfsdk.State{Schema: sch, Raw: domainToRaw(ctx, t, sch, &seeded)},
	}
	r.Update(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", resp.Diagnostics)
	}

	for _, req := range f.all() {
		if req.method != http.MethodGet {
			t.Errorf("unexpected %s %s — nothing is PATCHable, update is a re-read", req.method, req.path)
		}
	}

	got := rawToDomain(ctx, t, sch, resp.State.Raw)
	if got.State.ValueString() != "verified" {
		t.Errorf("state = %s, want the fresh read's", got.State.ValueString())
	}
	if !got.WaitForVerification.ValueBool() || got.Organization.ValueString() != "org-1" {
		t.Error("config values (wait_for_verification/organization) lost from state")
	}
}

// Update over a domain removed out-of-band removes it from state —
// never a clean apply over a ghost.
func TestCustomDomainUpdateRemovesOn404(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadDomain(ctx, t)

	f := newFakeAPI(t)
	f.domainGets = []scriptedResponse{script(http.StatusNotFound, `{"errors": [{"status": "404"}]}`)}
	r.pd = f.testClient()

	seeded := testDomain(func(d *customDomainModel) {
		d.ID = types.StringValue("dom-1")
		d.Organization = types.StringValue("org-1")
	})
	req := resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: sch, Raw: domainToRaw(ctx, t, sch, &seeded)},
		State: tfsdk.State{Schema: sch, Raw: domainToRaw(ctx, t, sch, &seeded)},
	}
	resp := &resource.UpdateResponse{
		State: tfsdk.State{Schema: sch, Raw: domainToRaw(ctx, t, sch, &seeded)},
	}
	r.Update(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsNull() {
		t.Errorf("state after a 404 update = %v, want null (removed)", resp.State.Raw)
	}
}

// THE unknown-identity update: when wait_for_verification or the
// timeouts change, core plans the Computed id UNKNOWN — and reading by
// the plan's ValueString() would GET /api/v1/access-domains/ (no id),
// whose 404 removes a LIVE domain. The row's id must come from state.
func TestCustomDomainUpdateCarriesRowIDFromState(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadDomain(ctx, t)

	f := newFakeAPI(t)
	f.domainGets = []scriptedResponse{script(200, domainVerifiedDoc)}
	r.pd = f.testClient()

	stateData := seededDomain()
	planData := seededDomain(func(d *customDomainModel) {
		d.WaitForVerification = types.BoolValue(true)
		// Core's shape for Computed attributes on this plan.
		d.ID = types.StringUnknown()
		d.Kind = types.StringUnknown()
		d.State = types.StringUnknown()
		d.OrganizationID = types.StringUnknown()
	})
	req := resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: sch, Raw: domainToRaw(ctx, t, sch, &planData)},
		State: tfsdk.State{Schema: sch, Raw: domainToRaw(ctx, t, sch, &stateData)},
	}
	resp := &resource.UpdateResponse{
		State: tfsdk.State{Schema: sch, Raw: domainToRaw(ctx, t, sch, &stateData)},
	}
	r.Update(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", resp.Diagnostics)
	}

	// The read went to the ROW, not the empty-id path.
	gets := 0
	for _, req := range f.all() {
		if req.method == http.MethodGet && strings.HasSuffix(req.path, "/access-domains/dom-1") {
			gets++
			continue
		}
		if strings.HasPrefix(req.path, "/api/v1/access-domains") {
			t.Errorf("GET %s ran — the plan's unknown id leaked into the read path", req.path)
		}
	}
	if gets != 1 {
		t.Errorf("row GETs = %d, want exactly one by the state's id", gets)
	}

	if resp.State.Raw.IsNull() {
		t.Fatal("state after the update = null — a LIVE domain was removed from state")
	}
	got := rawToDomain(ctx, t, sch, resp.State.Raw)
	if got.ID.ValueString() != "dom-1" || got.State.ValueString() != "verified" {
		t.Errorf("id/state = %s/%s, want the live row tracked and refreshed", got.ID.ValueString(), got.State.ValueString())
	}
	if !got.WaitForVerification.ValueBool() {
		t.Error("wait_for_verification lost from state — the config change must stick")
	}
}

// The schema contract: domain is Required + RequiresReplace (a
// different name is a different row); organization is Optional+
// Computed + RequiresReplace (the seed pattern, create-only on the
// wire); wait_for_verification is config-only Optional (not Computed —
// the wire has no echo); the four computed fields mirror the wire; the
// timeouts block carries create only.
func TestCustomDomainSchemaContract(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadDomain(ctx, t)

	want := []string{
		"domain", "organization", "wait_for_verification",
		"id", "kind", "state", "organization_id",
	}
	if len(sch.Attributes) != len(want) {
		t.Fatalf("schema attributes = %d, want exactly %d (%v)", len(sch.Attributes), len(want), want)
	}
	for _, name := range want {
		if _, ok := sch.Attributes[name]; !ok {
			t.Errorf("schema missing attribute %q", name)
		}
	}

	if !sch.Attributes["domain"].IsRequired() {
		t.Error("domain is not Required")
	}
	if !sch.Attributes["organization"].IsOptional() || !sch.Attributes["organization"].IsComputed() {
		t.Error("organization is not Optional+Computed — imports cannot converge without the seed")
	}
	if sch.Attributes["wait_for_verification"].IsComputed() {
		t.Error("wait_for_verification must not be Computed — the wire has no echo, it is config-only")
	}
	if sch.Attributes["wait_for_verification"].IsRequired() {
		t.Error("wait_for_verification is not Optional (default false)")
	}
	// domain's canonicalization-and-replace modifier rides the schema;
	// organization has NONE — its replacement decision needs the orgs
	// LIST, so it lives in the resource's ModifyPlan (the schema, and
	// any modifiers built with it, are cached from a throwaway
	// instance with no client).
	domainModifiers := sch.Attributes["domain"].(schema.StringAttribute).PlanModifiers
	if len(domainModifiers) != 1 {
		t.Fatalf("domain plan modifiers = %d, want exactly the canonicalDomain modifier", len(domainModifiers))
	}
	if _, ok := domainModifiers[0].(canonicalDomain); !ok {
		t.Errorf("domain modifier = %T, want canonicalDomain", domainModifiers[0])
	}
	if got := sch.Attributes["organization"].(schema.StringAttribute).PlanModifiers; len(got) != 0 {
		t.Errorf("organization carries attribute plan modifiers (%v) — its decision is ModifyPlan's", got)
	}
	if _, ok := any(r).(resource.ResourceWithModifyPlan); !ok {
		t.Error("resource does not implement ResourceWithModifyPlan — organization has no replacement decision")
	}
	for _, name := range []string{"id", "kind", "state", "organization_id"} {
		if len(sch.Attributes[name].(schema.StringAttribute).PlanModifiers) != 0 {
			t.Errorf("%s carries plan modifiers — nothing here replaces in place", name)
		}
	}
	if len(sch.Attributes["wait_for_verification"].(schema.BoolAttribute).PlanModifiers) != 0 {
		t.Error("wait_for_verification carries plan modifiers — flipping it only re-reads")
	}
	for _, name := range []string{"id", "kind", "state", "organization_id"} {
		if !sch.Attributes[name].IsComputed() {
			t.Errorf("%s is not Computed (the wire echo)", name)
		}
	}

	block, ok := sch.Blocks["timeouts"].(schema.SingleNestedBlock)
	if !ok {
		t.Fatalf("timeouts block missing or not a SingleNestedBlock: %v", sch.Blocks)
	}
	if _, ok := block.Attributes["create"]; !ok {
		t.Error("timeouts.create missing — the wait deadline has no config surface")
	}
	if _, ok := block.Attributes["delete"]; ok {
		t.Error("timeouts.delete present — delete is a synchronous teardown, there is nothing to bound")
	}

	md := &resource.MetadataResponse{}
	r.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: "fluxvale"}, md)
	if md.TypeName != "fluxvale_custom_domain" {
		t.Errorf("TypeName = %q, want fluxvale_custom_domain", md.TypeName)
	}
}

// seededDomain is a tracked, imported-style row: every wire field
// populated, organization carrying the resolved id.
func seededDomain(mutators ...func(*customDomainModel)) customDomainModel {
	return testDomain(append([]func(*customDomainModel){
		func(d *customDomainModel) {
			d.ID = types.StringValue("dom-1")
			d.Domain = types.StringValue("apps.customer.com")
			d.Kind = types.StringValue("custom")
			d.State = types.StringValue("verified")
			d.Organization = types.StringValue("org-1")
			d.OrganizationID = types.StringValue("org-1")
		},
	}, mutators...)...)
}

// The blocker: a config (or provider block) omitting organization
// plans it UNKNOWN — the seed must fill it, or the unknown rides final
// state and the apply is rejected ("still indicated an unknown value
// after apply"), tainting every subsequent apply into
// destroy-and-re-register.
func TestCustomDomainCreateSeedsUnknownOrganization(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadDomain(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.domainGets = []scriptedResponse{script(200, domainPendingDoc)}
	r.pd = f.testClient()

	planData := testDomain(func(d *customDomainModel) { d.Organization = types.StringUnknown() })
	req := resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: domainToRaw(ctx, t, sch, &planData)},
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
	}
	r.Create(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Create: %v", resp.Diagnostics)
	}

	if attrs := attrsOf(t, findOne(t, f.all(), http.MethodPost, "/api/v1/access-domains")); attrs["organization_id"] != "org-1" {
		t.Errorf("organization_id = %v, want the sole org resolved from the unknown", attrs["organization_id"])
	}

	got := rawToDomain(ctx, t, sch, resp.State.Raw)
	if got.Organization.ValueString() != "org-1" || got.OrganizationID.ValueString() != "org-1" {
		t.Errorf("organization/organization_id = %q/%q, want the resolved org seeded (never an unknown)",
			got.Organization.ValueString(), got.OrganizationID.ValueString())
	}
}

// The domain plan modifier decides on CANONICAL names but plans only
// values core accepts: config value on create, prior value on a
// same-name casing diff, config value + replacement on a canonical
// change.
func TestCanonicalDomainModifier(t *testing.T) {
	run := func(plan, state types.String) *planmodifier.StringResponse {
		t.Helper()
		resp := &planmodifier.StringResponse{PlanValue: plan}
		canonicalDomain{}.PlanModifyString(context.Background(),
			planmodifier.StringRequest{Path: path.Root("domain"), PlanValue: plan, StateValue: state}, resp)
		return resp
	}

	t.Run("create-shaped mixed-case config plans the config value", func(t *testing.T) {
		resp := run(types.StringValue("Apps.Customer.COM"), types.StringNull())
		if resp.PlanValue.ValueString() != "Apps.Customer.COM" {
			t.Errorf("planned domain = %q, want the config value untouched (planned==config is core's rule on create)",
				resp.PlanValue.ValueString())
		}
		if resp.RequiresReplace {
			t.Error("a create cannot replace — the casing decision happens at the wire")
		}
	})

	t.Run("mixed-case config over canonical state plans the prior", func(t *testing.T) {
		resp := run(types.StringValue("Apps.Customer.COM"), types.StringValue("apps.customer.com"))
		if resp.PlanValue.ValueString() != "apps.customer.com" {
			t.Errorf("planned domain = %q, want the PRIOR value (planned==prior is the accepted casing rewrite)", resp.PlanValue.ValueString())
		}
		if resp.RequiresReplace {
			t.Error("casing alone must not replace — that is the destroy/re-register loop")
		}
	})

	t.Run("canonical config over canonical state stays clean", func(t *testing.T) {
		resp := run(types.StringValue("apps.customer.com"), types.StringValue("apps.customer.com"))
		if resp.PlanValue.ValueString() != "apps.customer.com" || resp.RequiresReplace {
			t.Errorf("plan/requiresReplace = %q/%v, want unchanged/false", resp.PlanValue.ValueString(), resp.RequiresReplace)
		}
	})

	t.Run("a different canonical name keeps the config and replaces", func(t *testing.T) {
		resp := run(types.StringValue("Other.Example.COM"), types.StringValue("apps.customer.com"))
		if resp.PlanValue.ValueString() != "Other.Example.COM" {
			t.Errorf("planned domain = %q, want the config value kept (a rewrite would trip core's plan rule)", resp.PlanValue.ValueString())
		}
		if !resp.RequiresReplace {
			t.Error("a canonical-name change must replace (a different name is a different row)")
		}
	})

	t.Run("a non-canonical prior form is canonical-equal", func(t *testing.T) {
		resp := run(types.StringValue("apps.customer.com"), types.StringValue("Apps.Customer.COM"))
		if resp.PlanValue.ValueString() != "Apps.Customer.COM" || resp.RequiresReplace {
			t.Errorf("plan/requiresReplace = %q/%v, want the prior form planned and no replace",
				resp.PlanValue.ValueString(), resp.RequiresReplace)
		}
	})

	t.Run("unknown and null plans are left alone", func(t *testing.T) {
		resp := run(types.StringUnknown(), types.StringValue("apps.customer.com"))
		if !resp.PlanValue.IsUnknown() || resp.RequiresReplace {
			t.Error("unknown plan values cannot be decided on yet — no-op")
		}
		resp = run(types.StringNull(), types.StringNull())
		if !resp.PlanValue.IsNull() || resp.RequiresReplace {
			t.Error("null plan values are the destroy shape — no-op")
		}
	})
}

// The production create flow end to end: the plan carries the config's
// form (core's plan rule), the wire request carries the canonical
// form, and the applied state carries the configured form — plan and
// applied state agree, no churn.
func TestCustomDomainMixedCaseConfigConverges(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadDomain(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = soleOrgDoc()
	f.domainGets = []scriptedResponse{script(200, domainPendingDoc)}
	r.pd = f.testClient()

	// The modifier leaves a create plan's config value alone — the
	// framework hands Create the config's form.
	mixed := testDomain(func(d *customDomainModel) { d.Domain = types.StringValue("Apps.Customer.COM") })
	modReq := planmodifier.StringRequest{
		Path:       path.Root("domain"),
		PlanValue:  mixed.Domain,
		StateValue: types.StringNull(),
	}
	modResp := &planmodifier.StringResponse{PlanValue: mixed.Domain}
	canonicalDomain{}.PlanModifyString(ctx, modReq, modResp)
	mixed.Domain = modResp.PlanValue

	req := resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: domainToRaw(ctx, t, sch, &mixed)},
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
	}
	r.Create(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Create: %v", resp.Diagnostics)
	}

	// The wire saw the canonical form.
	if attrs := attrsOf(t, findOne(t, f.all(), http.MethodPost, "/api/v1/access-domains")); attrs["domain"] != "apps.customer.com" {
		t.Errorf("POST domain = %v, want the canonical name (the send-time transform)", attrs["domain"])
	}
	// The wire returns the canonical row; state keeps the configured
	// form — equal to the plan, so the apply converges.
	got := rawToDomain(ctx, t, sch, resp.State.Raw)
	if got.Domain.ValueString() != "Apps.Customer.COM" {
		t.Errorf("state domain = %q, want the configured form preserved (plan and applied state agree)", got.Domain.ValueString())
	}
}

// A refresh over a configured mixed-case form keeps it: the wire's
// canonical name canonically equals the state's, so mapDomain
// preserves the user's casing.
func TestCustomDomainReadPreservesConfiguredForm(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadDomain(ctx, t)

	f := newFakeAPI(t)
	f.domainGets = []scriptedResponse{script(200, domainVerifiedDoc)} // canonical on the wire
	r.pd = f.testClient()

	stateData := testDomain(func(d *customDomainModel) {
		d.ID = types.StringValue("dom-1")
		d.Domain = types.StringValue("Apps.Customer.COM")
	})
	req := resource.ReadRequest{
		State: tfsdk.State{Schema: sch, Raw: domainToRaw(ctx, t, sch, &stateData)},
	}
	resp := &resource.ReadResponse{
		State: tfsdk.State{Schema: sch, Raw: domainToRaw(ctx, t, sch, &stateData)},
	}
	r.Read(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", resp.Diagnostics)
	}

	got := rawToDomain(ctx, t, sch, resp.State.Raw)
	if got.Domain.ValueString() != "Apps.Customer.COM" {
		t.Errorf("domain = %q, want the configured form preserved (the wire name canonically equals it)", got.Domain.ValueString())
	}
}

// ModifyPlan reconciles the planned organization against the stored
// row: same organization (in any form) plans the PRIOR value; a
// genuinely different organization keeps the config value and
// replaces. Create planning (null state) and destroy planning (null
// plan) are guarded — reading a null prior errors.
func TestCustomDomainModifyPlanNormalizesOrganization(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadDomain(ctx, t)

	t.Run("slug config over imported id plans the prior", func(t *testing.T) {
		f := newFakeAPI(t)
		f.orgsDoc = orgsDoc // acme → org-1
		r.pd = f.testClient()

		stateData := seededDomain()
		planData := seededDomain(func(d *customDomainModel) { d.Organization = types.StringValue("acme") })
		req := resource.ModifyPlanRequest{
			Plan:  tfsdk.Plan{Schema: sch, Raw: domainToRaw(ctx, t, sch, &planData)},
			State: tfsdk.State{Schema: sch, Raw: domainToRaw(ctx, t, sch, &stateData)},
		}
		resp := &resource.ModifyPlanResponse{Plan: tfsdk.Plan{Schema: sch, Raw: domainToRaw(ctx, t, sch, &planData)}}
		r.ModifyPlan(ctx, req, resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("ModifyPlan: %v", resp.Diagnostics)
		}

		got := rawToDomain(ctx, t, sch, resp.Plan.Raw)
		if got.Organization.ValueString() != "org-1" {
			t.Errorf("planned organization = %q, want the PRIOR value (planned==prior is the rewrite core accepts)", got.Organization.ValueString())
		}
		if len(resp.RequiresReplace) != 0 {
			t.Errorf("RequiresReplace = %v, want none — the org is the same, only the FORM differed", resp.RequiresReplace)
		}
	})

	t.Run("id config over prior-slug state plans the prior too", func(t *testing.T) {
		f := newFakeAPI(t)
		f.orgsDoc = orgsDoc
		r.pd = f.testClient()

		stateData := seededDomain(func(d *customDomainModel) { d.Organization = types.StringValue("acme") })
		planData := seededDomain(func(d *customDomainModel) { d.Organization = types.StringValue("org-1") })
		req := resource.ModifyPlanRequest{
			Plan:  tfsdk.Plan{Schema: sch, Raw: domainToRaw(ctx, t, sch, &planData)},
			State: tfsdk.State{Schema: sch, Raw: domainToRaw(ctx, t, sch, &stateData)},
		}
		resp := &resource.ModifyPlanResponse{Plan: tfsdk.Plan{Schema: sch, Raw: domainToRaw(ctx, t, sch, &planData)}}
		r.ModifyPlan(ctx, req, resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("ModifyPlan: %v", resp.Diagnostics)
		}

		got := rawToDomain(ctx, t, sch, resp.Plan.Raw)
		if got.Organization.ValueString() != "acme" {
			t.Errorf("planned organization = %q, want the prior slug kept (the forms name the same org)", got.Organization.ValueString())
		}
		if len(resp.RequiresReplace) != 0 {
			t.Errorf("RequiresReplace = %v, want none", resp.RequiresReplace)
		}
	})

	t.Run("a genuinely different organization keeps the config and replaces", func(t *testing.T) {
		f := newFakeAPI(t)
		f.orgsDoc = orgsDoc
		r.pd = f.testClient()

		stateData := seededDomain()
		planData := seededDomain(func(d *customDomainModel) { d.Organization = types.StringValue("b-co") })
		req := resource.ModifyPlanRequest{
			Plan:  tfsdk.Plan{Schema: sch, Raw: domainToRaw(ctx, t, sch, &planData)},
			State: tfsdk.State{Schema: sch, Raw: domainToRaw(ctx, t, sch, &stateData)},
		}
		resp := &resource.ModifyPlanResponse{Plan: tfsdk.Plan{Schema: sch, Raw: domainToRaw(ctx, t, sch, &planData)}}
		r.ModifyPlan(ctx, req, resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("ModifyPlan: %v", resp.Diagnostics)
		}

		got := rawToDomain(ctx, t, sch, resp.Plan.Raw)
		if got.Organization.ValueString() != "b-co" {
			t.Errorf("planned organization = %q, want the config value kept (a rewrite would trip core's plan rule)", got.Organization.ValueString())
		}
		if len(resp.RequiresReplace) != 1 {
			t.Errorf("RequiresReplace = %v, want exactly the organization path", resp.RequiresReplace)
		}
	})

	t.Run("create planning (null state) is a no-op", func(t *testing.T) {
		// THE null-prior guard: reading a null prior state errors —
		// this shape must return before any Get.
		f := newFakeAPI(t)
		f.orgsDoc = soleOrgDoc()
		r.pd = f.testClient()

		planData := testDomain(func(d *customDomainModel) { d.Organization = types.StringValue("acme") })
		req := resource.ModifyPlanRequest{
			Plan:  tfsdk.Plan{Schema: sch, Raw: domainToRaw(ctx, t, sch, &planData)},
			State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
		}
		resp := &resource.ModifyPlanResponse{Plan: tfsdk.Plan{Schema: sch, Raw: domainToRaw(ctx, t, sch, &planData)}}
		r.ModifyPlan(ctx, req, resp)
		assertNoErrors(t, &resp.Diagnostics)
		if len(f.all()) != 0 {
			t.Errorf("requests = %+v — create planning has no prior to reconcile", f.all())
		}
		got := rawToDomain(ctx, t, sch, resp.Plan.Raw)
		if got.Organization.ValueString() != "acme" {
			t.Errorf("planned organization = %q, want the config value untouched", got.Organization.ValueString())
		}
	})

	t.Run("null-prior organization stays unrewritten", func(t *testing.T) {
		f := newFakeAPI(t)
		f.orgsDoc = soleOrgDoc()
		r.pd = f.testClient()

		stateData := seededDomain(func(d *customDomainModel) { d.Organization = types.StringNull() })
		planData := seededDomain(func(d *customDomainModel) { d.Organization = types.StringValue("acme") })
		req := resource.ModifyPlanRequest{
			Plan:  tfsdk.Plan{Schema: sch, Raw: domainToRaw(ctx, t, sch, &planData)},
			State: tfsdk.State{Schema: sch, Raw: domainToRaw(ctx, t, sch, &stateData)},
		}
		resp := &resource.ModifyPlanResponse{Plan: tfsdk.Plan{Schema: sch, Raw: domainToRaw(ctx, t, sch, &planData)}}
		r.ModifyPlan(ctx, req, resp)
		assertNoErrors(t, &resp.Diagnostics)
		if len(f.all()) != 0 {
			t.Errorf("requests = %+v — nothing to reconcile against", f.all())
		}
		got := rawToDomain(ctx, t, sch, resp.Plan.Raw)
		if got.Organization.ValueString() != "acme" {
			t.Errorf("planned organization = %q, want the config value untouched", got.Organization.ValueString())
		}
	})

	t.Run("unknown plan organization skips the lookup", func(t *testing.T) {
		f := newFakeAPI(t)
		f.orgsDoc = soleOrgDoc()
		r.pd = f.testClient()

		stateData := seededDomain(func(d *customDomainModel) { d.Organization = types.StringNull() })
		planData := seededDomain(func(d *customDomainModel) { d.Organization = types.StringUnknown() })
		req := resource.ModifyPlanRequest{
			Plan:  tfsdk.Plan{Schema: sch, Raw: domainToRaw(ctx, t, sch, &planData)},
			State: tfsdk.State{Schema: sch, Raw: domainToRaw(ctx, t, sch, &stateData)},
		}
		resp := &resource.ModifyPlanResponse{Plan: tfsdk.Plan{Schema: sch, Raw: domainToRaw(ctx, t, sch, &planData)}}
		r.ModifyPlan(ctx, req, resp)
		assertNoErrors(t, &resp.Diagnostics)
		if len(f.all()) != 0 {
			t.Errorf("requests = %+v — an unknown plan org is create's seed's job, no lookup", f.all())
		}
	})

	t.Run("destroy planning is skipped", func(t *testing.T) {
		f := newFakeAPI(t)
		r.pd = f.testClient()

		stateData := seededDomain()
		req := resource.ModifyPlanRequest{
			Plan:  tfsdk.Plan{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
			State: tfsdk.State{Schema: sch, Raw: domainToRaw(ctx, t, sch, &stateData)},
		}
		resp := &resource.ModifyPlanResponse{}
		r.ModifyPlan(ctx, req, resp)
		assertNoErrors(t, &resp.Diagnostics)
		if len(f.all()) != 0 {
			t.Errorf("requests = %+v — destroy planning has no organization to compare", f.all())
		}
	})
}

// The review's required convergence case: import by id, then plan with
// a SLUG organization — the resolved forms name the same org, so the
// plan carries the prior (the wire-seeded id) and nothing replaces.
func TestCustomDomainImportThenSlugPlanConverges(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadDomain(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = orgsDoc
	f.domainGets = []scriptedResponse{script(200, domainVerifiedDoc)}
	r.pd = f.testClient()

	impReq := resource.ImportStateRequest{ID: "dom-1"}
	impResp := &resource.ImportStateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
	}
	r.ImportState(ctx, impReq, impResp)
	if impResp.Diagnostics.HasError() {
		t.Fatalf("ImportState: %v", impResp.Diagnostics)
	}

	readReq := resource.ReadRequest{State: impResp.State}
	readResp := &resource.ReadResponse{State: impResp.State}
	r.Read(ctx, readReq, readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", readResp.Diagnostics)
	}

	// The config the user writes: organization = "acme" (the slug),
	// everything else mirroring the imported row.
	planData := seededDomain(func(d *customDomainModel) { d.Organization = types.StringValue("acme") })
	mpReq := resource.ModifyPlanRequest{
		Plan:  tfsdk.Plan{Schema: sch, Raw: domainToRaw(ctx, t, sch, &planData)},
		State: tfsdk.State{Schema: sch, Raw: readResp.State.Raw},
	}
	mpResp := &resource.ModifyPlanResponse{Plan: tfsdk.Plan{Schema: sch, Raw: domainToRaw(ctx, t, sch, &planData)}}
	r.ModifyPlan(ctx, mpReq, mpResp)
	if mpResp.Diagnostics.HasError() {
		t.Fatalf("ModifyPlan rejected a converging slug config: %v", mpResp.Diagnostics)
	}
	if len(mpResp.RequiresReplace) != 0 {
		t.Errorf("RequiresReplace = %v, want none — the slug names the same org as the imported id", mpResp.RequiresReplace)
	}
	got := rawToDomain(ctx, t, sch, mpResp.Plan.Raw)
	if got.Organization.ValueString() != "org-1" {
		t.Errorf("planned organization = %q, want the normalized id", got.Organization.ValueString())
	}
}

// Import (and every refresh) needs no orgs LIST: the read goes by id
// and the wire seeds organization — a multi-org token with no default
// imports fine, and a revoked membership doesn't break the by-id read.
func TestCustomDomainImportNeedsNoOrgResolution(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadDomain(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = "" // any LIST would fail to decode — the tripwire
	f.domainGets = []scriptedResponse{script(200, domainVerifiedDoc)}
	r.pd = f.testClient()

	impReq := resource.ImportStateRequest{ID: "dom-1"}
	impResp := &resource.ImportStateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
	}
	r.ImportState(ctx, impReq, impResp)
	if impResp.Diagnostics.HasError() {
		t.Fatalf("ImportState: %v", impResp.Diagnostics)
	}

	readReq := resource.ReadRequest{State: impResp.State}
	readResp := &resource.ReadResponse{State: impResp.State}
	r.Read(ctx, readReq, readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", readResp.Diagnostics)
	}

	got := rawToDomain(ctx, t, sch, readResp.State.Raw)
	if got.Organization.ValueString() != "org-1" || got.OrganizationID.ValueString() != "org-1" {
		t.Errorf("organization/organization_id = %q/%q, want the wire's echo seeded",
			got.Organization.ValueString(), got.OrganizationID.ValueString())
	}
	for _, req := range f.all() {
		if strings.HasSuffix(req.path, "/organizations") {
			t.Errorf("GET %s ran — the by-id read must not resolve the org", req.path)
		}
	}
}
