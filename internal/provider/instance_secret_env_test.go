package provider

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// --- schema ----------------------------------------------------------

func TestSecretEnvSchemaFlags(t *testing.T) {
	ctx := context.Background()
	_, sch := newSchemadResource(ctx, t)

	secret, ok := sch.Attributes["secret_env_vars"].(schema.MapAttribute)
	if !ok {
		t.Fatalf("secret_env_vars is %T, want schema.MapAttribute", sch.Attributes["secret_env_vars"])
	}
	if !secret.Optional || secret.Required || secret.Computed {
		t.Errorf("secret_env_vars Optional/Required/Computed = %v/%v/%v, want optional input only",
			secret.Optional, secret.Required, secret.Computed)
	}
	if !secret.Sensitive {
		t.Error("secret_env_vars is not sensitive")
	}
	if !secret.WriteOnly {
		t.Error("secret_env_vars is not write-only — values would land in state")
	}
	if secret.ElementType != types.StringType {
		t.Errorf("secret_env_vars element type = %v, want string", secret.ElementType)
	}

	version, ok := sch.Attributes["secret_env_vars_version"].(schema.Int64Attribute)
	if !ok {
		t.Fatalf("secret_env_vars_version is %T, want schema.Int64Attribute", sch.Attributes["secret_env_vars_version"])
	}
	if !version.Optional || version.Required || version.Computed {
		t.Errorf("secret_env_vars_version Optional/Required/Computed = %v/%v/%v, want an optional integer",
			version.Optional, version.Required, version.Computed)
	}
	if version.Sensitive || version.WriteOnly {
		t.Error("secret_env_vars_version must be visible — it is the rotation signal")
	}

	keys, ok := sch.Attributes["secret_env_vars_keys"].(schema.ListAttribute)
	if !ok {
		t.Fatalf("secret_env_vars_keys is %T, want schema.ListAttribute", sch.Attributes["secret_env_vars_keys"])
	}
	if !keys.Computed || keys.Optional || keys.Required {
		t.Errorf("secret_env_vars_keys Computed/Optional/Required = %v/%v/%v, want computed-only",
			keys.Computed, keys.Optional, keys.Required)
	}
	if keys.Sensitive {
		t.Error("secret_env_vars_keys is sensitive — it carries key names only and should be reviewable")
	}
}

// --- config validation ----------------------------------------------

func configFor(t *testing.T, ctx context.Context, sch schema.Schema, data *instanceModel) resource.ValidateConfigRequest {
	t.Helper()
	return resource.ValidateConfigRequest{
		Config: tfsdk.Config{Schema: sch, Raw: modelToRaw(ctx, t, sch, data)},
	}
}

func validateInstanceConfig(t *testing.T, data *instanceModel) resource.ValidateConfigResponse {
	t.Helper()
	ctx := context.Background()
	r, sch := newSchemadResource(ctx, t)
	resp := resource.ValidateConfigResponse{}
	r.ValidateConfig(ctx, configFor(t, ctx, sch, data), &resp)
	return resp
}

func secretMap(t *testing.T, m map[string]string) types.Map {
	t.Helper()
	return envVarInput(t, m)
}

func TestSecretEnvConfigRequiresVersion(t *testing.T) {
	data := testInstance(func(d *instanceModel) {
		d.SecretEnvVars = secretMap(t, map[string]string{"API_KEY": "wo-value"})
	})
	resp := validateInstanceConfig(t, &data)
	assertError(t, &resp.Diagnostics, "secret_env_vars_version is required")
}

func TestSecretEnvConfigRejectsVersionWithoutMap(t *testing.T) {
	data := testInstance(func(d *instanceModel) {
		d.SecretEnvVarsVersion = types.Int64Value(2)
	})
	resp := validateInstanceConfig(t, &data)
	assertError(t, &resp.Diagnostics, "secret_env_vars_version requires secret_env_vars")
}

