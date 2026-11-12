package provider

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func TestListAppsMapsTheWireFields(t *testing.T) {
	f := newFakeAPI(t)
	f.appsDoc = appsDoc

	diags := &diag.Diagnostics{}
	apps := listApps(context.Background(), f.testClient().client, diags)
	assertNoErrors(t, diags)

	if len(apps) != 2 {
		t.Fatalf("apps = %d rows, want 2", len(apps))
	}
	first := apps[0]
	if first.ID.ValueString() != "app-1" || first.Slug.ValueString() != "forgejo" ||
		first.Name.ValueString() != "Forgejo" {
		t.Errorf("apps[0] = %s/%s/%s, want app-1/forgejo/Forgejo",
			first.ID.ValueString(), first.Slug.ValueString(), first.Name.ValueString())
	}
	if first.Tagline.ValueString() != "Git forge" {
		t.Errorf("tagline = %s, want Git forge", first.Tagline.ValueString())
	}
	if first.FileAccess.ValueBool() {
		t.Errorf("file_access = true, want false")
	}
	// Absent optionals stay null — not empty strings.
	second := apps[1]
	if !second.Tagline.IsNull() || !second.Description.IsNull() {
		t.Errorf("sparse row tagline/description = %s/%s, want nulls",
			second.Tagline.ValueString(), second.Description.ValueString())
	}
	if !second.FileAccess.ValueBool() {
		t.Errorf("file_access = false, want true")
	}
}

func TestListAppVersionsSlugResolutionAndMapping(t *testing.T) {
	f := newFakeAPI(t)
	f.appDoc = appDoc
	f.versionsDoc = versionsDoc

	pd := f.testClient()
	diags := &diag.Diagnostics{}
	versions := listAppVersions(context.Background(), pd.client,
		types.StringValue("forgejo"), types.StringNull(), diags)
	assertNoErrors(t, diags)

	// The slug was resolved through the catalog, not guessed.
	if findOne(t, f.all(), "GET", "/api/v1/apps/forgejo").path == "" {
		t.Error("no GET /apps/forgejo — slug not resolved")
	}
	if findOne(t, f.all(), "GET", "/api/v1/apps/app-1/versions").path == "" {
		t.Error("no GET /apps/app-1/versions")
	}

	if len(versions) != 2 {
		t.Fatalf("versions = %d rows, want 2", len(versions))
	}
	first := versions[0]
	if first.ID.ValueString() != "ver-2" || first.AppID.ValueString() != "app-1" ||
		first.Version.ValueString() != "2.0" {
		t.Errorf("versions[0] = %s/%s/%s, want ver-2/app-1/2.0",
			first.ID.ValueString(), first.AppID.ValueString(), first.Version.ValueString())
	}
	// The decimal default arrives as a string on the wire and maps to a
	// float.
	if first.DefaultCPUCores.ValueFloat64() != 1.5 {
		t.Errorf("default_cpu_cores = %v, want 1.5", first.DefaultCPUCores.ValueFloat64())
	}
	if first.DefaultMemoryMB.ValueInt64() != 512 || first.DefaultStorageGB.ValueInt64() != 2 {
		t.Errorf("default mem/storage = %d/%d, want 512/2",
			first.DefaultMemoryMB.ValueInt64(), first.DefaultStorageGB.ValueInt64())
	}
	if got := envMapOf(t, first.DefaultEnvVars); got["FOO"] != "bar" {
		t.Errorf("default_env_vars = %v, want FOO=bar", got)
	}
	if first.ReleaseNotes.ValueString() != "new" || first.PublishedAt.ValueString() != "2026-02-01T00:00:00Z" {
		t.Errorf("release_notes/published_at = %s/%s, want the mapped values",
			first.ReleaseNotes.ValueString(), first.PublishedAt.ValueString())
	}

	second := versions[1]
	// Nulls on the wire stay null in the model — this must be able to
	// fail when the mapping fills them.
	if !second.ReleaseNotes.IsNull() || !second.PublishedAt.IsNull() {
		t.Errorf("nulls on the wire must stay null: release_notes=%v published_at=%v",
			second.ReleaseNotes, second.PublishedAt)
	}
	if second.DefaultStorageGB.ValueInt64() != 0 {
		t.Errorf("default_storage_gb = %d, want 0", second.DefaultStorageGB.ValueInt64())
	}
}

