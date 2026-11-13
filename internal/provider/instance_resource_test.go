package provider

import (
	"context"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// --- create ----------------------------------------------------------

func TestCreateHappyPathBodyShapeAndState(t *testing.T) {
	f := newFakeAPI(t)
	f.orgsDoc = `{"data": [{"id": "org-1", "type": "organization", "attributes": {"slug": "acme", "name": "Acme"}}]}`
	f.postResponse = createResponse
	f.instanceGets = []scriptedResponse{script(200, runningDoc)}

	pd := f.testClient()
	data := testInstance(func(d *instanceModel) {
		d.EnvVars = envVarInput(t, map[string]string{"API_KEY": "sekrit"})
	})
	diags := &diag.Diagnostics{}
	createInstance(context.Background(), pd, &data, diags)
	assertNoErrors(t, diags)

	reqs := f.all()

	// The POST: bearer auth, attributes placement, decimal-as-string,
	// sole-org default resolved.
	post := findOne(t, reqs, http.MethodPost, "/api/v1/instances")
	if post.auth != "Bearer fvpat_test" {
		t.Errorf("POST Authorization = %q, want the bearer token", post.auth)
	}
	attrs := attrsOf(t, post)
	if attrs["name"] != "Blog" {
		t.Errorf("attributes.name = %v, want Blog", attrs["name"])
	}
	if attrs["cpu_cores"] != "0.5" {
		t.Errorf("attributes.cpu_cores = %v (%T), want the decimal string \"0.5\"", attrs["cpu_cores"], attrs["cpu_cores"])
	}
	if attrs["memory_mb"] != 256.0 || attrs["storage_gb"] != 1.0 {
		t.Errorf("memory/storage = %v/%v, want 256/1", attrs["memory_mb"], attrs["storage_gb"])
	}
	if attrs["app_version_id"] != "ver-1" {
		t.Errorf("app_version_id = %v, want ver-1 (an attribute, not a relationship)", attrs["app_version_id"])
	}
	if attrs["organization_id"] != "org-1" {
		t.Errorf("organization_id = %v, want the sole org resolved (org-1)", attrs["organization_id"])
	}
	if envVars, ok := attrs["env_vars"].(map[string]any); !ok || envVars["API_KEY"] != "sekrit" {
		t.Errorf("env_vars = %v, want the write-only argument's map", attrs["env_vars"])
	}
	if _, ok := attrs["always_on"]; ok {
		t.Error("always_on sent at create — not in the action's accept list; it rides PATCH always-on")
	}
	if _, ok := attrs["exposure"]; ok {
		t.Error("exposure sent at create — not in the action's accept list; it rides PATCH exposure")
	}

	// Then the deploy PATCH — create lands :pending, deploying is a
	// separate verb.
	deploy := findOne(t, reqs, http.MethodPatch, "/api/v1/instances/inst-1/deploy")
	attrsOf(t, deploy) // empty attributes object, presence is the assert

	// No stop / always-on / exposure PATCHes for the defaults.
	for _, r := range reqs {
		for _, verb := range []string{"/stop", "/start", "/always-on", "/exposure"} {
			if strings.HasSuffix(r.path, verb) {
				t.Errorf("unexpected %s — defaults need no PATCH", r.path)
			}
		}
	}

	// The model carries the detail surface.
	if data.ID.ValueString() != "inst-1" || data.Slug.ValueString() != "velvety-otter-1234" {
		t.Errorf("id/slug = %s/%s, want inst-1/velvety-otter-1234", data.ID.ValueString(), data.Slug.ValueString())
	}
	if data.Status.ValueString() != "running" {
		t.Errorf("status = %s, want running after the wait", data.Status.ValueString())
	}
	if data.Namespace.ValueString() != "fluxvale-app-inst-1" {
		t.Errorf("namespace = %s, want the deployed namespace", data.Namespace.ValueString())
	}
	if !data.Port.IsNull() && data.Port.ValueInt64() != 3000 {
		t.Errorf("port = %d, want 3000", data.Port.ValueInt64())
	}
	if data.Image.ValueString() != "reg/app:1.0" {
		t.Errorf("image = %s, want the blueprint image", data.Image.ValueString())
	}
	if data.DeployedAt.IsNull() {
		t.Error("deployed_at null after deploy")
	}

	// env_vars is kept verbatim — the response carries no env_vars and
	// the model must not lose the configured value.
	if got := envMapOf(t, data.EnvVars); got["API_KEY"] != "sekrit" {
		t.Errorf("env_vars in state = %v, want the configured map kept", got)
	}
}

// The two identity forms: explicit app_version_id wins over slugs
// (no catalog calls at all).
func TestCreatePrefersExplicitAppVersionID(t *testing.T) {
	f := newFakeAPI(t)
	f.orgsDoc = `{"data": [{"id": "org-1", "type": "organization", "attributes": {"slug": "acme", "name": "Acme"}}]}`
	f.postResponse = createResponse
	f.instanceGets = []scriptedResponse{script(200, runningDoc)}
	f.appDoc = `{"errors": [{"title": "Not Found"}]}` // a catalog hit would fail the create

	pd := f.testClient()
	data := testInstance(func(d *instanceModel) {
		d.App = types.StringValue("forgejo")
		d.AppVersion = types.StringValue("1.0")
	})
	diags := &diag.Diagnostics{}
	createInstance(context.Background(), pd, &data, diags)
	assertNoErrors(t, diags)

	attrs := attrsOf(t, findOne(t, f.all(), http.MethodPost, "/api/v1/instances"))
	if attrs["app_version_id"] != "ver-1" {
		t.Errorf("app_version_id = %v, want the explicit id over the slug form", attrs["app_version_id"])
	}
}

// The slug form resolves through the catalog: GET /apps/:slug then
// GET /apps/:id/versions.
func TestCreateResolvesAppSlugAndVersion(t *testing.T) {
	f := newFakeAPI(t)
	f.orgsDoc = `{"data": [{"id": "org-1", "type": "organization", "attributes": {"slug": "acme", "name": "Acme"}}]}`
	f.appDoc = appDoc
	f.versionsDoc = versionsDoc
	f.postResponse = createResponse
	f.instanceGets = []scriptedResponse{script(200, runningDoc)}

	pd := f.testClient()
	data := testInstance(func(d *instanceModel) {
		d.AppVersionID = types.StringNull()
		d.App = types.StringValue("forgejo")
		d.AppVersion = types.StringValue("1.0")
	})
	diags := &diag.Diagnostics{}
	createInstance(context.Background(), pd, &data, diags)
	assertNoErrors(t, diags)

	reqs := f.all()
	if findOne(t, reqs, http.MethodGet, "/api/v1/apps/forgejo").path == "" {
		t.Fatal("no GET /apps/forgejo — the slug was not resolved through the catalog")
	}
	if findOne(t, reqs, http.MethodGet, "/api/v1/apps/app-1/versions").path == "" {
		t.Fatal("no GET /apps/app-1/versions")
	}
	attrs := attrsOf(t, findOne(t, reqs, http.MethodPost, "/api/v1/instances"))
	if attrs["app_version_id"] != "ver-1" {
		t.Errorf("app_version_id = %v, want ver-1 resolved from app forgejo + version 1.0", attrs["app_version_id"])
	}
}

func TestCreateSlugResolutionErrors(t *testing.T) {
	t.Run("neither form", func(t *testing.T) {
		f := newFakeAPI(t)
		pd := f.testClient()
		data := testInstance(func(d *instanceModel) { d.AppVersionID = types.StringNull() })
		diags := &diag.Diagnostics{}
		createInstance(context.Background(), pd, &data, diags)
		assertError(t, diags, "app_version_id", "app and app_version")
	})

	t.Run("unknown slug is the server's 404", func(t *testing.T) {
		f := newFakeAPI(t)
		f.appStatus = http.StatusNotFound
		f.appDoc = `{"errors": [{"title": "Not Found", "detail": "no app nope"}]}`
		pd := f.testClient()
		data := testInstance(func(d *instanceModel) {
			d.AppVersionID = types.StringNull()
			d.App = types.StringValue("nope")
			d.AppVersion = types.StringValue("1.0")
		})
		diags := &diag.Diagnostics{}
		createInstance(context.Background(), pd, &data, diags)
		assertError(t, diags, "looking up app", "no app nope")
	})

	t.Run("unknown version names the available ones", func(t *testing.T) {
		f := newFakeAPI(t)
		f.appDoc = appDoc
		f.versionsDoc = versionsDoc
		pd := f.testClient()
		data := testInstance(func(d *instanceModel) {
			d.AppVersionID = types.StringNull()
			d.App = types.StringValue("forgejo")
			d.AppVersion = types.StringValue("9.9")
		})
		diags := &diag.Diagnostics{}
		createInstance(context.Background(), pd, &data, diags)
		assertError(t, diags, `no version "9.9"`, "1.0", "2.0")
	})
}

// running=false: the stop PATCH lands only after the row is running
// (the verb refuses anything else), and the state reflects stopped.
func TestCreateThenStop(t *testing.T) {
	f := newFakeAPI(t)
	f.orgsDoc = `{"data": [{"id": "org-1", "type": "organization", "attributes": {"slug": "acme", "name": "Acme"}}]}`
	f.postResponse = createResponse
	f.instanceGets = []scriptedResponse{
		script(200, runningDoc),
		script(200, stoppedDoc),
	}

	pd := f.testClient()
	data := testInstance(func(d *instanceModel) { d.Running = types.BoolValue(false) })
	diags := &diag.Diagnostics{}
	createInstance(context.Background(), pd, &data, diags)
	assertNoErrors(t, diags)

	reqs := f.all()
	deploy := findOne(t, reqs, http.MethodPatch, "/deploy")
	stop := findOne(t, reqs, http.MethodPatch, "/stop")
	_ = deploy

	// Order matters: stop after the deploy PATCH (and the wait saw
	// running first — the stop verb's own precondition).
	firstStop := -1
	firstDeploy := -1
	for i, r := range reqs {
		if strings.HasSuffix(r.path, "/stop") && firstStop < 0 {
			firstStop = i
		}
		if strings.HasSuffix(r.path, "/deploy") && firstDeploy < 0 {
			firstDeploy = i
		}
	}
	if firstStop <= firstDeploy {
		t.Errorf("stop (index %d) must follow deploy (index %d): the verb needs a running row", firstStop, firstDeploy)
	}
	attrs := attrsOf(t, stop)
	if len(attrs) != 0 {
		t.Errorf("stop attributes = %v, want empty", attrs)
	}
	if data.Status.ValueString() != "stopped" {
		t.Errorf("status = %s, want stopped", data.Status.ValueString())
	}
}

// A null map element is a config mistake, not "": the create fails
// before the POST, naming the key.
func TestCreateRejectsNullEnvValue(t *testing.T) {
	f := newFakeAPI(t)
	f.orgsDoc = `{"data": [{"id": "org-1", "type": "organization", "attributes": {"slug": "acme", "name": "Acme"}}]}`

	pd := f.testClient()
	data := testInstance(func(d *instanceModel) {
		d.EnvVars = types.MapValueMust(types.StringType, map[string]attr.Value{
			"KEY": types.StringNull(),
		})
	})
	diags := &diag.Diagnostics{}
	createInstance(context.Background(), pd, &data, diags)
	assertError(t, diags,
		`env_vars["KEY"] is null`,
		"remove the key instead",
		"an empty string is a value",
	)
	for _, r := range f.all() {
		if r.method == http.MethodPost {
			t.Errorf("unexpected POST %s after a null env element", r.path)
		}
	}
}

// always_on/exposure are not create inputs — the action's accept list
// is name/cpu_cores/memory_mb/storage_gb/env_vars/app_version_id — so
// a non-default posture rides its PATCH verb after the deploy PATCH.
func TestCreateAppliesAlwaysOnAndExposureViaPatch(t *testing.T) {
	f := newFakeAPI(t)
	f.orgsDoc = `{"data": [{"id": "org-1", "type": "organization", "attributes": {"slug": "acme", "name": "Acme"}}]}`
	f.postResponse = createResponse
	f.instanceGets = []scriptedResponse{script(200, runningDoc)}

	pd := f.testClient()
	data := testInstance(func(d *instanceModel) {
		d.AlwaysOn = types.BoolValue(true)
		d.Exposure = types.StringValue("private")
	})
	diags := &diag.Diagnostics{}
	createInstance(context.Background(), pd, &data, diags)
	assertNoErrors(t, diags)
	reqs := f.all()

	// always_on/exposure go BEFORE the deploy PATCH — on a :pending row
	// they're attribute-only writes, and the deploy job converges on the
	// final posture (racing a live K8s apply is what the old order did).
	exposureIdx := -1
	alwaysOnIdx := -1
	deployIdx := -1
	for i, r := range reqs {
		switch {
		case strings.HasSuffix(r.path, "/exposure") && exposureIdx < 0:
			exposureIdx = i
		case strings.HasSuffix(r.path, "/always-on") && alwaysOnIdx < 0:
			alwaysOnIdx = i
		case strings.HasSuffix(r.path, "/deploy") && deployIdx < 0:
			deployIdx = i
		}
	}
	if exposureIdx < 0 || alwaysOnIdx < 0 || deployIdx < 0 {
		t.Fatalf("missing PATCHes: exposure=%d always-on=%d deploy=%d", exposureIdx, alwaysOnIdx, deployIdx)
	}
	if exposureIdx > deployIdx || alwaysOnIdx > deployIdx {
		t.Errorf("exposure (%d) and always-on (%d) must precede deploy (%d) — pre-deploy they're attribute-only",
			exposureIdx, alwaysOnIdx, deployIdx)
	}

	alwaysOn := attrsOf(t, reqs[alwaysOnIdx])
	if alwaysOn["always_on"] != true {
		t.Errorf("always-on attributes = %v, want always_on true", alwaysOn)
	}
	exposure := attrsOf(t, reqs[exposureIdx])
	if exposure["exposure"] != "private" {
		t.Errorf("exposure attributes = %v, want exposure private", exposure)
	}
}

// The deploy job failing: the terminal error status surfaces as a
// diagnostic carrying the status string — status_message is
// admin-hidden, so the status IS the whole story.
func TestCreateWaitSurfacesErrorStatus(t *testing.T) {
	f := newFakeAPI(t)
	f.orgsDoc = `{"data": [{"id": "org-1", "type": "organization", "attributes": {"slug": "acme", "name": "Acme"}}]}`
	f.postResponse = createResponse
	f.instanceGets = []scriptedResponse{script(200, errorDoc)}

	pd := f.testClient()
	data := testInstance()
	diags := &diag.Diagnostics{}
	createInstance(context.Background(), pd, &data, diags)
	assertError(t, diags, "did not reach running", `"error"`, "admin")
}

func TestCreateWaitTimeoutNamesLastStatus(t *testing.T) {
	f := newFakeAPI(t)
	f.orgsDoc = `{"data": [{"id": "org-1", "type": "organization", "attributes": {"slug": "acme", "name": "Acme"}}]}`
	f.postResponse = createResponse
	// A deploying row that never converges — the scripted last entry
	// repeats.
	f.instanceGets = []scriptedResponse{script(200, strings.ReplaceAll(runningDoc, `"running"`, `"deploying"`))}

	pd := f.testClient()
	pd.pollTimeout = 20 * time.Millisecond

	data := testInstance()
	diags := &diag.Diagnostics{}
	createInstance(context.Background(), pd, &data, diags)
	assertError(t, diags, "timed out", `"deploying"`)
}

// --- update ----------------------------------------------------------

func TestUpdateFlipsRunningAlwaysOnExposure(t *testing.T) {
	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{script(200, runningDoc)}

	pd := f.testClient()
	plan := testInstance(func(d *instanceModel) {
		d.ID = types.StringValue("inst-1")
		d.Running = types.BoolValue(false)
		d.AlwaysOn = types.BoolValue(true)
		d.Exposure = types.StringValue("private")
	})
	// Prior state: running (default), always_on false, exposure public —
	// everything flips.
	state := testInstance(func(d *instanceModel) {
		d.ID = types.StringValue("inst-1")
		d.AlwaysOn = types.BoolValue(false)
		d.Exposure = types.StringValue("public")
	})
	diags := &diag.Diagnostics{}
	updateInstance(context.Background(), pd, &plan, &state, diags)
	assertNoErrors(t, diags)

	reqs := f.all()
	stop := attrsOf(t, findOne(t, reqs, http.MethodPatch, "/stop"))
	if len(stop) != 0 {
		t.Errorf("stop attributes = %v, want empty", stop)
	}
	if alwaysOn := attrsOf(t, findOne(t, reqs, http.MethodPatch, "/always-on"))["always_on"]; alwaysOn != true {
		t.Errorf("always-on attributes = %v, want always_on true", alwaysOn)
	}
	if exposure := attrsOf(t, findOne(t, reqs, http.MethodPatch, "/exposure"))["exposure"]; exposure != "private" {
		t.Errorf("exposure attributes = %v, want private", exposure)
	}
}

// No posture change ⇒ no PATCH at all (an update that changes nothing
// must not send stop/start churn).
func TestUpdateNoChangeSendsNoPatch(t *testing.T) {
	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{script(200, runningDoc)}

	pd := f.testClient()
	plan := testInstance(func(d *instanceModel) {
		d.ID = types.StringValue("inst-1")
		d.AlwaysOn = types.BoolValue(false)
		d.Exposure = types.StringValue("public")
	})
	state := testInstance(func(d *instanceModel) {
		d.ID = types.StringValue("inst-1")
		d.AlwaysOn = types.BoolValue(false)
		d.Exposure = types.StringValue("public")
	})
	diags := &diag.Diagnostics{}
	updateInstance(context.Background(), pd, &plan, &state, diags)
	assertNoErrors(t, diags)

	for _, r := range f.all() {
		if r.method == http.MethodPatch {
			t.Errorf("unexpected PATCH %s with no posture change", r.path)
		}
	}
}

// --- env merge (#731, ADR-0052) --------------------------------------

// assertRequestList pins the WHOLE recorded traffic — the proof that an
// env-only update sends no POST/DELETE and no other verb.
func assertRequestList(t *testing.T, reqs []recordedRequest, want ...string) {
	t.Helper()
	got := make([]string, len(reqs))
	for i, r := range reqs {
		got[i] = r.method + " " + r.path
	}
	want = append([]string{}, want...)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("recorded requests = %v, want exactly %v", got, want)
	}
}

