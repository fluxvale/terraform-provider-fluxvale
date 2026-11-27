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

// newSchemadInstanceHostname builds the instance-hostname resource
// with its schema.
func newSchemadInstanceHostname(ctx context.Context, t *testing.T) (*instanceHostnameResource, schema.Schema) {
	t.Helper()
	r := NewInstanceHostnameResource().(*instanceHostnameResource)
	schemaResp := &resource.SchemaResponse{}
	r.Schema(ctx, resource.SchemaRequest{}, schemaResp)
	if schemaResp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", schemaResp.Diagnostics)
	}
	return r, schemaResp.Schema
}

func instanceHostnameToRaw(ctx context.Context, t *testing.T, sch schema.Schema, data *instanceHostnameModel) tftypes.Value {
	t.Helper()
	st := tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)}
	if diags := st.Set(ctx, data); diags.HasError() {
		t.Fatalf("State.Set: %v", diags)
	}
	return st.Raw
}

func rawToInstanceHostname(ctx context.Context, t *testing.T, sch schema.Schema, raw tftypes.Value) instanceHostnameModel {
	t.Helper()
	st := tfsdk.State{Schema: sch, Raw: raw}
	var data instanceHostnameModel
	if diags := st.Get(ctx, &data); diags.HasError() {
		t.Fatalf("State.Get: %v", diags)
	}
	return data
}

// testInstanceHostname builds an instanceHostnameModel with the
// required fields set; mutators adjust the rest.
func testInstanceHostname(mutators ...func(*instanceHostnameModel)) instanceHostnameModel {
	data := instanceHostnameModel{
		InstanceID:     types.StringValue("inst-1"),
		AccessDomainID: types.StringValue("dom-1"),
		Label:          types.StringValue("git"),
		ID:             types.StringNull(),
	}
	for _, m := range mutators {
		m(&data)
	}
	return data
}

// seededInstanceHostname is a tracked row: the wire's four fields
// populated with the fake's custom row (host-2, git under dom-1).
func seededInstanceHostname(mutators ...func(*instanceHostnameModel)) instanceHostnameModel {
	return testInstanceHostname(append([]func(*instanceHostnameModel){
		func(d *instanceHostnameModel) { d.ID = types.StringValue("host-2") },
	}, mutators...)...)
}

// hostnamesListWith renders a list document from raw row objects — the
// "is the row standing?" variants the adopt tests need.
func hostnamesListWith(rows ...string) string {
	return `{"data": [` + strings.Join(rows, ",") + `]}`
}

// refusalDoc renders the JSON:API error document carrying one
// sentence — the shape every mint/remove refusal rides.
func refusalDoc(detail string) string {
	return `{"errors": [{"detail": "` + detail + `"}]}`
}

// Create over the wrapper: the mint POST carries exactly
// access_domain_id + label (instance_id rides the path) and the 201's
// row is written into state verbatim.
func TestInstanceHostnameCreateMintsAndMapsRow(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadInstanceHostname(ctx, t)

	f := newFakeAPI(t)
	r.pd = f.testClient()

	planData := testInstanceHostname()
	req := resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: instanceHostnameToRaw(ctx, t, sch, &planData)},
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
	}
	r.Create(ctx, req, resp)
	assertNoErrors(t, &resp.Diagnostics)

	post := findOne(t, f.all(), http.MethodPost, "/api/v1/instances/inst-1/hostnames")
	attrs := attrsOf(t, post)
	if len(attrs) != 2 || attrs["access_domain_id"] != "dom-1" || attrs["label"] != "git" {
		t.Errorf("mint attributes = %v, want exactly access_domain_id + label (instance_id rides the path)", attrs)
	}

	got := rawToInstanceHostname(ctx, t, sch, resp.State.Raw)
	if got.ID.ValueString() != "host-2" || got.InstanceID.ValueString() != "inst-1" ||
		got.AccessDomainID.ValueString() != "dom-1" || got.Label.ValueString() != "git" {
		t.Errorf("id/instance_id/access_domain_id/label = %s/%s/%s/%s, want the wire's four fields",
			got.ID.ValueString(), got.InstanceID.ValueString(), got.AccessDomainID.ValueString(), got.Label.ValueString())
	}
	if len(resp.Diagnostics.Warnings()) != 0 {
		t.Errorf("warnings = %+v, want none (the mint succeeded)", resp.Diagnostics.Warnings())
	}
}