func TestListAppVersionsByIDSkipsTheSlugLookup(t *testing.T) {
	f := newFakeAPI(t)
	f.versionsDoc = versionsDoc

	pd := f.testClient()
	diags := &diag.Diagnostics{}
	versions := listAppVersions(context.Background(), pd.client,
		types.StringNull(), types.StringValue("app-1"), diags)
	assertNoErrors(t, diags)

	if len(versions) != 2 {
		t.Fatalf("versions = %d rows, want 2", len(versions))
	}
	for _, r := range f.all() {
		if strings.HasSuffix(r.path, "/apps/app-1") {
			t.Errorf("unexpected GET %s — the id form needs no catalog lookup", r.path)
		}
	}
}

func TestListAppVersionsInputArity(t *testing.T) {
	f := newFakeAPI(t)
	pd := f.testClient()

	t.Run("both forms", func(t *testing.T) {
		diags := &diag.Diagnostics{}
		listAppVersions(context.Background(), pd.client,
			types.StringValue("forgejo"), types.StringValue("app-1"), diags)
		assertError(t, diags, "exactly one")
	})

	t.Run("neither form", func(t *testing.T) {
		diags := &diag.Diagnostics{}
		listAppVersions(context.Background(), pd.client,
			types.StringNull(), types.StringNull(), diags)
		assertError(t, diags, "Set app or app_id")
	})
}

func TestListAppVersionsUnknownSlugSurfacesServer404(t *testing.T) {
	f := newFakeAPI(t)
	f.appStatus = 404
	f.appDoc = `{"errors": [{"title": "Not Found", "detail": "unknown app"}]}`

	pd := f.testClient()
	diags := &diag.Diagnostics{}
	listAppVersions(context.Background(), pd.client,
		types.StringValue("nope"), types.StringNull(), diags)
	assertError(t, diags, "unknown app")
}

// --- framework entry points (Configure + Read) ------------------------

// nullConfig builds a Config whose Raw is an all-null object of the
// schema's type, with the given strings set.
func nullConfig(ctx context.Context, t *testing.T, sch schema.Schema, set map[string]string) tfsdk.Config {
	t.Helper()
	objType := sch.Type().TerraformType(ctx).(tftypes.Object)
	vals := make(map[string]tftypes.Value, len(objType.AttributeTypes))
	for name, attrType := range objType.AttributeTypes {
		vals[name] = tftypes.NewValue(attrType, nil)
	}
	for name, s := range set {
		vals[name] = tftypes.NewValue(tftypes.String, s)
	}
	return tfsdk.Config{Schema: sch, Raw: tftypes.NewValue(objType, vals)}
}

