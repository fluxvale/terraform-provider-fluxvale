package provider

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// --- resize (stopped-first, #493 / ADR-0043) -------------------------

// verbIndexes locates the first occurrence of each lifecycle verb in
// the recorded requests.
func verbIndexes(t *testing.T, reqs []recordedRequest, verbs ...string) []int {
	t.Helper()
	out := make([]int, len(verbs))
	for i := range out {
		out[i] = -1
	}
	for i, r := range reqs {
		for j, v := range verbs {
			if out[j] < 0 && strings.HasSuffix(r.path, v) && r.method == http.MethodPatch {
				out[j] = i
			}
		}
	}
	for j, v := range verbs {
		if out[j] < 0 {
			t.Fatalf("no PATCH %s among %+v", v, reqs)
		}
	}
	return out
}

// A running row: stop → resize → start (the guard refuses a running
// resize, never defers it), with the resize body carrying all three
// dims — cpu_cores as the decimal string.
func TestUpdateResizeRunningStopsFirst(t *testing.T) {
	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{script(200, runningDoc)}

	pd := f.testClient()
	plan := testInstance(func(d *instanceModel) {
		d.ID = types.StringValue("inst-1")
		d.CPUCores = types.Float64Value(2)
		d.MemoryMB = types.Int64Value(1024)
		d.StorageGB = types.Int64Value(2)
	})
	state := testInstance(func(d *instanceModel) { d.ID = types.StringValue("inst-1") })
	diags := &diag.Diagnostics{}
	if updateInstance(context.Background(), pd, &plan, &state, diags) {
		t.Fatal("updateInstance reported the instance gone")
	}
	assertNoErrors(t, diags)

	stopI, resizeI, startI := func() (int, int, int) {
		idx := verbIndexes(t, f.all(), "/stop", "/resize", "/start")
		return idx[0], idx[1], idx[2]
	}()
	if !(stopI < resizeI && resizeI < startI) {
		t.Errorf("sequence = stop@%d resize@%d start@%d, want stop → resize → start", stopI, resizeI, startI)
	}

	// The resize body: the plan's allocation, cpu_cores as the decimal
	// string.
	resize := findOne(t, f.all(), http.MethodPatch, "/resize")
	attrs := attrsOf(t, resize)
	if attrs["cpu_cores"] != "2" {
		t.Errorf("resize cpu_cores = %v (%T), want the decimal string \"2\"", attrs["cpu_cores"], attrs["cpu_cores"])
	}
	if attrs["memory_mb"] != 1024.0 || attrs["storage_gb"] != 2.0 {
		t.Errorf("resize memory/storage = %v/%v, want 1024/2", attrs["memory_mb"], attrs["storage_gb"])
	}
}

// Already stopped, desired posture stopped: just the resize — no
// stop/start churn. With a requested flip, the start lands after.
func TestUpdateResizeStopped(t *testing.T) {
	pd := func(doc string) (*fakeAPI, *diag.Diagnostics, *instanceModel, *instanceModel) {
		f := newFakeAPI(t)
		f.instanceGets = []scriptedResponse{script(200, doc)}
		plan := testInstance(func(d *instanceModel) {
			d.ID = types.StringValue("inst-1")
			d.MemoryMB = types.Int64Value(1024)
		})
		state := testInstance(func(d *instanceModel) { d.ID = types.StringValue("inst-1") })
		return f, &diag.Diagnostics{}, &plan, &state
	}

	t.Run("stays stopped when running is not desired", func(t *testing.T) {
		f, diags, plan, state := pd(stoppedDoc)
		plan.Running = types.BoolValue(false)
		state.Running = types.BoolValue(false)
		updateInstance(context.Background(), f.testClient(), plan, state, diags)
		assertNoErrors(t, diags)
		for _, r := range f.all() {
			if r.method == http.MethodPatch && (strings.HasSuffix(r.path, "/stop") || strings.HasSuffix(r.path, "/start")) {
				t.Errorf("unexpected %s — the row was already stopped and stays so", r.path)
			}
		}
		if findOne(t, f.all(), http.MethodPatch, "/resize").path == "" {
			t.Fatal("no resize PATCH")
		}
	})

	t.Run("starts when the flip was requested", func(t *testing.T) {
		f, diags, plan, state := pd(stoppedDoc)
		plan.Running = types.BoolValue(true)
		state.Running = types.BoolValue(false)
		updateInstance(context.Background(), f.testClient(), plan, state, diags)
		assertNoErrors(t, diags)
		resizeI, startI := func() (int, int) {
			idx := verbIndexes(t, f.all(), "/resize", "/start")
			return idx[0], idx[1]
		}()
		if resizeI > startI {
			t.Errorf("resize@%d after start@%d — the guard requires stopped-first", resizeI, startI)
		}
	})
}