func TestSecretEnvConfigRejectsNonPositiveVersion(t *testing.T) {
	for _, version := range []int64{0, -1} {
		data := testInstance(func(d *instanceModel) {
			d.SecretEnvVars = secretMap(t, map[string]string{"API_KEY": "wo-value"})
			d.SecretEnvVarsVersion = types.Int64Value(version)
		})
		resp := validateInstanceConfig(t, &data)
		assertError(t, &resp.Diagnostics, "must be greater than 0")
	}
}

func TestSecretEnvConfigRejectsKeyInBothMaps(t *testing.T) {
	data := testInstance(func(d *instanceModel) {
		d.EnvVars = secretMap(t, map[string]string{"API_KEY": "plain", "PLAIN": "x"})
		d.SecretEnvVars = secretMap(t, map[string]string{"API_KEY": "wo-value", "OTHER": "y"})
		d.SecretEnvVarsVersion = types.Int64Value(1)
	})
	resp := validateInstanceConfig(t, &data)
	assertError(t, &resp.Diagnostics, "present in both", `"API_KEY"`)
	// Never echo a value in a diagnostic.
	for _, d := range resp.Diagnostics.Errors() {
		for _, value := range []string{"plain", "wo-value", `"x"`, `"y"`} {
			if strings.Contains(d.Detail(), value) {
				t.Fatalf("diagnostic leaked a value: %q", d.Detail())
			}
		}
	}
}

func TestSecretEnvConfigAcceptsPairedMap(t *testing.T) {
	data := testInstance(func(d *instanceModel) {
		d.EnvVars = secretMap(t, map[string]string{"PLAIN": "x"})
		d.SecretEnvVars = secretMap(t, map[string]string{"API_KEY": "wo-value"})
		d.SecretEnvVarsVersion = types.Int64Value(1)
	})
	resp := validateInstanceConfig(t, &data)
	if resp.Diagnostics.HasError() {
		t.Fatalf("valid secret config: %v", resp.Diagnostics)
	}
}

func TestSecretEnvConfigRejectsNullElement(t *testing.T) {
	data := testInstance(func(d *instanceModel) {
		d.SecretEnvVars = types.MapValueMust(types.StringType, map[string]attr.Value{
			"API_KEY": types.StringNull(),
		})
		d.SecretEnvVarsVersion = types.Int64Value(1)
	})
	resp := validateInstanceConfig(t, &data)
	assertError(t, &resp.Diagnostics, `secret_env_vars["API_KEY"] is null`)
}

// --- create ----------------------------------------------------------

func TestCreateMergesSecretEnvIntoOnePost(t *testing.T) {
	f := newFakeAPI(t)
	f.orgsDoc = `{"data": [{"id": "org-1", "type": "organization", "attributes": {"slug": "acme", "name": "Acme"}}]}`
	f.postResponse = createResponse
	f.instanceGets = []scriptedResponse{script(200, runningDoc)}

	pd := f.testClient()
	data := testInstance(func(d *instanceModel) {
		d.EnvVars = secretMap(t, map[string]string{"PLAIN": "visible"})
		d.SecretEnvVars = secretMap(t, map[string]string{"ZED": "second", "API_KEY": "first"})
		d.SecretEnvVarsVersion = types.Int64Value(1)
	})
	diags := new(diag.Diagnostics)
	createInstance(context.Background(), pd, &data, diags)
	assertNoErrors(t, diags)

	attrs := attrsOf(t, findOne(t, f.all(), http.MethodPost, "/api/v1/instances"))
	env, ok := attrs["env_vars"].(map[string]any)
	if !ok {
		t.Fatalf("env_vars = %#v, want a map", attrs["env_vars"])
	}
	want := map[string]any{"PLAIN": "visible", "API_KEY": "first", "ZED": "second"}
	if !reflect.DeepEqual(env, want) {
		t.Errorf("POST env_vars keys = %v, want the merged map", keysOfAny(env))
	}

	keys := listOf(t, data.SecretEnvVarsKeys)
	if !reflect.DeepEqual(keys, []string{"API_KEY", "ZED"}) {
		t.Errorf("secret_env_vars_keys = %v, want sorted [API_KEY ZED]", keys)
	}
}

