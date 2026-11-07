package provider

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// newSchemadResource builds the instance resource with its schema, the
// way the proto6 unit-test pattern does (provider_test.go's configure).
func newSchemadResource(ctx context.Context, t *testing.T) (*instanceResource, schema.Schema) {
	t.Helper()
	r := NewInstanceResource().(*instanceResource)
	schemaResp := &resource.SchemaResponse{}
	r.Schema(ctx, resource.SchemaRequest{}, schemaResp)
	if schemaResp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", schemaResp.Diagnostics)
	}
	return r, schemaResp.Schema
}

// nullObject is a null object value of the schema's type — the blank
// slate State/Plan/Config start from. Works for resource and data
// source schemas alike.
func nullObject(ctx context.Context, t *testing.T, sch tftypes.Type) tftypes.Value {
	t.Helper()
	return tftypes.NewValue(sch, nil)
}

// modelToRaw round-trips a model through a State to get the tftypes
// value the request structs need.
func modelToRaw(ctx context.Context, t *testing.T, sch schema.Schema, data *instanceModel) tftypes.Value {
	t.Helper()
	st := tfsdk.State{Schema: sch, Raw: nullObject(ctx, t, sch.Type().TerraformType(ctx))}
	if diags := st.Set(ctx, data); diags.HasError() {
		t.Fatalf("State.Set: %v", diags)
	}
	return st.Raw
}

// rawToModel reads state back into a model.
func rawToModel(ctx context.Context, t *testing.T, sch schema.Schema, raw tftypes.Value) instanceModel {
	t.Helper()
	st := tfsdk.State{Schema: sch, Raw: raw}
	var data instanceModel
	if diags := st.Get(ctx, &data); diags.HasError() {
		t.Fatalf("State.Get: %v", diags)
	}
	return data
}

// The full Create wrapper: Plan.Get → createInstance → State.Set, over
// a scripted API. This is the glue the factored functions don't cover.
func TestCreateWrapperEndToEnd(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadResource(ctx, t)

	f := newFakeAPI(t)
	f.orgsDoc = `{"data": [{"id": "org-1", "type": "organization", "attributes": {"slug": "acme", "name": "Acme"}}]}`
	f.postResponse = createResponse
	f.instanceGets = []scriptedResponse{script(200, runningDoc)}
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
	if resp.Diagnostics.HasError() {
		t.Fatalf("Create: %v", resp.Diagnostics)
	}

	got := rawToModel(ctx, t, sch, resp.State.Raw)
	if got.ID.ValueString() != "inst-1" || got.Status.ValueString() != "running" {
		t.Errorf("state id/status = %s/%s, want inst-1/running",
			got.ID.ValueString(), got.Status.ValueString())
	}
	if got.EnvVars.IsNull() || envMapOf(t, got.EnvVars)["API_KEY"] != "sekrit" {
		t.Errorf("state env_vars = %v, want the configured map carried into state", got.EnvVars)
	}
	if got.AlwaysOn.IsNull() || got.Exposure.IsNull() {
		t.Error("computed always_on/exposure not set from the server's values")
	}

	// The wire saw the create body shape.
	attrs := attrsOf(t, findOne(t, f.all(), http.MethodPost, "/api/v1/instances"))
	if attrs["cpu_cores"] != "0.5" {
		t.Errorf("cpu_cores = %v, want the decimal string", attrs["cpu_cores"])
	}
}