// assertEnvMerge decodes one PATCH /env body and compares its diff.
func assertEnvMerge(t *testing.T, r recordedRequest, wantSet map[string]string, wantRemove []string) {
	t.Helper()
	attrs := attrsOf(t, r)

	set, ok := attrs["set"].(map[string]any)
	if !ok {
		t.Fatalf("%s %s: set = %#v, want an object", r.method, r.path, attrs["set"])
	}
	gotSet := make(map[string]string, len(set))
	for k, v := range set {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("%s %s: set[%q] = %T, want string", r.method, r.path, k, v)
		}
		gotSet[k] = s
	}
	if !reflect.DeepEqual(gotSet, wantSet) {
		t.Errorf("%s %s: set = %v, want %v", r.method, r.path, gotSet, wantSet)
	}

	rawRemove, ok := attrs["remove"].([]any)
	if !ok {
		t.Fatalf("%s %s: remove = %#v, want an array", r.method, r.path, attrs["remove"])
	}
	gotRemove := make([]string, 0, len(rawRemove))
	for _, v := range rawRemove {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("%s %s: remove element = %T, want string", r.method, r.path, v)
		}
		gotRemove = append(gotRemove, s)
	}
	sort.Strings(gotRemove)
	want := append([]string{}, wantRemove...)
	sort.Strings(want)
	if !reflect.DeepEqual(gotRemove, want) {
		t.Errorf("%s %s: remove = %v, want %v", r.method, r.path, gotRemove, want)
	}
}

