package provider

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// secretEnvPairDiagnostics enforces the version contract at plan time
// (and again at apply time for direct callers): the map and its
// rotation integer travel together, and the version is positive.
// Diagnostics name keys, never values.
func secretEnvPairDiagnostics(secret types.Map, version types.Int64) diag.Diagnostics {
	var diags diag.Diagnostics

	if secret.IsUnknown() || version.IsUnknown() {
		// Unknown at plan time — the apply-time checks decide.
		return diags
	}

	switch {
	case secret.IsNull() && version.IsNull():
		return diags
	case secret.IsNull():
		diags.AddAttributeError(
			path.Root("secret_env_vars_version"),
			"secret_env_vars_version requires secret_env_vars",
			"Set secret_env_vars alongside the version, or remove both attributes. A version alone has nothing to rotate.",
		)
		return diags
	case version.IsNull():
		diags.AddAttributeError(
			path.Root("secret_env_vars_version"),
			"secret_env_vars_version is required",
			"secret_env_vars is set without its rotation version. Set secret_env_vars_version to a positive integer, "+
				"and bump it whenever a secret value changes.",
		)
		return diags
	case version.ValueInt64() <= 0:
		diags.AddAttributeError(
			path.Root("secret_env_vars_version"),
			"secret_env_vars_version must be greater than 0",
			fmt.Sprintf("Got %d. Start at 1 and increment it for every secret change.", version.ValueInt64()),
		)
		return diags
	}

	for key, value := range secret.Elements() {
		s, ok := value.(types.String)
		if !ok {
			diags.AddAttributeError(
				path.Root("secret_env_vars"),
				"Invalid secret_env_vars element",
				fmt.Sprintf("secret_env_vars[%q] has an unexpected type — values must be strings", key),
			)
			continue
		}
		if s.IsNull() {
			diags.AddAttributeError(
				path.Root("secret_env_vars"),
				"Invalid secret_env_vars",
				fmt.Sprintf("secret_env_vars[%q] is null — remove the key instead; an empty string is a value", key),
			)
		}
		// Unknown elements are resolved before apply; ValidateConfig
		// sees them often and must not reject a valid interpolation.
	}

	return diags
}

// ValidateConfig is the plan-time gate for the secret pair and the
// cross-map key rule. Terraform calls it before any diff, so a
// misconfiguration fails with a key name and never a value.
func (r *instanceResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var data instanceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(secretEnvPairDiagnostics(data.SecretEnvVars, data.SecretEnvVarsVersion)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if data.EnvVars.IsNull() || data.EnvVars.IsUnknown() ||
		data.SecretEnvVars.IsNull() || data.SecretEnvVars.IsUnknown() {
		return
	}

	overlap := make([]string, 0)
	for key := range data.SecretEnvVars.Elements() {
		if _, ok := data.EnvVars.Elements()[key]; ok {
			overlap = append(overlap, key)
		}
	}
	if len(overlap) == 0 {
		return
	}
	sort.Strings(overlap)
	resp.Diagnostics.AddAttributeError(
		path.Root("secret_env_vars"),
		"Key present in both env_vars and secret_env_vars",
		fmt.Sprintf("keys present in both maps: %s — put each key in exactly one map (a secret key belongs only in secret_env_vars).",
			joinKeys(overlap)),
	)
}

// secretEnvMap converts the write-only map into wire values. It is
// only called at apply time (Create/Update), when the configuration
// value is known.
func secretEnvMap(ctx context.Context, v types.Map) (map[string]string, diag.Diagnostics) {
	var diags diag.Diagnostics
	if v.IsNull() {
		return map[string]string{}, diags
	}
	if v.IsUnknown() {
		diags.AddError(
			"Invalid secret_env_vars",
			"secret_env_vars is unknown at apply time — resolve the value before applying.",
		)
		return nil, diags
	}
	var elems map[string]types.String
	diags.Append(v.ElementsAs(ctx, &elems, false)...)
	if diags.HasError() {
		return nil, diags
	}
	out := make(map[string]string, len(elems))
	for key, val := range elems {
		if val.IsNull() {
			diags.AddError(
				"Invalid secret_env_vars",
				fmt.Sprintf("secret_env_vars[%q] is null — remove the key instead; an empty string is a value", key),
			)
			continue
		}
		if val.IsUnknown() {
			diags.AddError(
				"Invalid secret_env_vars",
				fmt.Sprintf("secret_env_vars[%q] is unknown at apply time — resolve the value before applying", key),
			)
			continue
		}
		out[key] = val.ValueString()
	}
	if diags.HasError() {
		return nil, diags
	}
	return out, diags
}

// mergeEnvMaps folds the secret values into the regular env map for
// the create POST. Overlap is a plan-time error; this is the
// apply-time backstop (and names keys only).
func mergeEnvMaps(env, secret map[string]string) (map[string]string, diag.Diagnostics) {
	var diags diag.Diagnostics
	if overlap := mapOverlap(env, secret); len(overlap) > 0 {
		diags.AddError(
			"Invalid environment variables",
			fmt.Sprintf("keys present in both env_vars and secret_env_vars: %s — put each key in exactly one map", joinKeys(overlap)),
		)
		return nil, diags
	}
	out := make(map[string]string, len(env)+len(secret))
	for key, value := range env {
		out[key] = value
	}
	for key, value := range secret {
		out[key] = value
	}
	return out, diags
}

// mapOverlap returns the sorted intersection of two key sets.
func mapOverlap(left, right map[string]string) []string {
	overlap := make([]string, 0)
	for key := range right {
		if _, ok := left[key]; ok {
			overlap = append(overlap, key)
		}
	}
	sort.Strings(overlap)
	return overlap
}

func joinKeys(keys []string) string {
	quoted := make([]string, len(keys))
	for i, key := range keys {
		quoted[i] = fmt.Sprintf("%q", key)
	}
	return strings.Join(quoted, ", ")
}

// sortedKeys returns the map's keys in sorted order — the public key
// list and the remove diff both rely on deterministic order.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// secretKeysList renders the managed secret key names as the computed
// list. Values never enter it.
func secretKeysList(keys []string) types.List {
	sorted := append([]string{}, keys...)
	sort.Strings(sorted)
	values := make([]attr.Value, len(sorted))
	for i, key := range sorted {
		values[i] = types.StringValue(key)
	}
	return types.ListValueMust(types.StringType, values)
}

// secretKeysFromList reads the prior state's managed key names. A
// null/unknown list is the import case: no keys to remove yet.
func secretKeysFromList(ctx context.Context, v types.List, diags *diag.Diagnostics) []string {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}
	var keys []string
	diags.Append(v.ElementsAs(ctx, &keys, false)...)
	if diags.HasError() {
		return nil
	}
	sort.Strings(keys)
	return keys
}