func appsSchema(ctx context.Context, t *testing.T) schema.Schema {
	t.Helper()
	d := NewAppsDataSource().(*appsDataSource)
	resp := &datasource.SchemaResponse{}
	d.Schema(ctx, datasource.SchemaRequest{}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	return resp.Schema
}

func appVersionsSchema(ctx context.Context, t *testing.T) schema.Schema {
	t.Helper()
	d := NewAppVersionsDataSource().(*appVersionsDataSource)
	resp := &datasource.SchemaResponse{}
	d.Schema(ctx, datasource.SchemaRequest{}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	return resp.Schema
}

func instanceSchema(ctx context.Context, t *testing.T) schema.Schema {
	t.Helper()
	d := NewInstanceDataSource().(*instanceDataSource)
	resp := &datasource.SchemaResponse{}
	d.Schema(ctx, datasource.SchemaRequest{}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	return resp.Schema
}

// readInstanceDataSource drives one Read through the framework entry
// point with an all-null config plus the given string attributes.
func readInstanceDataSource(t *testing.T, d *instanceDataSource, sch schema.Schema, set map[string]string) *datasource.ReadResponse {
	t.Helper()
	ctx := context.Background()
	resp := &datasource.ReadResponse{
		State: tfsdk.State{Schema: sch, Raw: nullObject(ctx, t, sch.Type().TerraformType(ctx))},
	}
	d.Read(ctx, datasource.ReadRequest{Config: nullConfig(ctx, t, sch, set)}, resp)
	return resp
}

func TestInstanceDataSourceByID(t *testing.T) {
	ctx := context.Background()
	sch := instanceSchema(ctx, t)
	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{script(200, runningDoc)}

	d := NewInstanceDataSource().(*instanceDataSource)
	cfgResp := &datasource.ConfigureResponse{}
	d.Configure(ctx, datasource.ConfigureRequest{ProviderData: f.testClient()}, cfgResp)
	assertNoErrors(t, &cfgResp.Diagnostics)

	resp := readInstanceDataSource(t, d, sch, map[string]string{
		"id":           "inst-1",
		"organization": "acme",
	})
	assertNoErrors(t, &resp.Diagnostics)

	var got instanceDataSourceModel
	if diags := resp.State.Get(ctx, &got); diags.HasError() {
		t.Fatalf("State.Get: %v", diags)
	}
	if got.ID.ValueString() != "inst-1" || got.Slug.ValueString() != "velvety-otter-1234" {
		t.Errorf("id/slug = %q/%q, want the wire's identity", got.ID.ValueString(), got.Slug.ValueString())
	}
	if got.Name.ValueString() != "Blog" || got.Status.ValueString() != "running" ||
		got.Exposure.ValueString() != "public" || got.AlwaysOn.ValueBool() {
		t.Errorf("name/status/exposure/always_on = %q/%q/%q/%t, want Blog/running/public/false",
			got.Name.ValueString(), got.Status.ValueString(), got.Exposure.ValueString(), got.AlwaysOn.ValueBool())
	}
	if got.Namespace.ValueString() != "fluxvale-app-inst-1" || got.Image.ValueString() != "reg/app:1.0" {
		t.Errorf("namespace/image = %q/%q, want the detail document's values",
			got.Namespace.ValueString(), got.Image.ValueString())
	}
	if got.Port.ValueInt64() != 3000 || got.DeployedAt.ValueString() != "2026-11-06T10:00:00Z" {
		t.Errorf("port/deployed_at = %d/%q, want 3000/the detail document's timestamp",
			got.Port.ValueInt64(), got.DeployedAt.ValueString())
	}
	// The sparse fieldset's FK ids fill the computed pair.
	if got.OrganizationID.ValueString() != "org-1" || got.AppVersionID.ValueString() != "ver-1" {
		t.Errorf("organization_id/app_version_id = %q/%q, want org-1/ver-1",
			got.OrganizationID.ValueString(), got.AppVersionID.ValueString())
	}

	req := findOne(t, f.all(), "GET", "/api/v1/instances/inst-1")
	if fields := req.query.Get("fields[instance]"); !strings.Contains(fields, "organization_id") ||
		!strings.Contains(fields, "app_version_id") {
		t.Errorf("fields[instance] = %q, want the detail set plus both FK ids", fields)
	}
	if _, ok := req.query["organization_id"]; ok {
		t.Error("organization_id sent on an id lookup — the id form is unique")
	}
	// The id form must not resolve the configured organization at all.
	for _, r := range f.all() {
		if strings.HasSuffix(r.path, "/organizations") {
			t.Errorf("GET %s — the id form ignores the organization attribute", r.path)
		}
	}
}

func TestInstanceDataSourceBySlug(t *testing.T) {
	ctx := context.Background()
	sch := instanceSchema(ctx, t)
	f := newFakeAPI(t)
	f.instanceBySlugDoc = runningDoc

	d := NewInstanceDataSource().(*instanceDataSource)
	cfgResp := &datasource.ConfigureResponse{}
	d.Configure(ctx, datasource.ConfigureRequest{ProviderData: f.testClient()}, cfgResp)
	assertNoErrors(t, &cfgResp.Diagnostics)

	resp := readInstanceDataSource(t, d, sch, map[string]string{"slug": "velvety-otter-1234"})
	assertNoErrors(t, &resp.Diagnostics)

	var got instanceDataSourceModel
	if diags := resp.State.Get(ctx, &got); diags.HasError() {
		t.Fatalf("State.Get: %v", diags)
	}
	if got.ID.ValueString() != "inst-1" || got.Slug.ValueString() != "velvety-otter-1234" ||
		got.Name.ValueString() != "Blog" || got.Status.ValueString() != "running" {
		t.Errorf("lookup result = %s/%s/%s/%s, want inst-1/velvety-otter-1234/Blog/running",
			got.ID.ValueString(), got.Slug.ValueString(), got.Name.ValueString(), got.Status.ValueString())
	}
	if got.OrganizationID.ValueString() != "org-1" || got.AppVersionID.ValueString() != "ver-1" {
		t.Errorf("organization_id/app_version_id = %q/%q, want org-1/ver-1",
			got.OrganizationID.ValueString(), got.AppVersionID.ValueString())
	}

	req := findOne(t, f.all(), "GET", "/api/v1/instances/by-slug/velvety-otter-1234")
	if fields := req.query.Get("fields[instance]"); !strings.Contains(fields, "organization_id") ||
		!strings.Contains(fields, "app_version_id") {
		t.Errorf("fields[instance] = %q, want the detail set plus both FK ids", fields)
	}
	if _, ok := req.query["organization_id"]; ok {
		t.Error("organization_id sent with neither the attribute nor the provider default set")
	}
	for _, r := range f.all() {
		if strings.HasSuffix(r.path, "/organizations") {
			t.Errorf("GET %s — no org context was configured, so nothing should be resolved", r.path)
		}
	}
}

func TestInstanceDataSourceBySlugWithOrganization(t *testing.T) {
	ctx := context.Background()
	sch := instanceSchema(ctx, t)

	t.Run("the data source attribute resolves to the org id", func(t *testing.T) {
		f := newFakeAPI(t)
		f.instanceBySlugDoc = runningDoc
		f.orgsDoc = orgsDoc

		d := NewInstanceDataSource().(*instanceDataSource)
		cfgResp := &datasource.ConfigureResponse{}
		d.Configure(ctx, datasource.ConfigureRequest{ProviderData: f.testClient()}, cfgResp)
		assertNoErrors(t, &cfgResp.Diagnostics)

		resp := readInstanceDataSource(t, d, sch, map[string]string{
			"slug":         "velvety-otter-1234",
			"organization": "acme",
		})
		assertNoErrors(t, &resp.Diagnostics)

		findOne(t, f.all(), "GET", "/api/v1/organizations")
		req := findOne(t, f.all(), "GET", "/api/v1/instances/by-slug/velvety-otter-1234")
		if got := req.query.Get("organization_id"); got != "org-1" {
			t.Errorf("organization_id = %q, want org-1 (acme resolved to its id)", got)
		}
	})

	t.Run("the provider default is the fallback", func(t *testing.T) {
		f := newFakeAPI(t)
		f.instanceBySlugDoc = runningDoc
		f.orgsDoc = orgsDoc

		pd := f.testClient()
		pd.organization = types.StringValue("b-co")
		d := NewInstanceDataSource().(*instanceDataSource)
		cfgResp := &datasource.ConfigureResponse{}
		d.Configure(ctx, datasource.ConfigureRequest{ProviderData: pd}, cfgResp)
		assertNoErrors(t, &cfgResp.Diagnostics)

		resp := readInstanceDataSource(t, d, sch, map[string]string{"slug": "velvety-otter-1234"})
		assertNoErrors(t, &resp.Diagnostics)

		req := findOne(t, f.all(), "GET", "/api/v1/instances/by-slug/velvety-otter-1234")
		if got := req.query.Get("organization_id"); got != "org-2" {
			t.Errorf("organization_id = %q, want org-2 (the provider-level default)", got)
		}
	})
}

func TestInstanceDataSourceExactlyOneLookup(t *testing.T) {
	ctx := context.Background()
	sch := instanceSchema(ctx, t)

	cases := []struct {
		name string
		set  map[string]string
	}{
		{name: "both forms", set: map[string]string{"id": "inst-1", "slug": "velvety-otter-1234"}},
		{name: "neither form", set: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAPI(t)
			d := NewInstanceDataSource().(*instanceDataSource)
			cfgResp := &datasource.ConfigureResponse{}
			d.Configure(ctx, datasource.ConfigureRequest{ProviderData: f.testClient()}, cfgResp)
			assertNoErrors(t, &cfgResp.Diagnostics)

			resp := readInstanceDataSource(t, d, sch, tc.set)
			assertError(t, &resp.Diagnostics, "exactly one", "id", "slug")
			if reqs := f.all(); len(reqs) != 0 {
				t.Errorf("requests = %+v, want none — the arity refusal is client-side", reqs)
			}
		})
	}
}

func TestInstanceDataSourceNotFound(t *testing.T) {
	ctx := context.Background()
	sch := instanceSchema(ctx, t)

	t.Run("by slug", func(t *testing.T) {
		f := newFakeAPI(t)
		f.instanceBySlugStatus = http.StatusNotFound
		f.instanceBySlugDoc = `{"errors": [{"title": "Not Found"}]}`

		d := NewInstanceDataSource().(*instanceDataSource)
		cfgResp := &datasource.ConfigureResponse{}
		d.Configure(ctx, datasource.ConfigureRequest{ProviderData: f.testClient()}, cfgResp)
		assertNoErrors(t, &cfgResp.Diagnostics)

		resp := readInstanceDataSource(t, d, sch, map[string]string{"slug": "no-such-slug"})
		assertError(t, &resp.Diagnostics, "Instance not found", "no-such-slug")
	})

	t.Run("by id", func(t *testing.T) {
		f := newFakeAPI(t)
		f.instanceGets = []scriptedResponse{script(http.StatusNotFound, `{"errors": [{"title": "Not Found"}]}`)}

		d := NewInstanceDataSource().(*instanceDataSource)
		cfgResp := &datasource.ConfigureResponse{}
		d.Configure(ctx, datasource.ConfigureRequest{ProviderData: f.testClient()}, cfgResp)
		assertNoErrors(t, &cfgResp.Diagnostics)

		resp := readInstanceDataSource(t, d, sch, map[string]string{"id": "gone"})
		assertError(t, &resp.Diagnostics, "Instance not found", "gone")
	})
}

// A pending row's deploy-derived fields are null on the wire — the data
// source carries nulls, never an error.
func TestInstanceDataSourcePendingRowNulls(t *testing.T) {
	ctx := context.Background()
	sch := instanceSchema(ctx, t)
	f := newFakeAPI(t)
	f.instanceBySlugDoc = createResponse

	d := NewInstanceDataSource().(*instanceDataSource)
	cfgResp := &datasource.ConfigureResponse{}
	d.Configure(ctx, datasource.ConfigureRequest{ProviderData: f.testClient()}, cfgResp)
	assertNoErrors(t, &cfgResp.Diagnostics)

	resp := readInstanceDataSource(t, d, sch, map[string]string{"slug": "velvety-otter-1234"})
	assertNoErrors(t, &resp.Diagnostics)

	var got instanceDataSourceModel
	if diags := resp.State.Get(ctx, &got); diags.HasError() {
		t.Fatalf("State.Get: %v", diags)
	}
	if got.Status.ValueString() != "pending" {
		t.Errorf("status = %q, want pending", got.Status.ValueString())
	}
	if !got.Namespace.IsNull() || !got.DeployedAt.IsNull() {
		t.Errorf("namespace/deployed_at = %v/%v, want null on a pending row", got.Namespace, got.DeployedAt)
	}
	// The create response's route-default field set omits the FK ids —
	// absent parses as null, not drift.
	if !got.OrganizationID.IsNull() || !got.AppVersionID.IsNull() {
		t.Errorf("organization_id/app_version_id = %v/%v, want null when absent",
			got.OrganizationID, got.AppVersionID)
	}
	if got.Image.ValueString() != "reg/app:1.0" || got.Port.ValueInt64() != 3000 {
		t.Errorf("image/port = %q/%d, want the create response's values",
			got.Image.ValueString(), got.Port.ValueInt64())
	}
}

func TestAppsDataSourceConfigureAndRead(t *testing.T) {
	ctx := context.Background()
	sch := appsSchema(ctx, t)

	t.Run("wrong provider data type is an error", func(t *testing.T) {
		d := NewAppsDataSource().(*appsDataSource)
		resp := &datasource.ConfigureResponse{}
		d.Configure(ctx, datasource.ConfigureRequest{ProviderData: "nope"}, resp)
		assertError(t, &resp.Diagnostics, "Unexpected provider data type")
	})

	t.Run("read without configure is an error", func(t *testing.T) {
		d := NewAppsDataSource().(*appsDataSource)
		resp := &datasource.ReadResponse{
			State: tfsdk.State{Schema: sch, Raw: nullObject(ctx, t, sch.Type().TerraformType(ctx))},
		}
		d.Read(ctx, datasource.ReadRequest{Config: nullConfig(ctx, t, sch, nil)}, resp)
		assertError(t, &resp.Diagnostics, "Provider not configured")
	})

	t.Run("read end to end", func(t *testing.T) {
		f := newFakeAPI(t)
		f.appsDoc = appsDoc
		d := NewAppsDataSource().(*appsDataSource)
		cfgResp := &datasource.ConfigureResponse{}
		d.Configure(ctx, datasource.ConfigureRequest{ProviderData: f.testClient()}, cfgResp)
		assertNoErrors(t, &cfgResp.Diagnostics)

		resp := &datasource.ReadResponse{
			State: tfsdk.State{Schema: sch, Raw: nullObject(ctx, t, sch.Type().TerraformType(ctx))},
		}
		d.Read(ctx, datasource.ReadRequest{Config: nullConfig(ctx, t, sch, nil)}, resp)
		assertNoErrors(t, &resp.Diagnostics)

		var got appsModel
		if diags := resp.State.Get(ctx, &got); diags.HasError() {
			t.Fatalf("State.Get: %v", diags)
		}
		if len(got.Apps) != 2 || got.Apps[0].Slug.ValueString() != "forgejo" {
			t.Fatalf("apps = %+v, want the catalog rows", got.Apps)
		}
	})
}

func TestAppVersionsDataSourceConfigureAndRead(t *testing.T) {
	ctx := context.Background()
	sch := appVersionsSchema(ctx, t)

	t.Run("wrong provider data type is an error", func(t *testing.T) {
		d := NewAppVersionsDataSource().(*appVersionsDataSource)
		resp := &datasource.ConfigureResponse{}
		d.Configure(ctx, datasource.ConfigureRequest{ProviderData: 42}, resp)
		assertError(t, &resp.Diagnostics, "Unexpected provider data type")
	})

	t.Run("read end to end via the slug form", func(t *testing.T) {
		f := newFakeAPI(t)
		f.appDoc = appDoc
		f.versionsDoc = versionsDoc
		d := NewAppVersionsDataSource().(*appVersionsDataSource)
		cfgResp := &datasource.ConfigureResponse{}
		d.Configure(ctx, datasource.ConfigureRequest{ProviderData: f.testClient()}, cfgResp)
		assertNoErrors(t, &cfgResp.Diagnostics)

		resp := &datasource.ReadResponse{
			State: tfsdk.State{Schema: sch, Raw: nullObject(ctx, t, sch.Type().TerraformType(ctx))},
		}
		d.Read(ctx, datasource.ReadRequest{Config: nullConfig(ctx, t, sch, map[string]string{"app": "forgejo"})}, resp)
		assertNoErrors(t, &resp.Diagnostics)

		var got appVersionsModel
		if diags := resp.State.Get(ctx, &got); diags.HasError() {
			t.Fatalf("State.Get: %v", diags)
		}
		if len(got.AppVersions) != 2 || got.AppVersions[0].Version.ValueString() != "2.0" {
			t.Fatalf("versions = %+v, want the two scripted rows", got.AppVersions)
		}
	})
}