// envBase is an instance whose non-env attributes are already at their
// plan values — an env-only update must send only the merge PATCH (and
// the final read).
func envBase(t *testing.T, env map[string]string) instanceModel {
	return testInstance(func(d *instanceModel) {
		d.ID = types.StringValue("inst-1")
		d.EnvVars = envVarInput(t, env)
		d.AlwaysOn = types.BoolValue(false)
		d.Exposure = types.StringValue("public")
	})
}

// An env-only change is ONE PATCH /instances/:id/env — no POST, no
// DELETE, no replace, no posture PATCH.
func TestUpdateEnvOnlySendsOneMergePatch(t *testing.T) {
	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{script(200, runningDoc)}

	state := envBase(t, map[string]string{"API_KEY": "old", "KEEP": "same"})
	plan := state
	plan.EnvVars = envVarInput(t, map[string]string{"API_KEY": "new", "KEEP": "same", "ADDED": "yes"})

	diags := &diag.Diagnostics{}
	if updateInstance(context.Background(), f.testClient(), &plan, &state, diags) {
		t.Fatal("updateInstance reported the instance gone")
	}
	assertNoErrors(t, diags)

	reqs := f.all()
	assertRequestList(t, reqs,
		"PATCH /api/v1/instances/inst-1/env",
		"GET /api/v1/instances/inst-1",
	)
	assertEnvMerge(t, reqs[0],
		map[string]string{"API_KEY": "new", "ADDED": "yes"},
		nil,
	)

	// The id never changes — the merge is in-place, and the final read
	// keeps the same row.
	if plan.ID.ValueString() != "inst-1" {
		t.Errorf("id after env update = %q, want inst-1", plan.ID.ValueString())
	}
}