func TestCreateRejectsSecretEnvOverlapBeforePost(t *testing.T) {
	f := newFakeAPI(t)
	f.orgsDoc = `{"data": [{"id": "org-1", "type": "organization", "attributes": {"slug": "acme", "name": "Acme"}}]}`
	pd := f.testClient()
	data := testInstance(func(d *instanceModel) {
		d.EnvVars = secretMap(t, map[string]string{"API_KEY": "plain"})
		d.SecretEnvVars = secretMap(t, map[string]string{"API_KEY": "wo-value"})
		d.SecretEnvVarsVersion = types.Int64Value(1)
	})
	diags := new(diag.Diagnostics)
	createInstance(context.Background(), pd, &data, diags)
	assertError(t, diags, "keys present in both", `"API_KEY"`)
	for _, r := range f.all() {
		if r.method == http.MethodPost {
			t.Errorf("unexpected POST %s after an overlap", r.path)
		}
	}
}

// --- update ----------------------------------------------------------

func secretEnvBase(t *testing.T, secret map[string]string, version int64, keys []string) instanceModel {
	t.Helper()
	data := envBase(t, map[string]string{"PLAIN": "same"})
	data.SecretEnvVars = secretMap(t, secret)
	data.SecretEnvVarsVersion = types.Int64Value(version)
	data.SecretEnvVarsKeys = secretKeysList(keys)
	return data
}

func TestUpdateSecretRotationResendsEverySecretKey(t *testing.T) {
	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{script(200, runningDoc)}

	state := secretEnvBase(t, map[string]string{"API_KEY": "old", "OTHER": "keep"}, 1, []string{"API_KEY", "OTHER"})
	plan := state
	plan.SecretEnvVars = secretMap(t, map[string]string{"API_KEY": "new", "OTHER": "keep"})
	plan.SecretEnvVarsVersion = types.Int64Value(2)

	diags := new(diag.Diagnostics)
	updateInstance(context.Background(), f.testClient(), &plan, &state, diags)
	assertNoErrors(t, diags)

	reqs := f.all()
	assertRequestList(t, reqs,
		"PATCH /api/v1/instances/inst-1/env",
		"GET /api/v1/instances/inst-1",
	)
	// Both keys re-sent: the provider cannot diff write-only values, so
	// the version bump carries the whole set.
	assertEnvMerge(t, reqs[0],
		map[string]string{"API_KEY": "new", "OTHER": "keep"},
		nil,
	)
	if keys := listOf(t, plan.SecretEnvVarsKeys); !reflect.DeepEqual(keys, []string{"API_KEY", "OTHER"}) {
		t.Errorf("secret_env_vars_keys = %v, want both keys", keys)
	}
}

func TestUpdateUnchangedVersionDoesNotResendSecrets(t *testing.T) {
	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{script(200, runningDoc)}

	state := secretEnvBase(t, map[string]string{"API_KEY": "old"}, 1, []string{"API_KEY"})
	plan := state
	// Another attribute change (not a rotation) still has the write-only
	// value in config, but the version is unchanged: never resend.
	plan.EnvVars = secretMap(t, map[string]string{"PLAIN": "new"})

	diags := new(diag.Diagnostics)
	updateInstance(context.Background(), f.testClient(), &plan, &state, diags)
	assertNoErrors(t, diags)

	reqs := f.all()
	assertRequestList(t, reqs,
		"PATCH /api/v1/instances/inst-1/env",
		"GET /api/v1/instances/inst-1",
	)
	assertEnvMerge(t, reqs[0], map[string]string{"PLAIN": "new"}, nil)
}

