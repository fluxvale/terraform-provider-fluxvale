package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fluxvale/fluxvale/apps/tfprovider/internal/client"
	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// fakeAPI is a scripted FluxVale API: it records every request and
// serves canned JSON per route. GET /api/v1/instances/:id walks a
// scripted status sequence (last entry repeats — the poll loop's
// steady state), so create-wait and delete-poll tests script the
// transition they need.
type fakeAPI struct {
	*httptest.Server

	mu       sync.Mutex
	requests []recordedRequest

	// routes
	orgsDoc      string
	appsDoc      string
	appDoc       string // GET /apps/:slug
	appStatus    int
	versionsDoc  string
	postResponse string // POST /instances (201)
	instanceGets []scriptedResponse
	deleteStatus int
	// failPatch/failPatchStatus: when a PATCH path carries this suffix,
	// the fake answers with this status and an errors body — the
	// PATCH-failure arm.
	failPatch       string
	failPatchStatus int

	// backups + gateway slices
	backupGets         []scriptedResponse // GET /instances/:id/backup-setting
	backupPostDoc      string             // POST /instances/:id/backup-setting (201)
	backupPatchStatus  int                // PATCH /backup-settings/:id
	backupPatchDoc     string
	backupDeleteStatus int
	gatewayGets        []scriptedResponse // GET /organizations/:org/access-gateway
	gatewayPostStatus  int                // POST /access-gateways
	gatewayPostDoc     string
	detachStatus       int

	// access-domain slice
	domainPostStatus   int // POST /access-domains (the register)
	domainPostDoc      string
	domainGets         []scriptedResponse // GET /access-domains/:id
	domainDeleteStatus int
	domainDeleteDoc    string

	t *testing.T
}

type recordedRequest struct {
	method, path, auth string
	body               map[string]any
}

type scriptedResponse struct {
	status int
	body   string
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{
		t:                  t,
		deleteStatus:       http.StatusOK,
		backupDeleteStatus: http.StatusOK,
		backupPatchStatus:  http.StatusOK,
		detachStatus:       http.StatusOK,
		gatewayPostStatus:  http.StatusCreated,
		gatewayPostDoc:     gatewayCreateDoc,
		backupPostDoc:      backupRowDoc,
		backupPatchDoc:     backupRowDoc,
		domainPostStatus:   http.StatusCreated,
		domainPostDoc:      domainPendingDoc,
		domainDeleteStatus: http.StatusOK,
		domainDeleteDoc:    domainDeleteDoc,
	}
	mux := http.NewServeMux()

	mux.HandleFunc("/api/v1/organizations", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, nil)
		f.json(w, http.StatusOK, f.orgsDoc)
	})
	mux.HandleFunc("/api/v1/apps", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, nil)
		f.json(w, http.StatusOK, f.appsDoc)
	})
	mux.HandleFunc("/api/v1/apps/", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, nil)
		// /apps/<slug> vs /apps/<id>/versions — the version suffix wins.
		if strings.HasSuffix(r.URL.Path, "/versions") {
			f.json(w, http.StatusOK, f.versionsDoc)
			return
		}
		status := f.appStatus
		if status == 0 {
			status = http.StatusOK
		}
		f.json(w, status, f.appDoc)
	})
	mux.HandleFunc("/api/v1/instances", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			f.json(w, http.StatusMethodNotAllowed, `{"errors": [{"title": "Method Not Allowed"}]}`)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.record(r, body)
		f.json(w, http.StatusCreated, f.postResponse)
	})
	mux.HandleFunc("/api/v1/instances/", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		f.record(r, body)
		switch r.Method {
		case http.MethodGet:
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.instanceGets) == 0 {
				f.t.Errorf("GET %s: no scripted response left", r.URL.Path)
				f.json(w, http.StatusInternalServerError, `{"errors": [{"title": "script ran dry"}]}`)
				return
			}
			resp := f.instanceGets[0]
			if len(f.instanceGets) > 1 {
				f.instanceGets = f.instanceGets[1:]
			}
			f.json(w, resp.status, resp.body)
		case http.MethodDelete:
			f.json(w, f.deleteStatus, `{"id": "inst-1", "status": "pending"}`)
		case http.MethodPatch:
			if f.failPatch != "" && strings.HasSuffix(r.URL.Path, f.failPatch) {
				f.json(w, f.failPatchStatus, `{"errors": [{"title": "Forbidden", "detail": "token is missing the required scope"}]}`)
				return
			}
			f.json(w, http.StatusOK, patchResponse)
		default:
			f.json(w, http.StatusMethodNotAllowed, `{"errors": [{"title": "Method Not Allowed"}]}`)
		}
	})

	// Backups: the routed settings family. The instance-scoped patterns
	// are more specific than the /instances/ subtree above, so they win.
	mux.HandleFunc("POST /api/v1/instances/{id}/backup-setting", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.record(r, body)
		f.json(w, http.StatusCreated, f.backupPostDoc)
	})
	mux.HandleFunc("GET /api/v1/instances/{id}/backup-setting", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, nil)
		f.scriptedGet(w, r, &f.backupGets)
	})
	mux.HandleFunc("/api/v1/backup-settings/", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		f.record(r, body)
		switch r.Method {
		case http.MethodPatch:
			f.json(w, f.backupPatchStatus, f.backupPatchDoc)
		case http.MethodDelete:
			f.json(w, f.backupDeleteStatus, `{"meta": {"message": "deleted"}}`)
		default:
			f.json(w, http.StatusMethodNotAllowed, `{"errors": [{"title": "Method Not Allowed"}]}`)
		}
	})

	// Access gateway: the singleton read + create + pairing verbs.
	mux.HandleFunc("GET /api/v1/organizations/{org}/access-gateway", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, nil)
		f.scriptedGet(w, r, &f.gatewayGets)
	})
	mux.HandleFunc("POST /api/v1/access-gateways", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.record(r, body)
		f.json(w, f.gatewayPostStatus, f.gatewayPostDoc)
	})
	// The pairing verbs render flat/generic shapes — the 201 attach body
	// is the raw route row, the detach body a bare success flag.
	mux.HandleFunc("POST /api/v1/access-gateways/{id}/attachments", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.record(r, body)
		instanceID, _ := body["data"].(map[string]any)["instance_id"].(string)
		f.json(w, http.StatusCreated, fmt.Sprintf(
			`{"gateway_id": "gw-1", "instance_id": %q, "route_key": "8443"}`, instanceID))
	})
	mux.HandleFunc("DELETE /api/v1/access-gateways/{id}/attachments/{iid}", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, nil)
		f.json(w, f.detachStatus, `{"success": true}`)
	})

	// Access domains: the register (201), the id-scoped read, and the
	// synchronous teardown DELETE. The DELETE 200 renders the row flat
	// (the generic-action shape — no JSON:API envelope).
	mux.HandleFunc("POST /api/v1/access-domains", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.record(r, body)
		f.json(w, f.domainPostStatus, f.domainPostDoc)
	})
	mux.HandleFunc("GET /api/v1/access-domains/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, nil)
		f.scriptedGet(w, r, &f.domainGets)
	})
	mux.HandleFunc("DELETE /api/v1/access-domains/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, nil)
		f.json(w, f.domainDeleteStatus, f.domainDeleteDoc)
	})

	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