// Added + changed + removed in one apply is one PATCH with the exact
// three-way diff.
func TestUpdateEnvDiffAddedChangedRemoved(t *testing.T) {
	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{script(200, runningDoc)}

	state := envBase(t, map[string]string{"KEEP": "same", "CHANGE": "old", "DROP": "bye"})
	plan := state
	plan.EnvVars = envVarInput(t, map[string]string{"KEEP": "same", "CHANGE": "new", "ADDED": "yes"})

	diags := &diag.Diagnostics{}
	updateInstance(context.Background(), f.testClient(), &plan, &state, diags)
	assertNoErrors(t, diags)

	reqs := f.all()
	assertRequestList(t, reqs,
		"PATCH /api/v1/instances/inst-1/env",
		"GET /api/v1/instances/inst-1",
	)
	assertEnvMerge(t, reqs[0],
		map[string]string{"CHANGE": "new", "ADDED": "yes"},
		[]string{"DROP"},
	)
}

// A null plan map means "no user env": every key this resource
// previously managed is removed, and only that one PATCH is sent.
func TestUpdateEnvNullPlanRemovesEveryManagedKey(t *testing.T) {
	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{script(200, runningDoc)}

	state := envBase(t, map[string]string{"ALPHA": "1", "BETA": "2"})
	plan := state
	plan.EnvVars = types.MapNull(types.StringType)

	diags := &diag.Diagnostics{}
	updateInstance(context.Background(), f.testClient(), &plan, &state, diags)
	assertNoErrors(t, diags)

	reqs := f.all()
	assertRequestList(t, reqs,
		"PATCH /api/v1/instances/inst-1/env",
		"GET /api/v1/instances/inst-1",
	)
	assertEnvMerge(t, reqs[0], map[string]string{}, []string{"ALPHA", "BETA"})
}

