package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// handlerServer is testServer for handlers that assert what they
// received.
func handlerServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// instanceDocument is the detail surface the platform renders —
// @detail_fields, id at data.id. env_vars never rides responses
// (hide_fields); it appears here only to prove the parser ignores a
// key it must never carry.
func instanceDocument(id, status string) string {
	return `{"data": {"id": "` + id + `", "type": "instance", "attributes": {
		"name": "Blog", "slug": "velvety-otter-1234", "namespace": "fluxvale-app-` + id + `",
		"status": "` + status + `", "exposure": "public", "always_on": false,
		"env_vars": {"HOSTILE": "must-not-parse"},
		"image": "registry.example.com/app:1.0", "port": 3000,
		"healthcheck_path": "/", "storage_mount_path": "/data", "file_access": false,
		"cpu_cores": "0.5", "memory_mb": 256, "storage_gb": 1,
		"deployed_at": "2026-11-06T10:00:00Z"
	}}}`
}

// The create body is the wire contract (instance_route_test.exs +
// #503): everything — app_version_id, organization_id, env_vars —
// rides inside data.attributes; cpu_cores ships as the decimal's
// string form.
func TestCreateInstanceBodyShape(t *testing.T) {
	var gotAuth, gotCT, gotAccept string
	var gotPath, gotMethod string
	var gotBody map[string]any
	srv := handlerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotAccept = r.Header.Get("Accept")
		gotPath = r.URL.Path
		gotMethod = r.Method
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, instanceDocument("inst-1", "pending"))
	}))

	c := New(srv.URL, "fvpat_test", "test", srv.Client())
	in := CreateInstanceInput{
		Name:           "Blog",
		CPUCores:       0.5,
		MemoryMB:       256,
		StorageGB:      1,
		AppVersionID:   "ver-1",
		OrganizationID: "org-1",
		EnvVars:        map[string]string{"API_KEY": "sekrit"},
	}
	instance, err := c.CreateInstance(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	if gotMethod != http.MethodPost || gotPath != "/api/v1/instances" {
		t.Errorf("request = %s %s, want POST /api/v1/instances", gotMethod, gotPath)
	}
	if gotAuth != "Bearer fvpat_test" {
		t.Errorf("Authorization = %q, want the bearer token", gotAuth)
	}
	if gotCT != "application/vnd.api+json" || gotAccept != "application/vnd.api+json" {
		t.Errorf("Content-Type = %q, Accept = %q, want JSON:API both ways", gotCT, gotAccept)
	}

	data, _ := gotBody["data"].(map[string]any)
	if data == nil || data["type"] != "instance" {
		t.Fatalf("data = %+v, want type instance", gotBody["data"])
	}
	attrs, _ := data["attributes"].(map[string]any)
	if attrs == nil {
		t.Fatal("data.attributes missing — every create field rides the attributes object")
	}
	if attrs["name"] != "Blog" {
		t.Errorf("attributes.name = %v, want Blog", attrs["name"])
	}
	if attrs["cpu_cores"] != "0.5" {
		t.Errorf("attributes.cpu_cores = %v (%T), want the string \"0.5\" — the platform's decimal wire type", attrs["cpu_cores"], attrs["cpu_cores"])
	}
	if attrs["memory_mb"] != 256.0 || attrs["storage_gb"] != 1.0 {
		t.Errorf("attributes memory/storage = %v/%v, want 256/1", attrs["memory_mb"], attrs["storage_gb"])
	}
	if attrs["app_version_id"] != "ver-1" {
		t.Errorf("attributes.app_version_id = %v, want ver-1 (an attribute, not a relationship)", attrs["app_version_id"])
	}
	if attrs["organization_id"] != "org-1" {
		t.Errorf("attributes.organization_id = %v, want org-1 (the #408 argument renders as an attribute)", attrs["organization_id"])
	}
	if attrs["env_vars"].(map[string]any)["API_KEY"] != "sekrit" {
		t.Errorf("attributes.env_vars = %v, want the write-only argument's map", attrs["env_vars"])
	}
	for _, absent := range []string{"always_on", "exposure", "running"} {
		if _, ok := attrs[absent]; ok {
			t.Errorf("attributes.%s sent — not in the create action's accept list", absent)
		}
	}

	if instance.ID != "inst-1" || instance.Status != "pending" {
		t.Errorf("instance = %+v, want inst-1 pending", instance)
	}
	if instance.CPUCores == nil || *instance.CPUCores != 0.5 {
		t.Errorf("CPUCores = %v, want 0.5 parsed from the decimal string", instance.CPUCores)
	}
	if instance.Port == nil || *instance.Port != 3000 {
		t.Errorf("Port = %v, want 3000", instance.Port)
	}
	if instance.AlwaysOn {
		t.Errorf("AlwaysOn = true, want false")
	}
}