// The guard refusals surface the server's sentence verbatim and write
// no state. The 400s probe the list (and find no identical row here);
// the 404 fences — a foreign domain, a foreign instance — leave no row
// and get NO probe at all.
func TestInstanceHostnameCreateGuardRefusalsSurfaceVerbatim(t *testing.T) {
	cases := []struct {
		name   string
		status int
		doc    string
		needle string
		probe  bool // the 400/409 status-class probe
	}{
		{"not custom", http.StatusBadRequest, refusalDoc(hostnameRefusalNotCustom), hostnameRefusalNotCustom, true},
		{"not verified", http.StatusBadRequest, refusalDoc(hostnameRefusalNotVerified), hostnameRefusalNotVerified, true},
		{"different organization", http.StatusBadRequest, refusalDoc(hostnameRefusalDifferentOrg), hostnameRefusalDifferentOrg, true},
		{"foreign domain", http.StatusNotFound, `{"errors": [{"status": "404"}]}`, "status 404", false},
		{"foreign instance", http.StatusNotFound, `{"errors": [{"status": "404"}]}`, "status 404", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r, sch := newSchemadInstanceHostname(ctx, t)

			f := newFakeAPI(t)
			f.hostnamesPostStatus = tc.status
			f.hostnamesPostDoc = tc.doc
			f.hostnamesListDoc = hostnamesListWith(hostnameDefaultRowJSON) // no identical row
			r.pd = f.testClient()

			planData := testInstanceHostname()
			req := resource.CreateRequest{
				Plan: tfsdk.Plan{Schema: sch, Raw: instanceHostnameToRaw(ctx, t, sch, &planData)},
			}
			resp := &resource.CreateResponse{
				State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
			}
			r.Create(ctx, req, resp)

			assertError(t, &resp.Diagnostics, "Unable to add the custom address", tc.needle)
			assertErrorLacks(t, &resp.Diagnostics, "already stands")
			if !resp.State.Raw.IsNull() {
				t.Errorf("state = %v, want null (the mint was refused)", resp.State.Raw)
			}
			if len(resp.Diagnostics.Warnings()) != 0 {
				t.Errorf("warnings = %+v, want none (no standing row)", resp.Diagnostics.Warnings())
			}
			if tc.probe {
				findOne(t, f.all(), http.MethodGet, "/api/v1/instances/inst-1/hostnames")
			} else {
				for _, req := range f.all() {
					if req.method == http.MethodGet {
						t.Errorf("GET %s ran — the 404 fences leave no row, the probe never fires", req.path)
					}
				}
			}
		})
	}
}

// The row-stands refusals (the two "The address was added…" 400s) and
// the duplicate case behind BOTH pinned statuses (400 and 409 — the
// platform's route test accepts either, so the probe treats both
// alike): the probe finds the identical row (same domain + label, this
// instance) and adopts it — a warning quoting the server's refusal
// verbatim, state carrying the standing row's id.
func TestInstanceHostnameCreateAdoptsStandingRow(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		doc      string
		sentence string
	}{
		{"restart 400", http.StatusBadRequest, refusalDoc(hostnameRefusalRestart), hostnameRefusalRestart},
		{"app not told 400", http.StatusBadRequest, refusalDoc(hostnameRefusalAppNotTold), hostnameRefusalAppNotTold},
		{"duplicate 400", http.StatusBadRequest, refusalDoc(hostnameRefusalDuplicate), hostnameRefusalDuplicate},
		{"duplicate 409", http.StatusConflict, refusalDoc(hostnameRefusalDuplicate), hostnameRefusalDuplicate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r, sch := newSchemadInstanceHostname(ctx, t)

			f := newFakeAPI(t)
			f.hostnamesPostStatus = tc.status
			f.hostnamesPostDoc = tc.doc
			// The default list carries the identical standing row:
			// git under dom-1 on inst-1.
			r.pd = f.testClient()

			planData := testInstanceHostname()
			req := resource.CreateRequest{
				Plan: tfsdk.Plan{Schema: sch, Raw: instanceHostnameToRaw(ctx, t, sch, &planData)},
			}
			resp := &resource.CreateResponse{
				State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
			}
			r.Create(ctx, req, resp)
			assertNoErrors(t, &resp.Diagnostics)
			assertWarning(t, &resp.Diagnostics, "stands despite the mint's refusal", tc.sentence)

			// The probe really ran, and the standing row was adopted.
			findOne(t, f.all(), http.MethodGet, "/api/v1/instances/inst-1/hostnames")
			got := rawToInstanceHostname(ctx, t, sch, resp.State.Raw)
			if got.ID.ValueString() != "host-2" || got.InstanceID.ValueString() != "inst-1" ||
				got.AccessDomainID.ValueString() != "dom-1" || got.Label.ValueString() != "git" {
				t.Errorf("id/instance_id/access_domain_id/label = %s/%s/%s/%s, want the standing row adopted",
					got.ID.ValueString(), got.InstanceID.ValueString(), got.AccessDomainID.ValueString(), got.Label.ValueString())
			}
		})
	}
}