// An unchanged map sends no merge PATCH at all.
func TestUpdateEnvUnchangedSendsNoPatch(t *testing.T) {
	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{script(200, runningDoc)}

	env := map[string]string{"API_KEY": "same", "OTHER": "also"}
	state := envBase(t, env)
	plan := state

	diags := &diag.Diagnostics{}
	updateInstance(context.Background(), f.testClient(), &plan, &state, diags)
	assertNoErrors(t, diags)

	assertRequestList(t, f.all(), "GET /api/v1/instances/inst-1")
}

// A null map element is an error naming the key — never send "" and
// never fall back to a remove.
func TestUpdateEnvNullElementIsAnError(t *testing.T) {
	f := newFakeAPI(t)

	state := envBase(t, map[string]string{"KEY": "value"})
	plan := state
	plan.EnvVars = types.MapValueMust(types.StringType, map[string]attr.Value{
		"KEY": types.StringNull(),
	})

	diags := &diag.Diagnostics{}
	if updateInstance(context.Background(), f.testClient(), &plan, &state, diags) {
		t.Fatal("updateInstance reported the instance gone")
	}
	assertError(t, diags,
		`env_vars["KEY"] is null`,
		"remove the key instead",
		"an empty string is a value",
	)
	assertRequestList(t, f.all()) // no merge PATCH, no read
}

