package provider

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

const (
	orgDoc = `{"data":{"id":"org-1","type":"organization","attributes":{"slug":"acme","name":"Acme"}}}`

	orgRenamedDoc = `{"data":{"id":"org-1","type":"organization","attributes":{"slug":"acme","name":"Renamed"}}}`

	orgGuardDoc = `{"errors":[{"status":"400","detail":"This organization still has 1 instance. Delete it first."}]}`
)

func newSchemaOrg(ctx context.Context, t *testing.T) (*organizationResource, schema.Schema) {
	t.Helper()
	r := NewOrganizationResource().(*organizationResource)
	schemaResp := &resource.SchemaResponse{}
	r.Schema(ctx, resource.SchemaRequest{}, schemaResp)
	if schemaResp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", schemaResp.Diagnostics)
	}
	return r, schemaResp.Schema
}

func testOrganization(mutators ...func(*organizationModel)) organizationModel {
	data := organizationModel{
		Name: types.StringValue("Acme"),
		Slug: types.StringValue("acme"),
		ID:   types.StringNull(),
	}
	for _, mutate := range mutators {
		mutate(&data)
	}
	return data
}

func orgToRaw(ctx context.Context, t *testing.T, sch schema.Schema, data *organizationModel) tftypes.Value {
	t.Helper()
	st := tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)}
	if diags := st.Set(ctx, data); diags.HasError() {
		t.Fatalf("State.Set: %v", diags)
	}
	return st.Raw
}

func rawToOrg(ctx context.Context, t *testing.T, sch schema.Schema, raw tftypes.Value) organizationModel {
	t.Helper()
	st := tfsdk.State{Schema: sch, Raw: raw}
	var data organizationModel
	if diags := st.Get(ctx, &data); diags.HasError() {
		t.Fatalf("State.Get: %v", diags)
	}
	return data
}

func TestOrganizationSchema(t *testing.T) {
	ctx := context.Background()
	_, sch := newSchemaOrg(ctx, t)

	if !sch.Attributes["name"].IsRequired() {
		t.Error("name = optional, want required")
	}
	if !sch.Attributes["slug"].IsRequired() {
		t.Error("slug = optional, want required")
	}
	if !sch.Attributes["id"].IsComputed() {
		t.Error("id = not computed, want computed")
	}
	if !strings.Contains(sch.Description, "prevent_destroy") {
		t.Error("schema description does not recommend prevent_destroy")
	}
	if !strings.Contains(sch.Description, "wallet row") {
		t.Error("schema description does not name the wallet-row retention trigger")
	}

	// slug: a changed value must require replacement (the plan gate
	// then rejects it).
	stateData := testOrganization(func(d *organizationModel) { d.ID = types.StringValue("org-1") })
	slugPlan := testOrganization(func(d *organizationModel) {
		d.ID = types.StringValue("org-1")
		d.Slug = types.StringValue("other")
	})
	state := tfsdk.State{Schema: sch, Raw: orgToRaw(ctx, t, sch, &stateData)}
	plan := tfsdk.Plan{Schema: sch, Raw: orgToRaw(ctx, t, sch, &slugPlan)}
	slugAttr := sch.Attributes["slug"].(schema.StringAttribute)
	slugReq := planmodifier.StringRequest{
		State:       state,
		Plan:        plan,
		StateValue:  types.StringValue("acme"),
		PlanValue:   types.StringValue("other"),
		ConfigValue: types.StringValue("other"),
	}
	slugResp := &planmodifier.StringResponse{PlanValue: types.StringValue("other")}
	for _, m := range slugAttr.PlanModifiers {
		m.PlanModifyString(ctx, slugReq, slugResp)
	}
	if !slugResp.RequiresReplace {
		t.Error("slug plan modifiers do not set RequiresReplace")
	}

	// id: an unknown plan value must resolve from state (in-place
	// updates and outputs keep the known id).
	idPlan := testOrganization(func(d *organizationModel) { d.ID = types.StringUnknown() })
	idState := tfsdk.State{Schema: sch, Raw: orgToRaw(ctx, t, sch, &stateData)}
	idPlanValue := tfsdk.Plan{Schema: sch, Raw: orgToRaw(ctx, t, sch, &idPlan)}
	idAttr := sch.Attributes["id"].(schema.StringAttribute)
	idReq := planmodifier.StringRequest{
		State:       idState,
		Plan:        idPlanValue,
		StateValue:  types.StringValue("org-1"),
		PlanValue:   types.StringUnknown(),
		ConfigValue: types.StringNull(),
	}
	idResp := &planmodifier.StringResponse{PlanValue: types.StringUnknown()}
	for _, m := range idAttr.PlanModifiers {
		m.PlanModifyString(ctx, idReq, idResp)
	}
	if idResp.PlanValue.ValueString() != "org-1" {
		t.Errorf("id plan value = %s, want the state id", idResp.PlanValue)
	}
}