// A 400 or 409 whose probe finds NO identical row (or the probe itself
// errors) is the original refusal verbatim — no adoption, no state,
// no warning. The next apply re-mints.
func TestInstanceHostnameCreateRefusedWithoutStandingRow(t *testing.T) {
	cases := []struct {
		name   string
		status int
		doc    string
		needle string
	}{
		{"400 guard sentence", http.StatusBadRequest, refusalDoc(hostnameRefusalNotCustom), hostnameRefusalNotCustom},
		{"400 duplicate sentence", http.StatusBadRequest, refusalDoc(hostnameRefusalDuplicate), hostnameRefusalDuplicate},
		{"409 duplicate sentence", http.StatusConflict, refusalDoc(hostnameRefusalDuplicate), hostnameRefusalDuplicate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r, sch := newSchemadInstanceHostname(ctx, t)

			f := newFakeAPI(t)
			f.hostnamesPostStatus = tc.status
			f.hostnamesPostDoc = tc.doc
			f.hostnamesListDoc = hostnamesListWith(hostnameDefaultRowJSON) // the row is absent
			r.pd = f.testClient()

			planData := testInstanceHostname()
			req := resource.CreateRequest{
				Plan: tfsdk.Plan{Schema: sch, Raw: instanceHostnameToRaw(ctx, t, sch, &planData)},
			}
			resp := &resource.CreateResponse{
				State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
			}
			r.Create(ctx, req, resp)

			assertError(t, &resp.Diagnostics, "Unable to add the custom address", tc.needle)
			if !resp.State.Raw.IsNull() {
				t.Errorf("state = %v, want null (no standing row to adopt)", resp.State.Raw)
			}
			if len(resp.Diagnostics.Warnings()) != 0 {
				t.Errorf("warnings = %+v, want none (nothing was adopted)", resp.Diagnostics.Warnings())
			}
		})
	}
}

// Read maps the wire's four fields for the row the state's id names —
// the wire is the truth.
func TestInstanceHostnameReadMapsWireRow(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadInstanceHostname(ctx, t)

	f := newFakeAPI(t)
	r.pd = f.testClient()

	stateData := seededInstanceHostname()
	req := resource.ReadRequest{
		State: tfsdk.State{Schema: sch, Raw: instanceHostnameToRaw(ctx, t, sch, &stateData)},
	}
	resp := &resource.ReadResponse{
		State: tfsdk.State{Schema: sch, Raw: instanceHostnameToRaw(ctx, t, sch, &stateData)},
	}
	r.Read(ctx, req, resp)
	assertNoErrors(t, &resp.Diagnostics)

	findOne(t, f.all(), http.MethodGet, "/api/v1/instances/inst-1/hostnames")

	got := rawToInstanceHostname(ctx, t, sch, resp.State.Raw)
	if got.ID.ValueString() != "host-2" || got.InstanceID.ValueString() != "inst-1" ||
		got.AccessDomainID.ValueString() != "dom-1" || got.Label.ValueString() != "git" {
		t.Errorf("id/instance_id/access_domain_id/label = %s/%s/%s/%s, want the wire's four fields",
			got.ID.ValueString(), got.InstanceID.ValueString(), got.AccessDomainID.ValueString(), got.Label.ValueString())
	}
}

// The list without the row: the platform removed it — gone is gone,
// the resource leaves state and the next apply re-mints.
func TestInstanceHostnameReadRemovesWhenRowGone(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadInstanceHostname(ctx, t)

	f := newFakeAPI(t)
	f.hostnamesListDoc = hostnamesListWith(hostnameDefaultRowJSON) // host-2 is absent
	r.pd = f.testClient()

	stateData := seededInstanceHostname()
	req := resource.ReadRequest{
		State: tfsdk.State{Schema: sch, Raw: instanceHostnameToRaw(ctx, t, sch, &stateData)},
	}
	resp := &resource.ReadResponse{
		State: tfsdk.State{Schema: sch, Raw: instanceHostnameToRaw(ctx, t, sch, &stateData)},
	}
	r.Read(ctx, req, resp)
	assertNoErrors(t, &resp.Diagnostics)
	if !resp.State.Raw.IsNull() {
		t.Errorf("state after a row-less list = %v, want null (removed)", resp.State.Raw)
	}
}