// An empty string is a value, not a null/removal.
func TestUpdateEnvEmptyStringIsAValue(t *testing.T) {
	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{script(200, runningDoc)}

	state := envBase(t, map[string]string{"KEY": "old"})
	plan := state
	plan.EnvVars = envVarInput(t, map[string]string{"KEY": ""})

	diags := &diag.Diagnostics{}
	updateInstance(context.Background(), f.testClient(), &plan, &state, diags)
	assertNoErrors(t, diags)

	reqs := f.all()
	assertRequestList(t, reqs,
		"PATCH /api/v1/instances/inst-1/env",
		"GET /api/v1/instances/inst-1",
	)
	assertEnvMerge(t, reqs[0], map[string]string{"KEY": ""}, nil)
}

// No status gate: an error/pending/lost row still gets the merge PATCH
// — the server owns the status matrix (row-only there, live Secret on
// running/starting, refusals on deploying/restoring/deleting).
func TestUpdateEnvDoesNotGateOnStatus(t *testing.T) {
	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{script(200, errorDoc)}

	state := envBase(t, map[string]string{"API_KEY": "old"})
	plan := state
	plan.EnvVars = envVarInput(t, map[string]string{"API_KEY": "new"})

	diags := &diag.Diagnostics{}
	updateInstance(context.Background(), f.testClient(), &plan, &state, diags)
	assertNoErrors(t, diags)

	reqs := f.all()
	assertRequestList(t, reqs,
		"PATCH /api/v1/instances/inst-1/env",
		"GET /api/v1/instances/inst-1",
	)
	assertEnvMerge(t, reqs[0], map[string]string{"API_KEY": "new"}, nil)
}

// The server's 400 copy surfaces verbatim under the env diagnostic, and
// nothing after the failed merge is attempted.
func TestUpdateEnvPatchErrorSurfacesVerbatim(t *testing.T) {
	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{script(200, runningDoc)}
	f.failEnvPatchStatus = http.StatusBadRequest

	state := envBase(t, map[string]string{"ROOT_URL": "https://old.example"})
	plan := state
	plan.EnvVars = envVarInput(t, map[string]string{"ROOT_URL": "https://new.example"})

	diags := &diag.Diagnostics{}
	if updateInstance(context.Background(), f.testClient(), &plan, &state, diags) {
		t.Fatal("updateInstance reported the instance gone")
	}
	assertError(t, diags,
		"Unable to update the Instance environment",
		"fluxvale API: status 400: InvalidArgument: ROOT_URL is composed by the deployer and cannot be set",
	)
	assertRequestList(t, f.all(), "PATCH /api/v1/instances/inst-1/env")
}

// An unknown plan (whole map or one element) is a WARNING plus a skip:
// never send "" and never remove the managed keys on an unknown.
func TestUpdateEnvUnknownPlanWarnsAndSkips(t *testing.T) {
	cases := []struct {
		name string
		plan types.Map
	}{
		{"whole map unknown", types.MapUnknown(types.StringType)},
		{"element unknown", types.MapValueMust(types.StringType, map[string]attr.Value{
			"KNOWN": types.StringValue("x"),
			"MAYBE": types.StringUnknown(),
		})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAPI(t)
			f.instanceGets = []scriptedResponse{script(200, runningDoc)}

			state := envBase(t, map[string]string{"OLD": "1"})
			state.SecretEnvVarsVersion = types.Int64Value(1)
			state.SecretEnvVarsKeys = secretKeysList([]string{"OLD_SECRET"})
			plan := state
			plan.EnvVars = tc.plan
			// A rotation bundled with the unknown env must not be
			// consumed without being sent.
			plan.SecretEnvVarsVersion = types.Int64Value(2)

			diags := &diag.Diagnostics{}
			updateInstance(context.Background(), f.testClient(), &plan, &state, diags)
			assertNoErrors(t, diags)
			if len(diags.Warnings()) == 0 {
				t.Fatal("unknown env_vars: no warning diagnostic")
			}
			if got := diags.Warnings()[0].Summary(); got != "Instance environment not updated" {
				t.Errorf("warning summary = %q, want the env skip", got)
			}
			if !plan.SecretEnvVarsVersion.Equal(state.SecretEnvVarsVersion) {
				t.Errorf("version after an env skip = %v, want the prior %v", plan.SecretEnvVarsVersion, state.SecretEnvVarsVersion)
			}
			if keys := listOf(t, plan.SecretEnvVarsKeys); !reflect.DeepEqual(keys, []string{"OLD_SECRET"}) {
				t.Errorf("keys after an env skip = %v, want the prior list", keys)
			}

			// No merge PATCH — the final read is the only traffic.
			assertRequestList(t, f.all(), "GET /api/v1/instances/inst-1")
		})
	}
}