func TestOrganizationConfigure(t *testing.T) {
	ctx := context.Background()
	r := NewOrganizationResource().(*organizationResource)

	// Provider data absent: the framework calls Configure before data is
	// available; the resource stays unconfigured and reports on use.
	resp := &resource.ConfigureResponse{}
	r.Configure(ctx, resource.ConfigureRequest{}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Configure(nil): %v", resp.Diagnostics)
	}
	if r.pd != nil {
		t.Error("Configure(nil) stored provider data")
	}

	// Wrong type: a provider bug, reported loudly.
	resp = &resource.ConfigureResponse{}
	r.Configure(ctx, resource.ConfigureRequest{ProviderData: "nope"}, resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("Configure(wrong type): want a diagnostic")
	}
	assertError(t, &resp.Diagnostics, "Unexpected provider data type")

	// The real type configures the client.
	pd := newFakeAPI(t).testClient()
	resp = &resource.ConfigureResponse{}
	r.Configure(ctx, resource.ConfigureRequest{ProviderData: pd}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Configure(providerData): %v", resp.Diagnostics)
	}
	if r.pd != pd {
		t.Error("Configure did not store the provider data")
	}
}

func TestOrganizationCreateBodyAndState(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemaOrg(ctx, t)

	f := newFakeAPI(t)
	f.orgPostDoc = orgDoc
	r.pd = f.testClient()

	plan := testOrganization()
	req := resource.CreateRequest{Plan: tfsdk.Plan{Schema: sch, Raw: orgToRaw(ctx, t, sch, &plan)}}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
	}
	r.Create(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Create: %v", resp.Diagnostics)
	}

	post := findOne(t, f.all(), http.MethodPost, "/api/v1/organizations")
	attrs := attrsOf(t, post)
	if attrs["name"] != "Acme" || attrs["slug"] != "acme" {
		t.Errorf("attrs = %v, want name/slug from the config", attrs)
	}
	if _, present := attrs["organization_id"]; present {
		t.Error("POST carried organization_id — the org resource is not org-scoped")
	}

	// resolveOrgID's list read is not part of the org resource: every
	// request is the create alone.
	for _, req := range f.all() {
		if req.method == http.MethodGet && req.path == "/api/v1/organizations" {
			t.Error("Create listed organizations — the provider-level default must not apply")
		}
	}

	got := rawToOrg(ctx, t, sch, resp.State.Raw)
	if got.ID.ValueString() != "org-1" || got.Name.ValueString() != "Acme" || got.Slug.ValueString() != "acme" {
		t.Errorf("state = %+v, want the wire's id/name/slug", got)
	}
}

func TestOrganizationCreateReservedSlugSurfacesVerbatim(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemaOrg(ctx, t)

	f := newFakeAPI(t)
	f.orgPostStatus = http.StatusBadRequest
	f.orgPostDoc = `{"errors":[{"status":"400","detail":"is reserved"}]}`
	r.pd = f.testClient()

	plan := testOrganization(func(d *organizationModel) { d.Slug = types.StringValue("docs") })
	req := resource.CreateRequest{Plan: tfsdk.Plan{Schema: sch, Raw: orgToRaw(ctx, t, sch, &plan)}}
	resp := &resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
	}
	r.Create(ctx, req, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("Create succeeded, want the reserved-slug refusal")
	}
	assertError(t, &resp.Diagnostics, "is reserved")
	assertError(t, &resp.Diagnostics, "status 400")
}