// The empty list is the gone-INSTANCE case: a foreign or gone instance
// answers 200 [] — row-absence is the only "gone" signal the list has.
func TestInstanceHostnameReadRemovesOnEmptyList(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadInstanceHostname(ctx, t)

	f := newFakeAPI(t)
	f.hostnamesListDoc = `{"data": []}`
	r.pd = f.testClient()

	stateData := seededInstanceHostname()
	req := resource.ReadRequest{
		State: tfsdk.State{Schema: sch, Raw: instanceHostnameToRaw(ctx, t, sch, &stateData)},
	}
	resp := &resource.ReadResponse{
		State: tfsdk.State{Schema: sch, Raw: instanceHostnameToRaw(ctx, t, sch, &stateData)},
	}
	r.Read(ctx, req, resp)
	assertNoErrors(t, &resp.Diagnostics)
	if !resp.State.Raw.IsNull() {
		t.Errorf("state after an empty list = %v, want null (removed)", resp.State.Raw)
	}
}

// Update is the defensive re-read no-op: the identity is carried from
// STATE (never a plan unknown), a found row is mapped into state from
// the wire, a gone row removes — and nothing but the list GET ever
// runs (nothing is PATCHable; every change replaces).
func TestInstanceHostnameUpdateIsDefensiveReRead(t *testing.T) {
	t.Run("found: state set from the plan, identity from state", func(t *testing.T) {
		ctx := context.Background()
		r, sch := newSchemadInstanceHostname(ctx, t)

		f := newFakeAPI(t)
		r.pd = f.testClient()

		stateData := seededInstanceHostname()
		// Core's shape for the Computed id on this plan (and, were a
		// config diff reachable, the RequiresReplace identities) —
		// unknowns must not drive the read.
		planData := seededInstanceHostname(func(d *instanceHostnameModel) {
			d.ID = types.StringUnknown()
			d.InstanceID = types.StringUnknown()
			d.AccessDomainID = types.StringUnknown()
			d.Label = types.StringUnknown()
		})
		req := resource.UpdateRequest{
			Plan:  tfsdk.Plan{Schema: sch, Raw: instanceHostnameToRaw(ctx, t, sch, &planData)},
			State: tfsdk.State{Schema: sch, Raw: instanceHostnameToRaw(ctx, t, sch, &stateData)},
		}
		resp := &resource.UpdateResponse{
			State: tfsdk.State{Schema: sch, Raw: instanceHostnameToRaw(ctx, t, sch, &stateData)},
		}
		r.Update(ctx, req, resp)
		assertNoErrors(t, &resp.Diagnostics)

		for _, req := range f.all() {
			if req.method != http.MethodGet {
				t.Errorf("unexpected %s %s — update is a re-read, nothing mutates", req.method, req.path)
			}
			if req.method == http.MethodGet && !strings.HasSuffix(req.path, "/instances/inst-1/hostnames") {
				t.Errorf("GET %s ran — the plan's unknown identity leaked into the read path", req.path)
			}
		}

		got := rawToInstanceHostname(ctx, t, sch, resp.State.Raw)
		if got.ID.ValueString() != "host-2" || got.InstanceID.ValueString() != "inst-1" ||
			got.AccessDomainID.ValueString() != "dom-1" || got.Label.ValueString() != "git" {
			t.Errorf("id/instance_id/access_domain_id/label = %s/%s/%s/%s, want the wire's row in state",
				got.ID.ValueString(), got.InstanceID.ValueString(), got.AccessDomainID.ValueString(), got.Label.ValueString())
		}
	})

	t.Run("gone: removed from state", func(t *testing.T) {
		ctx := context.Background()
		r, sch := newSchemadInstanceHostname(ctx, t)

		f := newFakeAPI(t)
		f.hostnamesListDoc = hostnamesListWith(hostnameDefaultRowJSON) // host-2 is absent
		r.pd = f.testClient()

		seeded := seededInstanceHostname()
		req := resource.UpdateRequest{
			Plan:  tfsdk.Plan{Schema: sch, Raw: instanceHostnameToRaw(ctx, t, sch, &seeded)},
			State: tfsdk.State{Schema: sch, Raw: instanceHostnameToRaw(ctx, t, sch, &seeded)},
		}
		resp := &resource.UpdateResponse{
			State: tfsdk.State{Schema: sch, Raw: instanceHostnameToRaw(ctx, t, sch, &seeded)},
		}
		r.Update(ctx, req, resp)
		assertNoErrors(t, &resp.Diagnostics)
		if !resp.State.Raw.IsNull() {
			t.Errorf("state after a gone-row update = %v, want null (removed)", resp.State.Raw)
		}
	})
}