// Through the Update wrapper, state carries the configured map and the
// id/running posture survive — no "provider produced inconsistent
// result", no replacement.
func TestUpdateWrapperEnvKeepsConfiguredMap(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadResource(ctx, t)

	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{script(200, runningDoc)}
	r.pd = f.testClient()

	stateData := envBase(t, map[string]string{"API_KEY": "old", "KEEP": "same"})
	planData := stateData
	planData.EnvVars = envVarInput(t, map[string]string{"API_KEY": "new", "KEEP": "same", "ADDED": "yes"})

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

	got := rawToModel(ctx, t, sch, resp.State.Raw)
	want := map[string]string{"API_KEY": "new", "KEEP": "same", "ADDED": "yes"}
	if env := envMapOf(t, got.EnvVars); !reflect.DeepEqual(env, want) {
		t.Errorf("state env_vars = %v, want the configured plan map %v", env, want)
	}
	if got.ID.ValueString() != "inst-1" {
		t.Errorf("state id = %q, want inst-1 (never replaced)", got.ID.ValueString())
	}
	if got.Status.ValueString() != "running" {
		t.Errorf("state status = %q, want running", got.Status.ValueString())
	}

	findOne(t, f.all(), http.MethodPatch, "/api/v1/instances/inst-1/env")
	for _, req := range f.all() {
		if req.method == http.MethodPost || req.method == http.MethodDelete {
			t.Errorf("unexpected %s %s on an env-only update", req.method, req.path)
		}
	}
}

// An unknown plan that reaches Update must still write a state-safe
// map: warning, plan.EnvVars falls back to the prior state (never
// unknown — State.Set would fail), and no merge PATCH is sent.
func TestUpdateWrapperEnvUnknownKeepsPriorState(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		env  types.Map
	}{
		{"whole map unknown", types.MapUnknown(types.StringType)},
		{"element unknown", types.MapValueMust(types.StringType, map[string]attr.Value{
			"MAYBE": types.StringUnknown(),
		})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, sch := newSchemadResource(ctx, t)

			f := newFakeAPI(t)
			f.instanceGets = []scriptedResponse{script(200, runningDoc)}
			r.pd = f.testClient()

			stateData := envBase(t, map[string]string{"API_KEY": "keep"})
			planData := stateData
			planData.EnvVars = tc.env

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
			if len(resp.Diagnostics.Warnings()) == 0 {
				t.Fatal("unknown env_vars: no warning diagnostic")
			}

			got := rawToModel(ctx, t, sch, resp.State.Raw)
			if env := envMapOf(t, got.EnvVars); !reflect.DeepEqual(env, map[string]string{"API_KEY": "keep"}) {
				t.Errorf("state env_vars = %v, want the prior managed map", env)
			}
			if got.ID.ValueString() != "inst-1" {
				t.Errorf("state id = %q, want inst-1", got.ID.ValueString())
			}
			for _, req := range f.all() {
				if req.method == http.MethodPatch {
					t.Errorf("unexpected %s %s on an unknown env plan", req.method, req.path)
				}
			}
		})
	}
}

// --- read ------------------------------------------------------------

// A 404 on read means removed-from-terraform.
func TestRead404IsRemoval(t *testing.T) {
	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{script(http.StatusNotFound, `{"errors": [{"title": "Not Found"}]}`)}

	pd := f.testClient()
	data := testInstance(func(d *instanceModel) { d.ID = types.StringValue("gone") })
	diags := &diag.Diagnostics{}
	if !readInstance(context.Background(), pd, &data, diags) {
		t.Fatal("readInstance = false, want true (remove from state)")
	}
	assertNoErrors(t, diags)
}

// env_vars never parses from responses and the configured value
// survives Read — even a server that (against today's wire) sent
// env_vars back could not leak it into the model.
func TestReadKeepsConfiguredEnvVars(t *testing.T) {
	f := newFakeAPI(t)
	// The doc includes a hostile env_vars key the platform would never
	// send; the parser has no field for it.
	f.instanceGets = []scriptedResponse{script(200, strings.ReplaceAll(runningDoc,
		`"status": "running"`, `"status": "running", "env_vars": {"HOSTILE": "nope"}`))}

	pd := f.testClient()
	data := testInstance(func(d *instanceModel) {
		d.ID = types.StringValue("inst-1")
		d.EnvVars = envVarInput(t, map[string]string{"API_KEY": "sekrit"})
	})
	diags := &diag.Diagnostics{}
	readInstance(context.Background(), pd, &data, diags)
	assertNoErrors(t, diags)

	got := envMapOf(t, data.EnvVars)
	if got["API_KEY"] != "sekrit" || len(got) != 1 {
		t.Errorf("env_vars in state = %v, want exactly the configured map", got)
	}
}

// --- delete ----------------------------------------------------------