// scriptedGet pops the next scripted response for a GET route (the
// last entry repeats — the poll loop's steady state).
func (f *fakeAPI) scriptedGet(w http.ResponseWriter, r *http.Request, script *[]scriptedResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(*script) == 0 {
		f.t.Errorf("GET %s: no scripted response left", r.URL.Path)
		f.json(w, http.StatusInternalServerError, `{"errors": [{"title": "script ran dry"}]}`)
		return
	}
	resp := (*script)[0]
	if len(*script) > 1 {
		*script = (*script)[1:]
	}
	f.json(w, resp.status, resp.body)
}

func (f *fakeAPI) json(w http.ResponseWriter, status int, body string) {
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// handle wraps a mux handler with recording.
func (f *fakeAPI) record(r *http.Request, body map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, recordedRequest{
		method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"), body: body,
	})
}

func (f *fakeAPI) all() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...)
}

// testClient returns a providerData wired to the fake with fast poll
// pacing.
func (f *fakeAPI) testClient() *providerData {
	return &providerData{
		client:       client.New(f.URL, "fvpat_test", "test", f.Client()),
		pollInterval: time.Millisecond,
		pollTimeout:  100 * time.Millisecond,
	}
}

// --- fixtures --------------------------------------------------------

// Wire docs, shaped from the platform's own route tests + open_api.json.
const (
	orgsDoc = `{"data": [
		{"id": "org-1", "type": "organization", "attributes": {"slug": "acme", "name": "Acme"}},
		{"id": "org-2", "type": "organization", "attributes": {"slug": "b-co", "name": "B Co"}}
	]}`

	appDoc = `{"data": {"id": "app-1", "type": "app", "attributes": {"name": "Forgejo", "slug": "forgejo",
		"tagline": "Git forge", "description": "Self-hosted forge", "icon_url": null,
		"source_url": null, "docs_url": null, "file_access": false}}}`

	appsDoc = `{"data": [
		{"id": "app-1", "type": "app", "attributes": {"name": "Forgejo", "slug": "forgejo",
			"tagline": "Git forge", "description": "Self-hosted forge", "icon_url": null,
			"source_url": null, "docs_url": null, "file_access": false}},
		{"id": "app-2", "type": "app", "attributes": {"name": "Memos", "slug": "memos", "file_access": true}}
	]}`

	versionsDoc = `{"data": [
		{"id": "ver-2", "type": "app_version", "attributes": {"app_id": "app-1", "version": "2.0",
			"image": "reg/app:2.0", "port": 3000, "healthcheck_path": "/", "storage_mount_path": "/data",
			"default_cpu_cores": "1.5", "default_memory_mb": 512, "default_storage_gb": 2,
			"default_env_vars": {"FOO": "bar"}, "release_notes": "new", "published_at": "2026-02-01T00:00:00Z"}},
		{"id": "ver-1", "type": "app_version", "attributes": {"app_id": "app-1", "version": "1.0",
			"image": "reg/app:1.0", "port": 3000, "healthcheck_path": "/", "storage_mount_path": "/data",
			"default_cpu_cores": "0.5", "default_memory_mb": 256, "default_storage_gb": 0,
			"default_env_vars": {}, "release_notes": null, "published_at": null}}
	]}`

	// The create response: status pending, namespace/deployed_at null,
	// image/port derived server-side.
	createResponse = `{"data": {"id": "inst-1", "type": "instance", "attributes": {
		"name": "Blog", "slug": "velvety-otter-1234", "namespace": null,
		"status": "pending", "exposure": "public", "always_on": false,
		"image": "reg/app:1.0", "port": 3000, "healthcheck_path": "/",
		"storage_mount_path": "/data", "file_access": false,
		"cpu_cores": "0.5", "memory_mb": 256, "storage_gb": 1, "deployed_at": null}}}`

	runningDoc = `{"data": {"id": "inst-1", "type": "instance", "attributes": {
		"name": "Blog", "slug": "velvety-otter-1234", "namespace": "fluxvale-app-inst-1",
		"status": "running", "exposure": "public", "always_on": false,
		"image": "reg/app:1.0", "port": 3000, "healthcheck_path": "/",
		"storage_mount_path": "/data", "file_access": false,
		"cpu_cores": "0.5", "memory_mb": 256, "storage_gb": 1,
		"deployed_at": "2026-11-06T10:00:00Z",
		"organization_id": "org-1", "app_version_id": "ver-1"}}}`

	stoppedDoc = `{"data": {"id": "inst-1", "type": "instance", "attributes": {
		"name": "Blog", "slug": "velvety-otter-1234", "namespace": "fluxvale-app-inst-1",
		"status": "stopped", "exposure": "public", "always_on": false,
		"image": "reg/app:1.0", "port": 3000, "healthcheck_path": "/",
		"storage_mount_path": "/data", "file_access": false,
		"cpu_cores": "0.5", "memory_mb": 256, "storage_gb": 1,
		"deployed_at": "2026-11-06T10:00:00Z",
		"organization_id": "org-1", "app_version_id": "ver-1"}}}`

	errorDoc = `{"data": {"id": "inst-1", "type": "instance", "attributes": {
		"name": "Blog", "slug": "velvety-otter-1234", "namespace": "fluxvale-app-inst-1",
		"status": "error", "exposure": "public", "always_on": false,
		"image": "reg/app:1.0", "port": 3000, "healthcheck_path": "/",
		"storage_mount_path": "/data", "file_access": false,
		"cpu_cores": "0.5", "memory_mb": 256, "storage_gb": 1,
		"deployed_at": "2026-11-06T10:00:00Z",
		"organization_id": "org-1", "app_version_id": "ver-1"}}}`

	patchResponse = runningDoc

	// The backups row: retention/enabled at the server's defaults, the
	// write-only secret never present.
	backupRowDoc = `{"data": {"id": "bs-1", "type": "backup_setting", "attributes": {
		"instance_id": "inst-1", "endpoint": "https://account.r2.cloudflarestorage.com",
		"bucket": "fluxvale-kavita", "prefix": "fluxvale/app",
		"access_key_id": "AKIAIOSFODNN7EXAMPLE", "retention_keep": 7, "enabled": true}}}`

	// The create response OMITS provisioned_at (not in the create
	// route's field list — only the get route carries it): the fixture
	// omits it too, so the parser's absence-tolerance is exercised for
	// real.
	gatewayCreateDoc = `{"data": {"id": "gw-1", "type": "access_gateway", "attributes": {
		"organization_id": "org-1", "hostname": "fv-0a1b2c3d", "tailnet_domain": "example.ts.net",
		"provider": "tailscale", "state": "provisioning", "status_message": null,
		"monthly_cents": 500}}}`

	gatewayProvisioningDoc = `{"data": {"id": "gw-1", "type": "access_gateway", "attributes": {
		"organization_id": "org-1", "hostname": "fv-0a1b2c3d", "tailnet_domain": "example.ts.net",
		"provider": "tailscale", "state": "provisioning", "status_message": null,
		"monthly_cents": 500, "provisioned_at": null}}}`

	gatewayActiveDoc = `{"data": {"id": "gw-1", "type": "access_gateway", "attributes": {
		"organization_id": "org-1", "hostname": "fv-0a1b2c3d", "tailnet_domain": "example.ts.net",
		"provider": "tailscale", "state": "active", "status_message": null,
		"monthly_cents": 500, "provisioned_at": "2026-11-06T10:00:00Z"}}}`

	// The access-domain rows: the pinned public field set — id rides
	// the document, the attributes are exactly domain/kind/state/
	// organization_id (cert_secret_ref is internal, never on the wire).
	domainPendingDoc = `{"data": {"id": "dom-1", "type": "access_domain", "attributes": {
		"domain": "apps.customer.com", "kind": "custom", "state": "pending",
		"organization_id": "org-1"}}}`

	domainVerifiedDoc = `{"data": {"id": "dom-1", "type": "access_domain", "attributes": {
		"domain": "apps.customer.com", "kind": "custom", "state": "verified",
		"organization_id": "org-1"}}}`

	// The teardown DELETE's 200 renders the row flat (the generic
	// action's shape — the id at the top level, no JSON:API envelope).
	domainDeleteDoc = `{"id": "dom-1", "domain": "apps.customer.com", "kind": "custom", "state": "pending"}`
)