// Delete is one synchronous remove: the 200 and the already-gone 404
// both clear state (idempotent destroy), the platform-row 400 (the
// default address is not removable) surfaces verbatim and keeps state.
func TestInstanceHostnameDelete(t *testing.T) {
	t.Run("200 removes", func(t *testing.T) {
		ctx := context.Background()
		r, sch := newSchemadInstanceHostname(ctx, t)

		f := newFakeAPI(t)
		r.pd = f.testClient()

		stateData := seededInstanceHostname()
		req := resource.DeleteRequest{
			State: tfsdk.State{Schema: sch, Raw: instanceHostnameToRaw(ctx, t, sch, &stateData)},
		}
		resp := &resource.DeleteResponse{
			State: tfsdk.State{Schema: sch, Raw: instanceHostnameToRaw(ctx, t, sch, &stateData)},
		}
		r.Delete(ctx, req, resp)
		assertNoErrors(t, &resp.Diagnostics)

		delete := findOne(t, f.all(), http.MethodDelete, "/api/v1/instance-hostnames/host-2")
		if len(delete.body) != 0 {
			t.Errorf("delete body = %v, want none (the route takes no input)", delete.body)
		}
		if !resp.State.Raw.IsNull() {
			t.Errorf("state after delete = %v, want null (removed)", resp.State.Raw)
		}
	})

	t.Run("404 is idempotent success", func(t *testing.T) {
		ctx := context.Background()
		r, sch := newSchemadInstanceHostname(ctx, t)

		f := newFakeAPI(t)
		f.hostnamesDeleteStatus = http.StatusNotFound
		r.pd = f.testClient()

		stateData := seededInstanceHostname()
		req := resource.DeleteRequest{
			State: tfsdk.State{Schema: sch, Raw: instanceHostnameToRaw(ctx, t, sch, &stateData)},
		}
		resp := &resource.DeleteResponse{
			State: tfsdk.State{Schema: sch, Raw: instanceHostnameToRaw(ctx, t, sch, &stateData)},
		}
		r.Delete(ctx, req, resp)
		assertNoErrors(t, &resp.Diagnostics)
		if !resp.State.Raw.IsNull() {
			t.Errorf("state after a 404 delete = %v, want null (already gone)", resp.State.Raw)
		}
	})

	t.Run("platform-row 400 surfaces verbatim and keeps state", func(t *testing.T) {
		ctx := context.Background()
		r, sch := newSchemadInstanceHostname(ctx, t)

		f := newFakeAPI(t)
		f.hostnamesDeleteStatus = http.StatusBadRequest
		f.hostnamesDeleteDoc = refusalDoc(hostnameRefusalPlatformRow)
		r.pd = f.testClient()

		stateData := seededInstanceHostname()
		req := resource.DeleteRequest{
			State: tfsdk.State{Schema: sch, Raw: instanceHostnameToRaw(ctx, t, sch, &stateData)},
		}
		resp := &resource.DeleteResponse{
			State: tfsdk.State{Schema: sch, Raw: instanceHostnameToRaw(ctx, t, sch, &stateData)},
		}
		r.Delete(ctx, req, resp)

		assertError(t, &resp.Diagnostics, "Unable to remove the custom address", hostnameRefusalPlatformRow)
		if resp.State.Raw.IsNull() {
			t.Error("state removed on a refused delete — the row still stands server-side")
		}
	})
}