// The DELETE response is not the finish line — teardown is async, so
// the delete polls until the 404.
func TestDeletePollsUntil404(t *testing.T) {
	f := newFakeAPI(t)
	deletingDoc := strings.ReplaceAll(runningDoc, `"running"`, `"deleting"`)
	f.instanceGets = []scriptedResponse{
		script(200, deletingDoc),
		script(http.StatusNotFound, `{"errors": [{"title": "Not Found"}]}`),
	}

	pd := f.testClient()
	diags := &diag.Diagnostics{}
	deleteInstance(context.Background(), pd, types.StringValue("inst-1"), diags)
	assertNoErrors(t, diags)

	reqs := f.all()
	delete := findOne(t, reqs, http.MethodDelete, "/api/v1/instances/inst-1")
	if delete.method != http.MethodDelete {
		t.Fatalf("request = %s %s, want DELETE", delete.method, delete.path)
	}
	gets := 0
	for _, r := range reqs {
		if r.method == http.MethodGet && strings.HasSuffix(r.path, "/instances/inst-1") {
			gets++
		}
	}
	if gets < 2 {
		t.Errorf("GET count = %d, want at least 2 (deleting, then the 404)", gets)
	}
}

func TestDeleteTimeoutIsAnError(t *testing.T) {
	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{script(200, runningDoc)} // never goes away

	pd := f.testClient()
	pd.pollTimeout = 20 * time.Millisecond
	diags := &diag.Diagnostics{}
	deleteInstance(context.Background(), pd, types.StringValue("inst-1"), diags)
	assertError(t, diags, "teardown did not finish", "timed out")
}

// --- org resolution ---------------------------------------------------

func TestResolveOrgID(t *testing.T) {
	sole := `{"data": [{"id": "org-1", "type": "organization", "attributes": {"slug": "acme", "name": "Acme"}}]}`
	two := `{"data": [
		{"id": "org-1", "type": "organization", "attributes": {"slug": "acme", "name": "Acme"}},
		{"id": "org-2", "type": "organization", "attributes": {"slug": "b-co", "name": "B Co"}}
	]}`
	none := `{"data": []}`
	pd := func(doc string) *providerData {
		f := newFakeAPI(t)
		f.orgsDoc = doc
		return f.testClient()
	}

	t.Run("sole org is the default", func(t *testing.T) {
		diags := &diag.Diagnostics{}
		got := resolveOrgID(context.Background(), pd(sole).client, types.StringNull(), types.StringNull(), diags)
		assertNoErrors(t, diags)
		if got != "org-1" {
			t.Errorf("org id = %q, want org-1", got)
		}
	})

	t.Run("ambiguity is an error listing the orgs", func(t *testing.T) {
		diags := &diag.Diagnostics{}
		got := resolveOrgID(context.Background(), pd(two).client, types.StringNull(), types.StringNull(), diags)
		assertError(t, diags, "Multiple organizations", "Acme (acme)", "B Co (b-co)")
		if got != "" {
			t.Errorf("org id = %q on ambiguity, want empty", got)
		}
	})

	t.Run("no org at all", func(t *testing.T) {
		diags := &diag.Diagnostics{}
		resolveOrgID(context.Background(), pd(none).client, types.StringNull(), types.StringNull(), diags)
		assertError(t, diags, "No organization")
	})

	t.Run("override by slug wins", func(t *testing.T) {
		diags := &diag.Diagnostics{}
		got := resolveOrgID(context.Background(), pd(two).client, types.StringValue("b-co"), types.StringNull(), diags)
		assertNoErrors(t, diags)
		if got != "org-2" {
			t.Errorf("org id = %q, want org-2 (slug b-co)", got)
		}
	})

	t.Run("override by id wins", func(t *testing.T) {
		diags := &diag.Diagnostics{}
		got := resolveOrgID(context.Background(), pd(two).client, types.StringValue("org-1"), types.StringNull(), diags)
		assertNoErrors(t, diags)
		if got != "org-1" {
			t.Errorf("org id = %q, want org-1", got)
		}
	})

	t.Run("resource override beats the provider default", func(t *testing.T) {
		diags := &diag.Diagnostics{}
		got := resolveOrgID(context.Background(), pd(two).client, types.StringValue("b-co"), types.StringValue("acme"), diags)
		assertNoErrors(t, diags)
		if got != "org-2" {
			t.Errorf("org id = %q, want the resource override org-2", got)
		}
	})

	t.Run("provider default used when the resource is unset", func(t *testing.T) {
		diags := &diag.Diagnostics{}
		got := resolveOrgID(context.Background(), pd(two).client, types.StringNull(), types.StringValue("acme"), diags)
		assertNoErrors(t, diags)
		if got != "org-1" {
			t.Errorf("org id = %q, want the provider default org-1", got)
		}
	})

	t.Run("unknown override names the memberships", func(t *testing.T) {
		diags := &diag.Diagnostics{}
		resolveOrgID(context.Background(), pd(two).client, types.StringValue("nope"), types.StringNull(), diags)
		assertError(t, diags, "not found", "Acme (acme)")
	})
}