// A sleeping row walks up through running (start is permitted from
// sleeping, stop is not) before it can resize.
func TestUpdateResizeSleepingWalksUpThroughRunning(t *testing.T) {
	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{
		script(200, statusDoc("sleeping")), // the precondition GET
		script(200, statusDoc("starting")), // the wait after start
		script(200, runningDoc),            // running — repeat-last
	}

	pd := f.testClient()
	plan := testInstance(func(d *instanceModel) {
		d.ID = types.StringValue("inst-1")
		d.MemoryMB = types.Int64Value(1024)
	})
	state := testInstance(func(d *instanceModel) { d.ID = types.StringValue("inst-1") })
	diags := &diag.Diagnostics{}
	updateInstance(context.Background(), pd, &plan, &state, diags)
	assertNoErrors(t, diags)

	startI, stopI, resizeI := func() (int, int, int) {
		idx := verbIndexes(t, f.all(), "/start", "/stop", "/resize")
		return idx[0], idx[1], idx[2]
	}()
	if !(startI < stopI && stopI < resizeI) {
		t.Errorf("sequence = start@%d stop@%d resize@%d, want start → stop → resize", startI, stopI, resizeI)
	}
}

func TestUpdateResizeRefusals(t *testing.T) {
	t.Run("pending can never be stopped — replace", func(t *testing.T) {
		f := newFakeAPI(t)
		f.instanceGets = []scriptedResponse{script(200, statusDoc("pending"))}
		pd := f.testClient()
		plan := testInstance(func(d *instanceModel) {
			d.ID = types.StringValue("inst-1")
			d.MemoryMB = types.Int64Value(1024)
		})
		state := testInstance(func(d *instanceModel) { d.ID = types.StringValue("inst-1") })
		diags := &diag.Diagnostics{}
		updateInstance(context.Background(), pd, &plan, &state, diags)
		assertError(t, diags, `"pending"`, "Replace it instead")
	})

	t.Run("transient status names the observed status", func(t *testing.T) {
		f := newFakeAPI(t)
		f.instanceGets = []scriptedResponse{script(200, statusDoc("deploying"))}
		pd := f.testClient()
		plan := testInstance(func(d *instanceModel) {
			d.ID = types.StringValue("inst-1")
			d.MemoryMB = types.Int64Value(1024)
		})
		state := testInstance(func(d *instanceModel) { d.ID = types.StringValue("inst-1") })
		diags := &diag.Diagnostics{}
		updateInstance(context.Background(), pd, &plan, &state, diags)
		assertError(t, diags, `"deploying"`, "requires stopped")
	})
}

// --- stop/start preconditions (finding 4) ----------------------------

// A platform-slept row is already below running: desired stop skips
// the PATCH instead of wedging on the guard's 400.
func TestUpdateStopSkipsSleptRow(t *testing.T) {
	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{script(200, statusDoc("sleeping"))}

	pd := f.testClient()
	plan := testInstance(func(d *instanceModel) {
		d.ID = types.StringValue("inst-1")
		d.Running = types.BoolValue(false)
	})
	state := testInstance(func(d *instanceModel) { d.ID = types.StringValue("inst-1") })
	diags := &diag.Diagnostics{}
	updateInstance(context.Background(), pd, &plan, &state, diags)
	assertNoErrors(t, diags)

	for _, r := range f.all() {
		if r.method == http.MethodPatch && strings.HasSuffix(r.path, "/stop") {
			t.Errorf("stop PATCH sent against a sleeping row — already satisfied")
		}
	}
}