// Import names the instance to list under and the row id to find;
// the following Read fills label and access_domain_id from the list.
func TestInstanceHostnameImportSeedsAndReadFills(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadInstanceHostname(ctx, t)

	f := newFakeAPI(t) // the default list carries host-1 (the platform default row)
	r.pd = f.testClient()

	impReq := resource.ImportStateRequest{ID: "inst-1/host-1"}
	impResp := &resource.ImportStateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
	}
	r.ImportState(ctx, impReq, impResp)
	if impResp.Diagnostics.HasError() {
		t.Fatalf("ImportState: %v", impResp.Diagnostics)
	}

	imported := rawToInstanceHostname(ctx, t, sch, impResp.State.Raw)
	if imported.InstanceID.ValueString() != "inst-1" || imported.ID.ValueString() != "host-1" {
		t.Fatalf("imported instance_id/id = %s/%s, want the two halves of the import id",
			imported.InstanceID.ValueString(), imported.ID.ValueString())
	}
	if !imported.Label.IsNull() || !imported.AccessDomainID.IsNull() {
		t.Errorf("label/access_domain_id = %s/%s, want null (Read fills them from the list)",
			imported.Label, imported.AccessDomainID)
	}

	readReq := resource.ReadRequest{State: impResp.State}
	readResp := &resource.ReadResponse{State: impResp.State}
	r.Read(ctx, readReq, readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", readResp.Diagnostics)
	}

	got := rawToInstanceHostname(ctx, t, sch, readResp.State.Raw)
	if got.ID.ValueString() != "host-1" || got.InstanceID.ValueString() != "inst-1" {
		t.Errorf("id/instance_id = %s/%s, want the import's seeds kept", got.ID.ValueString(), got.InstanceID.ValueString())
	}
	if got.Label.ValueString() != "velvety-otter-1234" || got.AccessDomainID.ValueString() != "dom-default" {
		t.Errorf("label/access_domain_id = %s/%s, want the list's row filled in",
			got.Label.ValueString(), got.AccessDomainID.ValueString())
	}
}

// The import id is exactly one slash separating two non-empty halves.
func TestInstanceHostnameImportBadShapes(t *testing.T) {
	cases := []struct {
		name string
		id   string
	}{
		{"no slash", "host-1"},
		{"two slashes", "a/b/c"},
		{"empty instance half", "/host-1"},
		{"empty row half", "inst-1/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r, sch := newSchemadInstanceHostname(ctx, t)

			f := newFakeAPI(t)
			r.pd = f.testClient()

			impReq := resource.ImportStateRequest{ID: tc.id}
			impResp := &resource.ImportStateResponse{
				State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
			}
			r.ImportState(ctx, impReq, impResp)

			assertError(t, &impResp.Diagnostics, "Invalid import id", "<instance_id>/<hostname_id>")
			if reqs := f.all(); len(reqs) != 0 {
				t.Errorf("requests = %+v, want none — a bad import id never reaches the wire", reqs)
			}
		})
	}
}

// The schema contract: the three identity attributes are Required +
// RequiresReplace (identity is create-only — every change replaces the
// row); id is Computed with no modifiers; no blocks, no validators,
// nothing sensitive.
func TestInstanceHostnameSchemaContract(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadInstanceHostname(ctx, t)

	want := []string{"instance_id", "access_domain_id", "label", "id"}
	if len(sch.Attributes) != len(want) {
		t.Fatalf("schema attributes = %d, want exactly %d (%v)", len(sch.Attributes), len(want), want)
	}
	for _, name := range want {
		if _, ok := sch.Attributes[name]; !ok {
			t.Errorf("schema missing attribute %q", name)
		}
	}

	for _, name := range []string{"instance_id", "access_domain_id", "label"} {
		attr, ok := sch.Attributes[name].(schema.StringAttribute)
		if !ok {
			t.Fatalf("%s is not a string attribute", name)
		}
		if !attr.IsRequired() {
			t.Errorf("%s is not Required", name)
		}
		if len(attr.PlanModifiers) != 1 {
			t.Fatalf("%s plan modifiers = %d, want exactly RequiresReplace", name, len(attr.PlanModifiers))
		}
		const requiresReplaceDescription = "If the value of this attribute changes, Terraform will destroy and recreate the resource."
		if got := attr.PlanModifiers[0].Description(ctx); got != requiresReplaceDescription {
			t.Errorf("%s modifier description = %q, want the RequiresReplace description", name, got)
		}
	}

	if !sch.Attributes["id"].IsComputed() || sch.Attributes["id"].IsRequired() {
		t.Error("id must be Computed-only")
	}
	if len(sch.Attributes["id"].(schema.StringAttribute).PlanModifiers) != 0 {
		t.Error("id carries plan modifiers — nothing here replaces in place")
	}
	if len(sch.Blocks) != 0 {
		t.Errorf("blocks = %v, want none (every verb is synchronous — no timeouts)", sch.Blocks)
	}

	md := &resource.MetadataResponse{}
	r.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: "fluxvale"}, md)
	if md.TypeName != "fluxvale_instance_hostname" {
		t.Errorf("TypeName = %q, want fluxvale_instance_hostname", md.TypeName)
	}
}