// Read over a 404 removes the resource from state (Raw goes null) —
// the removed-from-state path through the wrapper.
func TestReadWrapperRemovesOn404(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadResource(ctx, t)

	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{script(http.StatusNotFound, `{"errors": [{"title": "Not Found"}]}`)}
	r.pd = f.testClient()

	stateData := testInstance(func(d *instanceModel) { d.ID = types.StringValue("gone") })
	req := resource.ReadRequest{
		State: tfsdk.State{Schema: sch, Raw: modelToRaw(ctx, t, sch, &stateData)},
	}
	resp := &resource.ReadResponse{
		State: tfsdk.State{Schema: sch, Raw: modelToRaw(ctx, t, sch, &stateData)},
	}
	r.Read(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsNull() {
		t.Errorf("state after 404 = %v, want null (removed)", resp.State.Raw)
	}
}

// Delete through the wrapper: clean diagnostics over a scripted
// teardown, no state rewrite.
func TestDeleteWrapperEndToEnd(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadResource(ctx, t)

	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{
		script(200, strings.ReplaceAll(runningDoc, `"running"`, `"deleting"`)),
		script(http.StatusNotFound, `{"errors": [{"title": "Not Found"}]}`),
	}
	r.pd = f.testClient()

	stateData := testInstance(func(d *instanceModel) { d.ID = types.StringValue("inst-1") })
	req := resource.DeleteRequest{
		State: tfsdk.State{Schema: sch, Raw: modelToRaw(ctx, t, sch, &stateData)},
	}
	resp := &resource.DeleteResponse{}
	r.Delete(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Delete: %v", resp.Diagnostics)
	}
	findOne(t, f.all(), http.MethodDelete, "/api/v1/instances/inst-1")
}

// The provider registers the slice's resources and data sources under
// the compatibility contract's type names.
func TestProviderRegistrations(t *testing.T) {
	ctx := context.Background()
	p := New("test")()

	wantResources := map[string]bool{
		"fluxvale_instance":         false,
		"fluxvale_instance_backups": false,
		"fluxvale_access_gateway":   false,
	}
	resources := p.Resources(ctx)
	if len(resources) != len(wantResources) {
		t.Fatalf("Resources = %d, want %d", len(resources), len(wantResources))
	}
	for _, factory := range resources {
		md := &resource.MetadataResponse{}
		factory().Metadata(ctx, resource.MetadataRequest{ProviderTypeName: "fluxvale"}, md)
		if _, ok := wantResources[md.TypeName]; !ok {
			t.Errorf("unexpected resource TypeName %q", md.TypeName)
		}
		wantResources[md.TypeName] = true
	}
	for name, seen := range wantResources {
		if !seen {
			t.Errorf("resource %q not registered", name)
		}
	}

	wantSources := map[string]bool{"fluxvale_apps": false, "fluxvale_app_versions": false}
	sources := p.DataSources(ctx)
	if len(sources) != len(wantSources) {
		t.Fatalf("DataSources = %d, want %d", len(sources), len(wantSources))
	}
	for _, factory := range sources {
		md := &datasource.MetadataResponse{}
		factory().Metadata(ctx, datasource.MetadataRequest{ProviderTypeName: "fluxvale"}, md)
		if _, ok := wantSources[md.TypeName]; !ok {
			t.Errorf("unexpected data source TypeName %q", md.TypeName)
		}
		wantSources[md.TypeName] = true
	}
	for name, seen := range wantSources {
		if !seen {
			t.Errorf("data source %q not registered", name)
		}
	}
}

// The schema contract: every attribute the compatibility contract and
// the wire pin. The identity set requires replacement (the API has no
// rename/version-change/org-change routes); the allocation set is
// IN-PLACE (the stopped-first resize, #493) — a RequiresReplace there
// would be a regression against finding 1 of the review round.
func TestInstanceSchemaContract(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadResource(ctx, t)

	want := []string{
		"organization", "organization_id", "name", "app_version_id", "app", "app_version",
		"cpu_cores", "memory_mb", "storage_gb", "env_vars",
		"running", "always_on", "exposure",
		"id", "slug", "namespace", "status", "port", "image", "deployed_at",
	}
	if len(sch.Attributes) != len(want) {
		t.Fatalf("schema attributes = %d, want exactly %d (%v)", len(sch.Attributes), len(want), want)
	}
	for _, name := range want {
		if _, ok := sch.Attributes[name]; !ok {
			t.Errorf("schema missing attribute %q", name)
		}
	}

	// Required allocation + identity; identity-only RequiresReplace.
	required := []string{"name", "cpu_cores", "memory_mb", "storage_gb"}
	for _, name := range required {
		if !sch.Attributes[name].IsRequired() {
			t.Errorf("%s is not Required", name)
		}
	}
	replaceable := map[string]func() bool{
		"organization": func() bool {
			return len(sch.Attributes["organization"].(schema.StringAttribute).PlanModifiers) > 0
		},
		"name": func() bool {
			return len(sch.Attributes["name"].(schema.StringAttribute).PlanModifiers) > 0
		},
		"app_version_id": func() bool {
			return len(sch.Attributes["app_version_id"].(schema.StringAttribute).PlanModifiers) > 0
		},
		"env_vars": func() bool {
			return len(sch.Attributes["env_vars"].(schema.MapAttribute).PlanModifiers) > 0
		},
	}
	for name, hasPlanModifiers := range replaceable {
		if !hasPlanModifiers() {
			t.Errorf("%s has no plan modifiers (want RequiresReplace — create-only on the wire)", name)
		}
	}

	// The in-place set: allocation resizes (PATCH resize — stopped
	// first); running/always_on/exposure flip via their verbs.
	for _, name := range []string{"cpu_cores", "memory_mb", "storage_gb"} {
		switch a := sch.Attributes[name].(type) {
		case schema.Float64Attribute:
			if len(a.PlanModifiers) != 0 {
				t.Errorf("%s must resize in place (PATCH resize) — no plan modifiers", name)
			}
		case schema.Int64Attribute:
			if len(a.PlanModifiers) != 0 {
				t.Errorf("%s must resize in place (PATCH resize) — no plan modifiers", name)
			}
		}
	}
	if len(sch.Attributes["always_on"].(schema.BoolAttribute).PlanModifiers) != 0 {
		t.Error("always_on must update in place (PATCH always-on)")
	}
	if len(sch.Attributes["exposure"].(schema.StringAttribute).PlanModifiers) != 0 {
		t.Error("exposure must update in place (PATCH exposure)")
	}
	if sch.Attributes["running"].(schema.BoolAttribute).PlanModifiers != nil {
		t.Error("running must update in place (PATCH stop/start)")
	}

	// env_vars is sensitive (credentials ride it) and never computed.
	if !sch.Attributes["env_vars"].IsSensitive() {
		t.Error("env_vars is not sensitive")
	}

	// organization_id is the computed FK mirror; app_version_id is
	// Optional+Computed (config-or-resolved).
	if !sch.Attributes["organization_id"].(schema.StringAttribute).Computed {
		t.Error("organization_id must be Computed (the wire's resolved id)")
	}
	av := sch.Attributes["app_version_id"].(schema.StringAttribute)
	if !av.Optional || !av.Computed {
		t.Error("app_version_id must be Optional+Computed (config-or-resolved)")
	}

	// Metadata: the compatibility contract's type name.
	md := &resource.MetadataResponse{}
	r.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: "fluxvale"}, md)
	if md.TypeName != "fluxvale_instance" {
		t.Errorf("TypeName = %q, want fluxvale_instance", md.TypeName)
	}
}

// The resource's Configure arms: the providerData type-assertion both
// ways.
func TestInstanceResourceConfigureArms(t *testing.T) {
	ctx := context.Background()
	f := newFakeAPI(t)

	r := NewInstanceResource().(*instanceResource)
	resp := &resource.ConfigureResponse{}
	r.Configure(ctx, resource.ConfigureRequest{ProviderData: f.testClient()}, resp)
	assertNoErrors(t, &resp.Diagnostics)
	if r.pd == nil {
		t.Fatal("Configure with *providerData left r.pd nil")
	}

	r2 := NewInstanceResource().(*instanceResource)
	resp2 := &resource.ConfigureResponse{}
	r2.Configure(ctx, resource.ConfigureRequest{ProviderData: "nope"}, resp2)
	assertError(t, &resp2.Diagnostics, "Unexpected provider data type")
}