// statusDoc renders the running fixture with a different status — the
// precondition/wait scripts' building block.
func statusDoc(status string) string {
	return strings.ReplaceAll(runningDoc, `"running"`, `"`+status+`"`)
}

// fake doc builders for scripted GET sequences.
func script(status int, doc string) scriptedResponse {
	return scriptedResponse{status: status, body: doc}
}

// --- model helpers ---------------------------------------------------

// testInstance builds an instanceModel with the required fields set;
// mutators adjust the rest.
func testInstance(mutators ...func(*instanceModel)) instanceModel {
	data := instanceModel{
		Organization: types.StringNull(),
		Name:         types.StringValue("Blog"),
		AppVersionID: types.StringValue("ver-1"),
		App:          types.StringNull(),
		AppVersion:   types.StringNull(),
		CPUCores:     types.Float64Value(0.5),
		MemoryMB:     types.Int64Value(256),
		StorageGB:    types.Int64Value(1),
		EnvVars:      types.MapNull(types.StringType),
		Running:      types.BoolNull(),
		AlwaysOn:     types.BoolNull(),
		Exposure:     types.StringNull(),
		ID:           types.StringNull(),
		Slug:         types.StringNull(),
		Namespace:    types.StringNull(),
		Status:       types.StringNull(),
		Port:         types.Int64Null(),
		Image:        types.StringNull(),
		DeployedAt:   types.StringNull(),
	}
	for _, m := range mutators {
		m(&data)
	}
	return data
}