// Empty optional fields must be omitted from the body, not sent null.
func TestCreateInstanceOmitsUnsetFields(t *testing.T) {
	var gotBody map[string]any
	srv := handlerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, instanceDocument("inst-1", "pending"))
	}))

	c := New(srv.URL, "fvpat_test", "test", srv.Client())
	_, err := c.CreateInstance(context.Background(), CreateInstanceInput{
		Name: "Blog", CPUCores: 2, MemoryMB: 512, StorageGB: 5, AppVersionID: "ver-1",
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	attrs := gotBody["data"].(map[string]any)["attributes"].(map[string]any)
	if _, ok := attrs["organization_id"]; ok {
		t.Error("organization_id sent — empty means the server's personal-org default")
	}
	if _, ok := attrs["env_vars"]; ok {
		t.Error("env_vars sent — nil means the server merges catalog defaults")
	}
	if attrs["cpu_cores"] != "2" {
		t.Errorf("cpu_cores = %v, want \"2\" (shortest decimal form)", attrs["cpu_cores"])
	}
}

// Pre-deploy rows carry null namespace/deployed_at — absent-from-map
// and explicit null must both parse as absent.
func TestGetInstanceParsesNullDetailFields(t *testing.T) {
	doc := `{"data": {"id": "inst-1", "type": "instance", "attributes": {
		"name": "Blog", "slug": "velvety-otter-1234", "namespace": null,
		"status": "pending", "exposure": "public", "always_on": false,
		"image": "registry.example.com/app:1.0", "port": 3000,
		"healthcheck_path": "/", "storage_mount_path": "/data", "file_access": false,
		"cpu_cores": "0.5", "memory_mb": 256, "storage_gb": 1, "deployed_at": null
	}}}`
	srv := testServer(t, http.StatusOK, doc)

	c := New(srv.URL, "fvpat_test", "test", srv.Client())
	instance, err := c.GetInstance(context.Background(), "inst-1")
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if instance.Namespace != nil || instance.DeployedAt != nil {
		t.Errorf("namespace/deployed_at = %v/%v, want nil on a pending row", instance.Namespace, instance.DeployedAt)
	}
	if instance.Status != "pending" {
		t.Errorf("Status = %q, want pending", instance.Status)
	}
}

func TestGetInstance404IsTyped(t *testing.T) {
	srv := testServer(t, http.StatusNotFound, `{"errors": [{"title": "Not Found", "detail": "no such instance"}]}`)
	c := New(srv.URL, "fvpat_test", "test", srv.Client())

	_, err := c.GetInstance(context.Background(), "gone")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Fatalf("err = %v, want a 404 *APIError (the removed-from-state signal)", err)
	}
}