func TestUpdateRemovingSecretKeyNeedsOnlyTheBump(t *testing.T) {
	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{script(200, runningDoc)}

	state := secretEnvBase(t,
		map[string]string{"KEEP": "one", "DROP": "two"}, 4, []string{"DROP", "KEEP"})
	plan := state
	plan.SecretEnvVars = secretMap(t, map[string]string{"KEEP": "one"})
	plan.SecretEnvVarsVersion = types.Int64Value(5)

	diags := new(diag.Diagnostics)
	updateInstance(context.Background(), f.testClient(), &plan, &state, diags)
	assertNoErrors(t, diags)

	reqs := f.all()
	assertRequestList(t, reqs,
		"PATCH /api/v1/instances/inst-1/env",
		"GET /api/v1/instances/inst-1",
	)
	assertEnvMerge(t, reqs[0], map[string]string{"KEEP": "one"}, []string{"DROP"})
	if keys := listOf(t, plan.SecretEnvVarsKeys); !reflect.DeepEqual(keys, []string{"KEEP"}) {
		t.Errorf("secret_env_vars_keys = %v, want [KEEP]", keys)
	}
}

func TestUpdateRemovingEverySecretKeyDropsBothAttributes(t *testing.T) {
	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{script(200, runningDoc)}

	state := secretEnvBase(t, map[string]string{"A": "1", "B": "2"}, 7, []string{"A", "B"})
	plan := state
	plan.SecretEnvVars = types.MapNull(types.StringType)
	plan.SecretEnvVarsVersion = types.Int64Null()

	diags := new(diag.Diagnostics)
	updateInstance(context.Background(), f.testClient(), &plan, &state, diags)
	assertNoErrors(t, diags)

	reqs := f.all()
	assertRequestList(t, reqs,
		"PATCH /api/v1/instances/inst-1/env",
		"GET /api/v1/instances/inst-1",
	)
	assertEnvMerge(t, reqs[0], map[string]string{}, []string{"A", "B"})
	if keys := listOf(t, plan.SecretEnvVarsKeys); len(keys) != 0 {
		t.Errorf("secret_env_vars_keys = %v, want empty", keys)
	}
}

func TestUpdateSecretUnknownWarnsAndKeepsState(t *testing.T) {
	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{script(200, runningDoc)}

	state := secretEnvBase(t, map[string]string{"API_KEY": "old"}, 3, []string{"API_KEY"})
	plan := state
	plan.SecretEnvVars = types.MapUnknown(types.StringType)
	plan.SecretEnvVarsVersion = types.Int64Value(4)

	diags := new(diag.Diagnostics)
	updateInstance(context.Background(), f.testClient(), &plan, &state, diags)
	assertNoErrors(t, diags)
	if len(diags.Warnings()) == 0 {
		t.Fatal("unknown secret_env_vars: no warning")
	}
	assertRequestList(t, f.all(), "GET /api/v1/instances/inst-1")
	if !plan.SecretEnvVarsVersion.Equal(state.SecretEnvVarsVersion) {
		t.Errorf("version after unknown skip = %v, want the prior %v", plan.SecretEnvVarsVersion, state.SecretEnvVarsVersion)
	}
	if keys := listOf(t, plan.SecretEnvVarsKeys); !reflect.DeepEqual(keys, []string{"API_KEY"}) {
		t.Errorf("keys after unknown skip = %v, want the prior list", keys)
	}
}

// A secret unknown at apply time skips the whole merge: an env_vars
// change in the same plan must stay in state as the prior value, not
// as a change the server never saw.
func TestUpdateSecretUnknownKeepsEnvInState(t *testing.T) {
	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{script(200, runningDoc)}

	state := secretEnvBase(t, map[string]string{"API_KEY": "old"}, 3, []string{"API_KEY"})
	plan := state
	plan.EnvVars = secretMap(t, map[string]string{"PLAIN": "new"})
	plan.SecretEnvVars = types.MapUnknown(types.StringType)
	plan.SecretEnvVarsVersion = types.Int64Value(4)

	diags := new(diag.Diagnostics)
	updateInstance(context.Background(), f.testClient(), &plan, &state, diags)
	assertNoErrors(t, diags)
	assertRequestList(t, f.all(), "GET /api/v1/instances/inst-1")
	if got := envMapOf(t, plan.EnvVars); !reflect.DeepEqual(got, map[string]string{"PLAIN": "same"}) {
		t.Errorf("env_vars after a secret skip = %v, want the prior state", got)
	}
	if !plan.SecretEnvVarsVersion.Equal(state.SecretEnvVarsVersion) {
		t.Errorf("version after a secret skip = %v, want the prior %v", plan.SecretEnvVarsVersion, state.SecretEnvVarsVersion)
	}
}