// envVarInput wraps a plain map into the schema's env_vars types.Map.
func envVarInput(t *testing.T, m map[string]string) types.Map {
	t.Helper()
	v, diags := types.MapValueFrom(context.Background(), types.StringType, m)
	if diags.HasError() {
		t.Fatalf("MapValueFrom: %v", diags)
	}
	return v
}

// testBackups builds a backupsModel with the required fields set;
// mutators adjust the rest.
func testBackups(mutators ...func(*backupsModel)) backupsModel {
	data := backupsModel{
		InstanceID:      types.StringValue("inst-1"),
		Endpoint:        types.StringValue("https://account.r2.cloudflarestorage.com"),
		Bucket:          types.StringValue("fluxvale-kavita"),
		Prefix:          types.StringNull(),
		AccessKeyID:     types.StringValue("AKIAIOSFODNN7EXAMPLE"),
		RetentionKeep:   types.Int64Null(),
		Enabled:         types.BoolNull(),
		SecretAccessKey: types.StringNull(),
		ID:              types.StringNull(),
	}
	for _, m := range mutators {
		m(&data)
	}
	return data
}

// testGateway builds a gatewayModel with the required fields set;
// mutators adjust the rest.
func testGateway(mutators ...func(*gatewayModel)) gatewayModel {
	data := gatewayModel{
		Organization:   types.StringNull(),
		TailnetDomain:  types.StringValue("example.ts.net"),
		AuthKey:        types.StringValue("tskey-test"),
		Instances:      types.SetNull(types.StringType),
		ID:             types.StringNull(),
		OrganizationID: types.StringNull(),
		Hostname:       types.StringNull(),
		AccessProvider: types.StringNull(),
		State:          types.StringNull(),
		StatusMessage:  types.StringNull(),
		MonthlyCents:   types.Int64Null(),
		ProvisionedAt:  types.StringNull(),
	}
	for _, m := range mutators {
		m(&data)
	}
	return data
}