func TestUpdateStopDiagnosticOnTransientStatus(t *testing.T) {
	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{script(200, statusDoc("starting"))}

	pd := f.testClient()
	plan := testInstance(func(d *instanceModel) {
		d.ID = types.StringValue("inst-1")
		d.Running = types.BoolValue(false)
	})
	state := testInstance(func(d *instanceModel) { d.ID = types.StringValue("inst-1") })
	diags := &diag.Diagnostics{}
	updateInstance(context.Background(), pd, &plan, &state, diags)
	assertError(t, diags, `"starting"`, "only stops a running row")
}

// A running row with desired start is satisfied — no PATCH.
func TestUpdateStartSkipsRunningRow(t *testing.T) {
	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{script(200, runningDoc)}

	pd := f.testClient()
	plan := testInstance(func(d *instanceModel) {
		d.ID = types.StringValue("inst-1")
		d.Running = types.BoolValue(true)
	})
	state := testInstance(func(d *instanceModel) {
		d.ID = types.StringValue("inst-1")
		d.Running = types.BoolValue(false)
	})
	diags := &diag.Diagnostics{}
	updateInstance(context.Background(), pd, &plan, &state, diags)
	assertNoErrors(t, diags)

	for _, r := range f.all() {
		if r.method == http.MethodPatch && strings.HasSuffix(r.path, "/start") {
			t.Error("start PATCH sent against a running row")
		}
	}
}

func TestUpdateStartDiagnosticOnErrorRow(t *testing.T) {
	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{script(200, errorDoc)}

	pd := f.testClient()
	plan := testInstance(func(d *instanceModel) {
		d.ID = types.StringValue("inst-1")
		d.Running = types.BoolValue(true)
	})
	state := testInstance(func(d *instanceModel) {
		d.ID = types.StringValue("inst-1")
		d.Running = types.BoolValue(false)
	})
	diags := &diag.Diagnostics{}
	updateInstance(context.Background(), pd, &plan, &state, diags)
	assertError(t, diags, `"error"`, "stopped or sleeping")
}

// --- :lost is terminal in the create wait (finding 6) ----------------

func TestCreateWaitFailsFastOnLost(t *testing.T) {
	f := newFakeAPI(t)
	f.orgsDoc = `{"data": [{"id": "org-1", "type": "organization", "attributes": {"slug": "acme", "name": "Acme"}}]}`
	f.postResponse = createResponse
	f.instanceGets = []scriptedResponse{script(200, statusDoc("lost"))}

	pd := f.testClient()
	data := testInstance()
	diags := &diag.Diagnostics{}
	createInstance(context.Background(), pd, &data, diags)
	assertError(t, diags, "did not reach running", `"lost"`)
}

// --- DELETE 404 is success (finding 7) -------------------------------

func TestDelete404OnDeleteIsSuccess(t *testing.T) {
	f := newFakeAPI(t)
	f.deleteStatus = http.StatusNotFound

	pd := f.testClient()
	diags := &diag.Diagnostics{}
	deleteInstance(context.Background(), pd, types.StringValue("inst-1"), diags)
	assertNoErrors(t, diags)

	for _, r := range f.all() {
		if r.method == http.MethodGet {
			t.Error("GET after a 404 DELETE — already gone, nothing to wait for")
		}
	}
}

// --- update PATCH-error branch surfaces the server's wording ---------

func TestUpdatePatchErrorSurfaces(t *testing.T) {
	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{script(200, runningDoc)}
	f.failPatch = "/stop"
	f.failPatchStatus = http.StatusForbidden

	pd := f.testClient()
	plan := testInstance(func(d *instanceModel) {
		d.ID = types.StringValue("inst-1")
		d.Running = types.BoolValue(false)
	})
	state := testInstance(func(d *instanceModel) { d.ID = types.StringValue("inst-1") })
	diags := &diag.Diagnostics{}
	updateInstance(context.Background(), pd, &plan, &state, diags)
	assertError(t, diags, "Unable to stop", "missing the required scope")
}

// --- resolve helper: the poll pacing helper itself -------------------

func TestWaitInstanceStatusFailFast(t *testing.T) {
	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{script(200, statusDoc("lost"))}

	pd := f.testClient()
	pd.pollTimeout = 20 * time.Millisecond
	last, err := waitInstanceStatus(context.Background(), pd, "inst-1", "running")
	if err == nil || !strings.Contains(err.Error(), `"lost"`) {
		t.Fatalf("err = %v, want the lost fail-fast", err)
	}
	if last != "lost" {
		t.Errorf("last status = %q, want lost", last)
	}
}