func TestOrganizationReadMapsAndRemovesOn404(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemaOrg(ctx, t)

	f := newFakeAPI(t)
	f.orgGets = []scriptedResponse{script(200, orgDoc)}
	r.pd = f.testClient()

	state := testOrganization(func(d *organizationModel) { d.ID = types.StringValue("org-1") })
	readResp := &resource.ReadResponse{
		State: tfsdk.State{Schema: sch, Raw: orgToRaw(ctx, t, sch, &state)},
	}
	r.Read(ctx, resource.ReadRequest{State: tfsdk.State{Schema: sch, Raw: orgToRaw(ctx, t, sch, &state)}}, readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", readResp.Diagnostics)
	}

	got := rawToOrg(ctx, t, sch, readResp.State.Raw)
	if got.ID.ValueString() != "org-1" || got.Name.ValueString() != "Acme" || got.Slug.ValueString() != "acme" {
		t.Errorf("state = %+v, want the wire's row", got)
	}
	findOne(t, f.all(), http.MethodGet, "/api/v1/organizations/org-1")

	f.orgGets = []scriptedResponse{script(404, `{"errors":[{"status":"404","detail":"gone"}]}`)}
	gone := testOrganization(func(d *organizationModel) { d.ID = types.StringValue("org-1") })
	goneResp := &resource.ReadResponse{
		State: tfsdk.State{Schema: sch, Raw: orgToRaw(ctx, t, sch, &gone)},
	}
	r.Read(ctx, resource.ReadRequest{State: tfsdk.State{Schema: sch, Raw: orgToRaw(ctx, t, sch, &gone)}}, goneResp)
	if goneResp.Diagnostics.HasError() {
		t.Fatalf("Read 404: %v", goneResp.Diagnostics)
	}
	if !goneResp.State.Raw.IsNull() {
		t.Error("state kept on 404, want removed (gone is gone)")
	}
}

func TestOrganizationRead403KeepsState(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemaOrg(ctx, t)

	f := newFakeAPI(t)
	f.orgGets = []scriptedResponse{script(403, `{"errors":[{"status":"403","detail":"forbidden"}]}`)}
	r.pd = f.testClient()

	state := testOrganization(func(d *organizationModel) { d.ID = types.StringValue("org-1") })
	resp := &resource.ReadResponse{
		State: tfsdk.State{Schema: sch, Raw: orgToRaw(ctx, t, sch, &state)},
	}
	r.Read(ctx, resource.ReadRequest{State: tfsdk.State{Schema: sch, Raw: orgToRaw(ctx, t, sch, &state)}}, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("Read 403 succeeded, want an error")
	}
	if resp.State.Raw.IsNull() {
		t.Error("state removed on 403, want kept — a lost membership must not forget the org")
	}
}

func TestOrganizationImportBySlugReadFillsIdentity(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemaOrg(ctx, t)

	f := newFakeAPI(t)
	f.orgBySlugGets = []scriptedResponse{script(200, orgDoc)}
	r.pd = f.testClient()

	// ImportState passthrough sets slug; id and name are still null.
	state := testOrganization(func(d *organizationModel) {
		d.Name = types.StringNull()
		d.ID = types.StringNull()
	})
	resp := &resource.ReadResponse{
		State: tfsdk.State{Schema: sch, Raw: orgToRaw(ctx, t, sch, &state)},
	}
	r.Read(ctx, resource.ReadRequest{State: tfsdk.State{Schema: sch, Raw: orgToRaw(ctx, t, sch, &state)}}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", resp.Diagnostics)
	}

	got := rawToOrg(ctx, t, sch, resp.State.Raw)
	if got.ID.ValueString() != "org-1" || got.Name.ValueString() != "Acme" {
		t.Errorf("state = %+v, want the slug lookup's row", got)
	}
	findOne(t, f.all(), http.MethodGet, "/api/v1/organizations/by-slug/acme")
}

func TestOrganizationImportStatePassesThroughSlug(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemaOrg(ctx, t)

	resp := &resource.ImportStateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)},
	}
	r.ImportState(ctx, resource.ImportStateRequest{ID: "acme"}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("ImportState: %v", resp.Diagnostics)
	}

	var slug types.String
	if diags := resp.State.GetAttribute(ctx, path.Root("slug"), &slug); diags.HasError() {
		t.Fatalf("GetAttribute: %v", diags)
	}
	if slug.ValueString() != "acme" {
		t.Errorf("slug = %q, want acme", slug.ValueString())
	}
}