// testDomain builds a customDomainModel with the required fields set;
// mutators adjust the rest. The timeouts object is null with the
// schema's exact CustomType shape (create only) — the framework
// null-diffs against it.
func testDomain(mutators ...func(*customDomainModel)) customDomainModel {
	data := customDomainModel{
		Domain:              types.StringValue("apps.customer.com"),
		Organization:        types.StringNull(),
		WaitForVerification: types.BoolNull(),
		Timeouts: timeouts.Value{Object: types.ObjectNull(map[string]attr.Type{
			"create": types.StringType,
		})},
		ID:             types.StringNull(),
		Kind:           types.StringNull(),
		State:          types.StringNull(),
		OrganizationID: types.StringNull(),
	}
	for _, m := range mutators {
		m(&data)
	}
	return data
}

// timeoutsWith builds the model's timeouts object carrying a create
// duration — the wait-deadline override in tests (the validator only
// checks parseability, the framework stores the string).
func timeoutsWith(create string) timeouts.Value {
	return timeouts.Value{Object: types.ObjectValueMust(
		map[string]attr.Type{"create": types.StringType},
		map[string]attr.Value{"create": types.StringValue(create)},
	)}
}

// setInput wraps plain strings into the schema's instances types.Set.
func setInput(t *testing.T, ids ...string) types.Set {
	t.Helper()
	v, diags := types.SetValueFrom(context.Background(), types.StringType, ids)
	if diags.HasError() {
		t.Fatalf("SetValueFrom: %v", diags)
	}
	return v
}

// assertNoErrors fails the test when the diagnostics carry errors.
func assertNoErrors(t *testing.T, diags *diag.Diagnostics) {
	t.Helper()
	if diags.HasError() {
		t.Fatalf("unexpected error diagnostics: %v", diags)
	}
}

// assertError fails the test unless the diagnostics carry an error
// whose summary+detail contain every needle.
func assertError(t *testing.T, diags *diag.Diagnostics, needles ...string) {
	t.Helper()
	if !diags.HasError() {
		t.Fatalf("expected error diagnostics containing %v, got none", needles)
	}
	joined := ""
	for _, d := range diags.Errors() {
		joined += d.Summary() + " | " + d.Detail() + "\n"
	}
	for _, needle := range needles {
		if !strings.Contains(joined, needle) {
			t.Fatalf("diagnostics = %q, want something containing %q", joined, needle)
		}
	}
}

// findOne returns the first recorded request matching method+suffix.
func findOne(t *testing.T, reqs []recordedRequest, method, pathSuffix string) recordedRequest {
	t.Helper()
	for _, r := range reqs {
		if r.method == method && strings.HasSuffix(r.path, pathSuffix) {
			return r
		}
	}
	t.Fatalf("no %s %s among %+v", method, pathSuffix, reqs)
	return recordedRequest{}
}

// attrsOf digs the attributes object out of a recorded JSON:API body.
func attrsOf(t *testing.T, r recordedRequest) map[string]any {
	t.Helper()
	data, ok := r.body["data"].(map[string]any)
	if !ok {
		t.Fatalf("%s %s: body has no data object: %+v", r.method, r.path, r.body)
	}
	attrs, _ := data["attributes"].(map[string]any)
	return attrs
}

// envMapOf reads a types.Map back into a plain map for assertions.
func envMapOf(t *testing.T, m types.Map) map[string]string {
	t.Helper()
	if m.IsNull() {
		return nil
	}
	var elems map[string]types.String
	diags := m.ElementsAs(context.Background(), &elems, false)
	if diags.HasError() {
		t.Fatalf("ElementsAs: %v", diags)
	}
	out := make(map[string]string, len(elems))
	for k, v := range elems {
		out[k] = v.ValueString()
	}
	return out
}