// The wrapper must never write the write-only map to state, while the
// computed key list and version do ride state.
func TestUpdateWrapperKeepsSecretsOutOfState(t *testing.T) {
	ctx := context.Background()
	r, sch := newSchemadResource(ctx, t)

	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{script(200, runningDoc)}
	r.pd = f.testClient()

	stateData := secretEnvBase(t, map[string]string{"API_KEY": "old"}, 1, []string{"API_KEY"})
	planData := stateData
	planData.SecretEnvVars = secretMap(t, map[string]string{"API_KEY": "new"})
	planData.SecretEnvVarsVersion = types.Int64Value(2)

	req := resource.UpdateRequest{
		Plan:   tfsdk.Plan{Schema: sch, Raw: modelToRaw(ctx, t, sch, &planData)},
		State:  tfsdk.State{Schema: sch, Raw: modelToRaw(ctx, t, sch, &stateData)},
		Config: tfsdk.Config{Schema: sch, Raw: modelToRaw(ctx, t, sch, &planData)},
	}
	resp := &resource.UpdateResponse{
		State: tfsdk.State{Schema: sch, Raw: modelToRaw(ctx, t, sch, &stateData)},
	}
	r.Update(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", resp.Diagnostics)
	}

	got := rawToModel(ctx, t, sch, resp.State.Raw)
	if !got.SecretEnvVars.IsNull() {
		t.Errorf("state secret_env_vars = %v, want null (write-only never persists)", got.SecretEnvVars)
	}
	if got.SecretEnvVarsVersion.ValueInt64() != 2 {
		t.Errorf("state version = %v, want 2", got.SecretEnvVarsVersion)
	}
	if keys := listOf(t, got.SecretEnvVarsKeys); !reflect.DeepEqual(keys, []string{"API_KEY"}) {
		t.Errorf("state keys = %v, want [API_KEY]", keys)
	}
}

// Moving a key from secret_env_vars to env_vars without a bump is
// refused: the write-only map cannot show the move, and silently
// leaving the old secret key tracked would later remove it from the
// app even though the config still sets it as a plain env var.
func TestUpdateSecretToEnvMoveWithoutBumpIsRefused(t *testing.T) {
	f := newFakeAPI(t)

	state := secretEnvBase(t, map[string]string{"MOVE": "old"}, 1, []string{"MOVE"})
	plan := state
	plan.EnvVars = secretMap(t, map[string]string{"PLAIN": "same", "MOVE": "new"})
	plan.SecretEnvVars = secretMap(t, map[string]string{})
	plan.SecretEnvVarsVersion = types.Int64Value(1)

	diags := new(diag.Diagnostics)
	if updateInstance(context.Background(), f.testClient(), &plan, &state, diags) {
		t.Fatal("updateInstance reported the instance gone")
	}
	assertError(t, diags, "keys changed without a version bump", `"MOVE"`)
	for _, r := range f.all() {
		if r.method == http.MethodPatch {
			t.Errorf("unexpected PATCH %s after a key-set change without a bump", r.path)
		}
	}
}