// The lifecycle PATCH verbs carry the resource id plus an attributes
// object — the exact shape the route tests send (empty attributes for
// the no-input verbs).
func TestInstanceActionBodyShape(t *testing.T) {
	cases := []struct {
		action    string
		attrs     map[string]any
		wantAttrs map[string]any
	}{
		{"deploy", map[string]any{}, map[string]any{}},
		{"stop", map[string]any{}, map[string]any{}},
		{"always-on", map[string]any{"always_on": true}, map[string]any{"always_on": true}},
		{"exposure", map[string]any{"exposure": "private"}, map[string]any{"exposure": "private"}},
	}
	for _, tc := range cases {
		var gotPath, gotMethod string
		var gotBody map[string]any
		srv := handlerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath, gotMethod = r.URL.Path, r.Method
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				t.Errorf("%s: decode request body: %v", tc.action, err)
			}
			fmt.Fprint(w, instanceDocument("inst-1", "running"))
		}))

		c := New(srv.URL, "fvpat_test", "test", srv.Client())
		if _, err := c.InstanceAction(context.Background(), "inst-1", tc.action, tc.attrs); err != nil {
			t.Fatalf("%s: InstanceAction: %v", tc.action, err)
		}

		wantPath := "/api/v1/instances/inst-1/" + tc.action
		if gotMethod != http.MethodPatch || gotPath != wantPath {
			t.Errorf("%s: request = %s %s, want PATCH %s", tc.action, gotMethod, gotPath, wantPath)
		}
		data := gotBody["data"].(map[string]any)
		if data["type"] != "instance" || data["id"] != "inst-1" {
			t.Errorf("%s: data = %+v, want type instance + id inst-1", tc.action, data)
		}
		attrs := data["attributes"].(map[string]any)
		if len(attrs) != len(tc.wantAttrs) {
			t.Errorf("%s: attributes = %+v, want exactly %v", tc.action, attrs, tc.wantAttrs)
		}
		for k, want := range tc.wantAttrs {
			if attrs[k] != want {
				t.Errorf("%s: attributes[%s] = %v, want %v", tc.action, k, attrs[k], want)
			}
		}
	}
}

// The generic :delete renders flat (no JSON:API envelope — the route
// test's own shape) and must decode harmlessly.
func TestDeleteInstanceToleratesFlatBody(t *testing.T) {
	var gotMethod, gotPath string
	srv := handlerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		fmt.Fprint(w, `{"id": "inst-1", "status": "pending"}`)
	}))

	c := New(srv.URL, "fvpat_test", "test", srv.Client())
	if err := c.DeleteInstance(context.Background(), "inst-1"); err != nil {
		t.Fatalf("DeleteInstance over a flat body: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/api/v1/instances/inst-1" {
		t.Errorf("request = %s %s, want DELETE /api/v1/instances/inst-1", gotMethod, gotPath)
	}
}

// The read requests the extended sparse fieldset — @detail_fields
// plus the public FK ids import converges on (ash_json_api's fields[]
// replaces the route's default list).
func TestGetInstanceRequestsFKFields(t *testing.T) {
	var gotQuery map[string][]string
	srv := handlerServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery, _ = url.ParseQuery(r.URL.RawQuery)
		fmt.Fprint(w, instanceDocument("inst-1", "running"))
	})

	c := New(srv.URL, "fvpat_test", "test", srv.Client())
	instance, err := c.GetInstance(context.Background(), "inst-1")
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}

	fields := gotQuery["fields[instance]"]
	if len(fields) != 1 {
		t.Fatalf("fields[instance] = %v, want one parameter", fields)
	}
	for _, want := range []string{"status", "cpu_cores", "organization_id", "app_version_id"} {
		if !strings.Contains(fields[0], want) {
			t.Errorf("fields[instance] = %q, missing %q", fields[0], want)
		}
	}
	if strings.Contains(fields[0], "env_vars") || strings.Contains(fields[0], "status_message") {
		t.Errorf("fields[instance] = %q — must not request the hidden pair", fields[0])
	}

	// The fixture document carries no FK ids — absent parses as empty.
	if instance.OrganizationID != "" || instance.AppVersionID != "" {
		t.Errorf("FK ids = %q/%q, want empty when the server omits them",
			instance.OrganizationID, instance.AppVersionID)
	}
}