// --- framework plumbing: state-tainting + import ---------------------

// A post-POST failure must leave TRACKED state (id + planned values),
// not an invisible billing instance the next apply would duplicate.
func TestCreateWrapperTaintsStateOnPostPOSTFailure(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadResource(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = `{"data": [{"id": "org-1", "type": "organization", "attributes": {"slug": "acme", "name": "Acme"}}]}`
	f.postResponse = createResponse
	f.failPatch = "/deploy"
	f.failPatchStatus = http.StatusForbidden
	r.pd = f.testClient()

	planData := testInstance(func(d *instanceModel) {
		d.EnvVars = envVarInput(t, map[string]string{"API_KEY": "sekrit"})
	})
	req := resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: modelToRaw(ctx, t, sch, &planData)},
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: nullObject(ctx, t, sch.Type().TerraformType(ctx))},
	}
	r.Create(ctx, req, resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("deploy PATCH failure: no error diagnostic")
	}
	if resp.State.Raw.IsNull() {
		t.Fatal("state left empty after a post-POST failure — the instance is untracked (next apply creates a second one)")
	}

	got := rawToModel(ctx, t, sch, resp.State.Raw)
	if got.ID.ValueString() != "inst-1" {
		t.Errorf("tainted state id = %q, want inst-1", got.ID.ValueString())
	}
	// What the create response carried is preserved; the not-yet-filled
	// computed fields are null, never unknown (unknown can't ride state).
	if got.Slug.ValueString() != "velvety-otter-1234" {
		t.Errorf("tainted state slug = %q, want the create response's", got.Slug.ValueString())
	}
	if got.Namespace.IsUnknown() || got.Status.IsUnknown() || got.AppVersionID.IsUnknown() {
		t.Errorf("unknown values leaked into state: namespace=%v status=%v app_version_id=%v",
			got.Namespace, got.Status, got.AppVersionID)
	}
	if envMapOf(t, got.EnvVars)["API_KEY"] != "sekrit" {
		t.Errorf("tainted state env_vars = %v, want the configured map", envMapOf(t, got.EnvVars))
	}
}

// A pre-POST failure (org resolution) must NOT write state — nothing
// was created.
func TestCreateWrapperLeavesStateEmptyOnPrePOSTFailure(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadResource(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = `{"data": []}` // no organizations — resolution fails pre-POST
	r.pd = f.testClient()

	planData := testInstance()
	req := resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: modelToRaw(ctx, t, sch, &planData)},
	}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: nullObject(ctx, t, sch.Type().TerraformType(ctx))},
	}
	r.Create(ctx, req, resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("org resolution failure: no error diagnostic")
	}
	if !resp.State.Raw.IsNull() {
		t.Errorf("state written on a pre-POST failure: %v — nothing was created", resp.State.Raw)
	}
}

// Update over a row deleted between refresh and apply removes it from
// state instead of reporting a clean apply on a ghost.
func TestUpdateWrapperRemovesOn404(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadResource(ctx, t)

	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{script(http.StatusNotFound, `{"errors": [{"title": "Not Found"}]}`)}
	r.pd = f.testClient()

	planData := testInstance(func(d *instanceModel) { d.ID = types.StringValue("gone") })
	stateData := planData
	req := resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: sch, Raw: modelToRaw(ctx, t, sch, &planData)},
		State: tfsdk.State{Schema: sch, Raw: modelToRaw(ctx, t, sch, &stateData)},
	}
	resp := &resource.UpdateResponse{
		State: tfsdk.State{Schema: sch, Raw: modelToRaw(ctx, t, sch, &stateData)},
	}
	r.Update(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsNull() {
		t.Errorf("state after a 404 update = %v, want null (removed)", resp.State.Raw)
	}
}