func TestOrganizationUpdatePatchesNameOnly(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemaOrg(ctx, t)

	f := newFakeAPI(t)
	f.orgPatchDoc = orgRenamedDoc
	r.pd = f.testClient()

	state := testOrganization(func(d *organizationModel) { d.ID = types.StringValue("org-1") })
	plan := testOrganization(func(d *organizationModel) {
		d.ID = types.StringValue("org-1")
		d.Name = types.StringValue("Renamed")
	})
	req := resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: sch, Raw: orgToRaw(ctx, t, sch, &plan)},
		State: tfsdk.State{Schema: sch, Raw: orgToRaw(ctx, t, sch, &state)},
	}
	resp := &resource.UpdateResponse{
		State: tfsdk.State{Schema: sch, Raw: orgToRaw(ctx, t, sch, &state)},
	}
	r.Update(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", resp.Diagnostics)
	}

	patch := findOne(t, f.all(), http.MethodPatch, "/api/v1/organizations/org-1")
	attrs := attrsOf(t, patch)
	if attrs["name"] != "Renamed" {
		t.Errorf("name = %v, want Renamed", attrs["name"])
	}
	if _, present := attrs["slug"]; present {
		t.Error("PATCH carried slug — the provider exposes name only")
	}

	got := rawToOrg(ctx, t, sch, resp.State.Raw)
	if got.Name.ValueString() != "Renamed" {
		t.Errorf("state name = %q, want Renamed", got.Name.ValueString())
	}
}

func TestOrganizationModifyPlanRejectsSlugChange(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemaOrg(ctx, t)

	state := testOrganization(func(d *organizationModel) { d.ID = types.StringValue("org-1") })
	plan := testOrganization(func(d *organizationModel) {
		d.ID = types.StringValue("org-1")
		d.Slug = types.StringValue("other")
	})
	resp := &resource.ModifyPlanResponse{
		Plan: tfsdk.Plan{Schema: sch, Raw: orgToRaw(ctx, t, sch, &plan)},
	}
	r.ModifyPlan(ctx, resource.ModifyPlanRequest{
		Plan:  tfsdk.Plan{Schema: sch, Raw: orgToRaw(ctx, t, sch, &plan)},
		State: tfsdk.State{Schema: sch, Raw: orgToRaw(ctx, t, sch, &state)},
	}, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("ModifyPlan accepted a slug change, want rejection")
	}
	assertError(t, &resp.Diagnostics, "Replacing fluxvale_organization is rejected")
	assertError(t, &resp.Diagnostics, `"acme"`)
}

func TestOrganizationModifyPlanAllowsANameOnlyChange(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemaOrg(ctx, t)

	state := testOrganization(func(d *organizationModel) { d.ID = types.StringValue("org-1") })
	plan := testOrganization(func(d *organizationModel) {
		d.ID = types.StringValue("org-1")
		d.Name = types.StringValue("Renamed")
	})
	resp := &resource.ModifyPlanResponse{
		Plan: tfsdk.Plan{Schema: sch, Raw: orgToRaw(ctx, t, sch, &plan)},
	}
	r.ModifyPlan(ctx, resource.ModifyPlanRequest{
		Plan:  tfsdk.Plan{Schema: sch, Raw: orgToRaw(ctx, t, sch, &plan)},
		State: tfsdk.State{Schema: sch, Raw: orgToRaw(ctx, t, sch, &state)},
	}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("ModifyPlan(name-only): %v", resp.Diagnostics)
	}
}

func TestOrganizationModifyPlanHasNoStateOnCreateOrDestroy(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemaOrg(ctx, t)

	plan := testOrganization()
	// Create: no prior state.
	resp := &resource.ModifyPlanResponse{Plan: tfsdk.Plan{Schema: sch, Raw: orgToRaw(ctx, t, sch, &plan)}}
	r.ModifyPlan(ctx, resource.ModifyPlanRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: orgToRaw(ctx, t, sch, &plan)},
	}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("ModifyPlan(create): %v", resp.Diagnostics)
	}

	// Destroy: no plan.
	state := testOrganization(func(d *organizationModel) { d.ID = types.StringValue("org-1") })
	resp = &resource.ModifyPlanResponse{Plan: tfsdk.Plan{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)}}
	r.ModifyPlan(ctx, resource.ModifyPlanRequest{
		State: tfsdk.State{Schema: sch, Raw: orgToRaw(ctx, t, sch, &state)},
	}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("ModifyPlan(destroy): %v", resp.Diagnostics)
	}
}