// The FK ids ride the read when the server echoes them (route test
// :120-138 — only the hidden pair is outside the field catalog).
func TestGetInstanceParsesFKIDs(t *testing.T) {
	doc := `{"data": {"id": "inst-1", "type": "instance", "attributes": {
		"name": "Blog", "slug": "velvety-otter-1234", "namespace": null,
		"status": "running", "exposure": "public", "always_on": false,
		"image": "reg/app:1.0", "port": 3000,
		"healthcheck_path": "/", "storage_mount_path": "/data", "file_access": false,
		"cpu_cores": "0.5", "memory_mb": 256, "storage_gb": 1, "deployed_at": null,
		"organization_id": "org-7", "app_version_id": "ver-3"
	}}}`
	srv := testServer(t, http.StatusOK, doc)
	c := New(srv.URL, "fvpat_test", "test", srv.Client())

	instance, err := c.GetInstance(context.Background(), "inst-1")
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if instance.OrganizationID != "org-7" || instance.AppVersionID != "ver-3" {
		t.Errorf("FK ids = %q/%q, want org-7/ver-3", instance.OrganizationID, instance.AppVersionID)
	}
}

// The create/PATCH responses keep the route default (no FK ids) —
// absent parses as empty, not drift.
func TestInstanceActionToleratesAbsentFKIDs(t *testing.T) {
	doc := `{"data": {"id": "inst-1", "type": "instance", "attributes": {
		"name": "Blog", "slug": "velvety-otter-1234", "status": "running",
		"exposure": "public", "always_on": false
	}}}`
	srv := testServer(t, http.StatusOK, doc)
	c := New(srv.URL, "fvpat_test", "test", srv.Client())

	instance, err := c.InstanceAction(context.Background(), "inst-1", "stop", map[string]any{})
	if err != nil {
		t.Fatalf("InstanceAction over a route-default response: %v", err)
	}
	if instance.OrganizationID != "" || instance.AppVersionID != "" {
		t.Errorf("FK ids = %q/%q, want empty on a route-default response",
			instance.OrganizationID, instance.AppVersionID)
	}
}

// The resize body carries all three dims — cpu_cores as the decimal
// string, memory/storage as integers.
func TestResizeInstanceBodyShape(t *testing.T) {
	var gotBody map[string]any
	var gotPath string
	srv := handlerServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		fmt.Fprint(w, instanceDocument("inst-1", "stopped"))
	})

	c := New(srv.URL, "fvpat_test", "test", srv.Client())
	if _, err := c.ResizeInstance(context.Background(), "inst-1", 2, 1024, 5); err != nil {
		t.Fatalf("ResizeInstance: %v", err)
	}

	if gotPath != "/api/v1/instances/inst-1/resize" {
		t.Errorf("path = %q, want /api/v1/instances/inst-1/resize", gotPath)
	}
	attrs := gotBody["data"].(map[string]any)["attributes"].(map[string]any)
	if attrs["cpu_cores"] != "2" {
		t.Errorf("cpu_cores = %v (%T), want the decimal string \"2\"", attrs["cpu_cores"], attrs["cpu_cores"])
	}
	if attrs["memory_mb"] != 1024.0 || attrs["storage_gb"] != 5.0 {
		t.Errorf("memory/storage = %v/%v, want 1024/5", attrs["memory_mb"], attrs["storage_gb"])
	}
}

// A wrong-typed detail field is shape drift, not a silent zero.
func TestInstanceShapeDriftIsAnError(t *testing.T) {
	doc := `{"data": {"id": "inst-1", "type": "instance", "attributes": {
		"name": "Blog", "slug": "velvety-otter-1234", "status": 7,
		"exposure": "public", "always_on": false
	}}}`
	srv := testServer(t, http.StatusOK, doc)
	c := New(srv.URL, "fvpat_test", "test", srv.Client())

	if _, err := c.GetInstance(context.Background(), "inst-1"); err == nil || !strings.Contains(err.Error(), "status") {
		t.Fatalf("err = %v, want the status shape-drift error", err)
	}
}