// Update through the wrapper — the start flip lands the PATCH and
// writes the refreshed state.
func TestUpdateWrapperStartFlip(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadResource(ctx, t)

	f := newFakeAPI(t)
	// The precondition GET sees stopped; the final read sees running.
	f.instanceGets = []scriptedResponse{
		script(200, stoppedDoc),
		script(200, runningDoc),
	}
	r.pd = f.testClient()

	planData := testInstance(func(d *instanceModel) {
		d.ID = types.StringValue("inst-1")
		d.Running = types.BoolValue(true)
	})
	stateData := testInstance(func(d *instanceModel) {
		d.ID = types.StringValue("inst-1")
		d.Running = types.BoolValue(false)
	})
	req := resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: sch, Raw: modelToRaw(ctx, t, sch, &planData)},
		State: tfsdk.State{Schema: sch, Raw: modelToRaw(ctx, t, sch, &stateData)},
	}
	resp := &resource.UpdateResponse{
		State: tfsdk.State{Schema: sch, Raw: modelToRaw(ctx, t, sch, &stateData)},
	}
	r.Update(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", resp.Diagnostics)
	}

	// The start PATCH fired, and state refreshed from the wire.
	findOne(t, f.all(), http.MethodPatch, "/start")
	got := rawToModel(ctx, t, sch, resp.State.Raw)
	if got.Status.ValueString() != "running" {
		t.Errorf("status after start = %q, want the wire's running", got.Status.ValueString())
	}
	if got.Running.ValueBool() != true {
		t.Errorf("running in state = %v, want the plan's true", got.Running)
	}
}

// A PATCH failure through the wrapper: error diagnostic, prior state
// left standing (terraform keeps the last-known state on apply error).
func TestUpdateWrapperPatchErrorKeepsState(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadResource(ctx, t)

	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{script(200, runningDoc)}
	f.failPatch = "/stop"
	f.failPatchStatus = http.StatusForbidden
	r.pd = f.testClient()

	planData := testInstance(func(d *instanceModel) {
		d.ID = types.StringValue("inst-1")
		d.Running = types.BoolValue(false)
	})
	stateData := testInstance(func(d *instanceModel) {
		d.ID = types.StringValue("inst-1")
		d.Status = types.StringValue("running")
	})
	req := resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: sch, Raw: modelToRaw(ctx, t, sch, &planData)},
		State: tfsdk.State{Schema: sch, Raw: modelToRaw(ctx, t, sch, &stateData)},
	}
	resp := &resource.UpdateResponse{
		State: tfsdk.State{Schema: sch, Raw: modelToRaw(ctx, t, sch, &stateData)},
	}
	r.Update(ctx, req, resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("stop PATCH failure: no error diagnostic")
	}
	// State untouched — still the prior status, not the plan.
	got := rawToModel(ctx, t, sch, resp.State.Raw)
	if got.Status.ValueString() != "running" {
		t.Errorf("state status after failed update = %q, want the prior running", got.Status.ValueString())
	}
}

// Import converges on the FK ids the wire echoes: ImportState sets the
// id, Read fills organization_id/app_version_id from the sparse
// fieldset.
func TestImportConvergesOnFKIDs(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadResource(ctx, t)

	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{script(200, runningDoc)}
	r.pd = f.testClient()

	importReq := resource.ImportStateRequest{ID: "inst-1"}
	importResp := &resource.ImportStateResponse{
		State: tfsdk.State{Schema: sch, Raw: nullObject(ctx, t, sch.Type().TerraformType(ctx))},
	}
	r.ImportState(ctx, importReq, importResp)
	if importResp.Diagnostics.HasError() {
		t.Fatalf("ImportState: %v", importResp.Diagnostics)
	}

	readReq := resource.ReadRequest{State: importResp.State}
	readResp := &resource.ReadResponse{State: importResp.State}
	r.Read(ctx, readReq, readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("Read after import: %v", readResp.Diagnostics)
	}

	got := rawToModel(ctx, t, sch, readResp.State.Raw)
	if got.OrganizationID.ValueString() != "org-1" {
		t.Errorf("organization_id = %q, want org-1 from the sparse fieldset", got.OrganizationID.ValueString())
	}
	if got.AppVersionID.ValueString() != "ver-1" {
		t.Errorf("app_version_id = %q, want ver-1 from the sparse fieldset", got.AppVersionID.ValueString())
	}
	// The wire does NOT echo these — they stay null after import.
	if !got.EnvVars.IsNull() {
		t.Errorf("env_vars after import = %v, want null (write-only)", got.EnvVars)
	}
	if !got.App.IsNull() || !got.AppVersion.IsNull() {
		t.Errorf("app/app_version after import = %v/%v, want null (no wire echo)", got.App, got.AppVersion)
	}
}