func TestOrganizationUpdate404RemovesState(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemaOrg(ctx, t)

	f := newFakeAPI(t)
	f.orgPatchStatus = http.StatusNotFound
	f.orgPatchDoc = `{"errors":[{"status":"404","detail":"gone"}]}`
	r.pd = f.testClient()

	state := testOrganization(func(d *organizationModel) { d.ID = types.StringValue("org-1") })
	plan := testOrganization(func(d *organizationModel) {
		d.ID = types.StringValue("org-1")
		d.Name = types.StringValue("Renamed")
	})
	resp := &resource.UpdateResponse{
		State: tfsdk.State{Schema: sch, Raw: orgToRaw(ctx, t, sch, &state)},
	}
	r.Update(ctx, resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: sch, Raw: orgToRaw(ctx, t, sch, &plan)},
		State: tfsdk.State{Schema: sch, Raw: orgToRaw(ctx, t, sch, &state)},
	}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update 404: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsNull() {
		t.Error("state kept on update 404, want removed (the org is gone or hidden)")
	}
}

func TestOrganizationUpdateUsesStateIDWhenPlanIDIsUnknown(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemaOrg(ctx, t)

	f := newFakeAPI(t)
	f.orgPatchDoc = orgRenamedDoc
	r.pd = f.testClient()

	state := testOrganization(func(d *organizationModel) { d.ID = types.StringValue("org-1") })
	plan := testOrganization(func(d *organizationModel) {
		d.ID = types.StringUnknown()
		d.Name = types.StringValue("Renamed")
	})
	resp := &resource.UpdateResponse{
		State: tfsdk.State{Schema: sch, Raw: orgToRaw(ctx, t, sch, &state)},
	}
	r.Update(ctx, resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: sch, Raw: orgToRaw(ctx, t, sch, &plan)},
		State: tfsdk.State{Schema: sch, Raw: orgToRaw(ctx, t, sch, &state)},
	}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update unknown id: %v", resp.Diagnostics)
	}

	findOne(t, f.all(), http.MethodPatch, "/api/v1/organizations/org-1")
	got := rawToOrg(ctx, t, sch, resp.State.Raw)
	if got.ID.ValueString() != "org-1" {
		t.Errorf("state id = %q, want the state id", got.ID.ValueString())
	}
}

func TestOrganizationDeleteAnd404(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemaOrg(ctx, t)

	f := newFakeAPI(t)
	f.orgDeleteDoc = orgDoc
	r.pd = f.testClient()

	state := testOrganization(func(d *organizationModel) { d.ID = types.StringValue("org-1") })
	resp := &resource.DeleteResponse{
		State: tfsdk.State{Schema: sch, Raw: orgToRaw(ctx, t, sch, &state)},
	}
	r.Delete(ctx, resource.DeleteRequest{State: tfsdk.State{Schema: sch, Raw: orgToRaw(ctx, t, sch, &state)}}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Delete: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsNull() {
		t.Error("state kept after a successful delete")
	}
	findOne(t, f.all(), http.MethodDelete, "/api/v1/organizations/org-1")

	f.orgDeleteStatus = http.StatusNotFound
	f.orgDeleteDoc = `{"errors":[{"status":"404","detail":"gone"}]}`
	again := testOrganization(func(d *organizationModel) { d.ID = types.StringValue("org-1") })
	againResp := &resource.DeleteResponse{
		State: tfsdk.State{Schema: sch, Raw: orgToRaw(ctx, t, sch, &again)},
	}
	r.Delete(ctx, resource.DeleteRequest{State: tfsdk.State{Schema: sch, Raw: orgToRaw(ctx, t, sch, &again)}}, againResp)
	if againResp.Diagnostics.HasError() {
		t.Fatalf("Delete 404: %v", againResp.Diagnostics)
	}
	if !againResp.State.Raw.IsNull() {
		t.Error("state kept on delete 404, want idempotent removal")
	}
}

func TestOrganizationDeleteSurfacesGuardMessage(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemaOrg(ctx, t)

	f := newFakeAPI(t)
	f.orgDeleteStatus = http.StatusBadRequest
	f.orgDeleteDoc = orgGuardDoc
	r.pd = f.testClient()

	state := testOrganization(func(d *organizationModel) { d.ID = types.StringValue("org-1") })
	resp := &resource.DeleteResponse{
		State: tfsdk.State{Schema: sch, Raw: orgToRaw(ctx, t, sch, &state)},
	}
	r.Delete(ctx, resource.DeleteRequest{State: tfsdk.State{Schema: sch, Raw: orgToRaw(ctx, t, sch, &state)}}, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("Delete accepted a non-empty org, want the guard refusal")
	}
	assertError(t, &resp.Diagnostics, "still has 1 instance. Delete it first.")
}