// The same move WITH a bump is one set, never a remove — the running
// app keeps the key, now as a plain env var.
func TestUpdateSecretToEnvMoveWithBumpSetsOnly(t *testing.T) {
	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{script(200, runningDoc)}

	state := secretEnvBase(t, map[string]string{"MOVE": "old"}, 1, []string{"MOVE"})
	plan := state
	plan.EnvVars = secretMap(t, map[string]string{"PLAIN": "same", "MOVE": "new"})
	plan.SecretEnvVars = secretMap(t, map[string]string{})
	plan.SecretEnvVarsVersion = types.Int64Value(2)

	diags := new(diag.Diagnostics)
	updateInstance(context.Background(), f.testClient(), &plan, &state, diags)
	assertNoErrors(t, diags)

	reqs := f.all()
	assertRequestList(t, reqs,
		"PATCH /api/v1/instances/inst-1/env",
		"GET /api/v1/instances/inst-1",
	)
	assertEnvMerge(t, reqs[0], map[string]string{"MOVE": "new"}, nil)
	if keys := listOf(t, plan.SecretEnvVarsKeys); len(keys) != 0 {
		t.Errorf("secret_env_vars_keys = %v, want empty after the move", keys)
	}
}

// Moving env_vars -> secret_env_vars WITH a bump is one set too.
func TestUpdateEnvToSecretMoveWithBumpSetsOnly(t *testing.T) {
	f := newFakeAPI(t)
	f.instanceGets = []scriptedResponse{script(200, runningDoc)}

	state := envBase(t, map[string]string{"PLAIN": "same", "MOVE": "old"})
	state.SecretEnvVarsVersion = types.Int64Null()
	state.SecretEnvVarsKeys = types.ListNull(types.StringType)
	plan := state
	plan.EnvVars = secretMap(t, map[string]string{"PLAIN": "same"})
	plan.SecretEnvVars = secretMap(t, map[string]string{"MOVE": "new"})
	plan.SecretEnvVarsVersion = types.Int64Value(1)

	diags := new(diag.Diagnostics)
	updateInstance(context.Background(), f.testClient(), &plan, &state, diags)
	assertNoErrors(t, diags)

	reqs := f.all()
	assertRequestList(t, reqs,
		"PATCH /api/v1/instances/inst-1/env",
		"GET /api/v1/instances/inst-1",
	)
	assertEnvMerge(t, reqs[0], map[string]string{"MOVE": "new"}, nil)
	if keys := listOf(t, plan.SecretEnvVarsKeys); !reflect.DeepEqual(keys, []string{"MOVE"}) {
		t.Errorf("secret_env_vars_keys = %v, want [MOVE]", keys)
	}
}

func TestUpdateNewSecretKeyWithoutBumpIsRefused(t *testing.T) {
	f := newFakeAPI(t)

	state := secretEnvBase(t, map[string]string{"KEEP": "one"}, 1, []string{"KEEP"})
	plan := state
	plan.SecretEnvVars = secretMap(t, map[string]string{"KEEP": "one", "NEW": "two"})
	plan.SecretEnvVarsVersion = types.Int64Value(1)

	diags := new(diag.Diagnostics)
	if updateInstance(context.Background(), f.testClient(), &plan, &state, diags) {
		t.Fatal("updateInstance reported the instance gone")
	}
	assertError(t, diags, "keys changed without a version bump", `"NEW"`)
}

func TestUpdateRemovedSecretKeyWithoutBumpIsRefused(t *testing.T) {
	f := newFakeAPI(t)

	state := secretEnvBase(t, map[string]string{"KEEP": "one", "DROP": "two"}, 1, []string{"DROP", "KEEP"})
	plan := state
	plan.SecretEnvVars = secretMap(t, map[string]string{"KEEP": "one"})
	plan.SecretEnvVarsVersion = types.Int64Value(1)

	diags := new(diag.Diagnostics)
	if updateInstance(context.Background(), f.testClient(), &plan, &state, diags) {
		t.Fatal("updateInstance reported the instance gone")
	}
	assertError(t, diags, "keys changed without a version bump", `"DROP"`)
}

// --- helpers ---------------------------------------------------------

func listOf(t *testing.T, l types.List) []string {
	t.Helper()
	if l.IsNull() || l.IsUnknown() {
		return nil
	}
	var out []string
	if diags := l.ElementsAs(context.Background(), &out, false); diags.HasError() {
		t.Fatalf("ElementsAs: %v", diags)
	}
	return out
}

func keysOfAny(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}
