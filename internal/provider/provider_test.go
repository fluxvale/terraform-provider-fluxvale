package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// meDocument is the /api/v1/me shape the platform renders (id at
// data.id, email in data.attributes).
const meDocument = `{"data": {"id": "u-1", "type": "user", "attributes": {"email": "me@fluxvale.com", "platform_role": "user", "username": "me"}}}`

// meServer serves the /api/v1/me shape and records the Authorization
// header of the ping — the ground truth for token-resolution tests.
func meServer(t *testing.T, status int, body string) (*httptest.Server, *string) {
	t.Helper()
	auth := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/me" {
			auth = r.Header.Get("Authorization")
			w.WriteHeader(status)
			fmt.Fprint(w, body)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &auth
}

// str/nullStr/unknownStr build tftypes values for the config map.
func str(s string) tftypes.Value { return tftypes.NewValue(tftypes.String, s) }

var (
	nullStr    = tftypes.NewValue(tftypes.String, nil)
	unknownStr = tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
)

// configure runs the provider's Configure the way the proto6 unit-test
// pattern does: a hand-built ConfigureRequest carrying the schema and
// a Raw config value.
func configure(ctx context.Context, t *testing.T, apiURL, token, organization tftypes.Value) *provider.ConfigureResponse {
	t.Helper()

	p := New("test")()

	schemaResp := &provider.SchemaResponse{}
	p.Schema(ctx, provider.SchemaRequest{}, schemaResp)
	if schemaResp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", schemaResp.Diagnostics)
	}

	req := provider.ConfigureRequest{
		TerraformVersion: "test",
		Config: tfsdk.Config{
			Schema: schemaResp.Schema,
			Raw: tftypes.NewValue(schemaResp.Schema.Type().TerraformType(ctx), map[string]tftypes.Value{
				"api_url":      apiURL,
				"token":        token,
				"organization": organization,
			}),
		},
	}
	resp := &provider.ConfigureResponse{}
	p.Configure(ctx, req, resp)
	return resp
}

// TestConfigureMissingTokenNamesBothSources pins the diagnostic: no
// token attribute AND no FLUXVALE_PAT — the error names both sources.
func TestConfigureMissingTokenNamesBothSources(t *testing.T) {
	t.Setenv(envToken, "")

	ctx := context.Background()
	resp := configure(ctx, t, nullStr, nullStr, nullStr)

	if !resp.Diagnostics.HasError() {
		t.Fatal("Configure with no token anywhere: no error diagnostic")
	}
	found := false
	for _, d := range resp.Diagnostics.Errors() {
		if strings.Contains(d.Summary(), "token") && strings.Contains(d.Detail(), envToken) {
			found = true
		}
	}
	if !found {
		t.Fatalf("diagnostics = %+v, want an error naming both \"token\" and %s", resp.Diagnostics, envToken)
	}
}

func TestConfigureEnvVarFallback(t *testing.T) {
	t.Setenv(envToken, "fvpat_env")

	srv, auth := meServer(t, http.StatusOK, meDocument)
	resp := configure(context.Background(), t, str(srv.URL), nullStr, nullStr)

	if resp.Diagnostics.HasError() {
		t.Fatalf("Configure with FLUXVALE_PAT: %+v", resp.Diagnostics)
	}
	if *auth != "Bearer fvpat_env" {
		t.Errorf("ping Authorization = %q, want the env token", *auth)
	}
	data := resp.ResourceData.(*providerData)
	if data.userID != "u-1" || data.userEmail != "me@fluxvale.com" {
		t.Errorf("identity = %s/%s, want the /me ping's", data.userID, data.userEmail)
	}
}

func TestConfigureConfigTokenWins(t *testing.T) {
	t.Setenv(envToken, "fvpat_env")

	srv, auth := meServer(t, http.StatusOK, meDocument)
	resp := configure(context.Background(), t, str(srv.URL), str("fvpat_cfg"), str("org-1"))

	if resp.Diagnostics.HasError() {
		t.Fatalf("Configure with an explicit token: %+v", resp.Diagnostics)
	}
	if *auth != "Bearer fvpat_cfg" {
		t.Errorf("ping Authorization = %q, want the config token over the env", *auth)
	}
	data := resp.ResourceData.(*providerData)
	if data.organization.ValueString() != "org-1" {
		t.Errorf("organization = %q, want config passthrough", data.organization.ValueString())
	}
}

// TestConfigureUnknownTokenFallsBackToEnv pins the unknownable
// handling: a token unknown at plan time behaves like unset.
func TestConfigureUnknownTokenFallsBackToEnv(t *testing.T) {
	t.Setenv(envToken, "fvpat_env")

	srv, auth := meServer(t, http.StatusOK, meDocument)
	resp := configure(context.Background(), t, str(srv.URL), unknownStr, nullStr)

	if resp.Diagnostics.HasError() {
		t.Fatalf("Configure with unknown token: %+v", resp.Diagnostics)
	}
	if *auth != "Bearer fvpat_env" {
		t.Errorf("ping Authorization = %q, want the env fallback for the unknown value", *auth)
	}
}

func TestConfigureBadTokenFailsWithServerWording(t *testing.T) {
	doc := `{"errors": [{"title": "Unauthorized", "detail": "invalid or expired token"}]}`
	srv, _ := meServer(t, http.StatusUnauthorized, doc)
	resp := configure(context.Background(), t, str(srv.URL), str("fvpat_bad"), nullStr)

	if !resp.Diagnostics.HasError() {
		t.Fatal("Configure with a dead token: no error diagnostic")
	}
	found := false
	for _, d := range resp.Diagnostics.Errors() {
		if strings.Contains(d.Detail(), "invalid or expired token") {
			found = true
		}
	}
	if !found {
		t.Fatalf("diagnostics = %+v, want the server's 401 wording", resp.Diagnostics)
	}
}

// TestSchemaContract pins the STABLE half of the compatibility
// contract (docs/compatibility.md): the config attribute names, the
// sensitive token, the documented default.
func TestSchemaContract(t *testing.T) {
	ctx := context.Background()

	p := New("test")()
	schemaResp := &provider.SchemaResponse{}
	p.Schema(ctx, provider.SchemaRequest{}, schemaResp)

	want := []string{"api_url", "organization", "token"}
	attrs := schemaResp.Schema.Attributes
	for _, name := range want {
		if _, ok := attrs[name]; !ok {
			t.Errorf("schema missing STABLE attribute %q", name)
		}
	}
	if len(attrs) != len(want) {
		t.Errorf("schema attributes = %+v, want exactly %v", attrs, want)
	}
	if !attrs["token"].IsSensitive() {
		t.Error("token attribute is not sensitive")
	}
	if defaultAPIURL != "https://fluxvale.com" {
		t.Errorf("defaultAPIURL = %q, want the documented default", defaultAPIURL)
	}
}

// An api_url unknown at plan time must error, not silently resolve to
// the production default — the credential would hit the wrong
// environment.
func TestConfigureUnknownAPIURLErrors(t *testing.T) {
	srv, _ := meServer(t, http.StatusOK, meDocument)

	resp := configure(context.Background(), t, unknownStr, str("fvpat_test"), nullStr)

	if !resp.Diagnostics.HasError() {
		t.Fatalf("unknown api_url: no error, want the not-known diagnostic (server %s never hit)", srv.URL)
	}
	summary := resp.Diagnostics.Errors()[0].Summary()
	if !strings.Contains(summary, "API URL is not known yet") {
		t.Errorf("summary = %q, want the not-known wording", summary)
	}
}

func TestConfigureTrimsConfigToken(t *testing.T) {
	srv, auth := meServer(t, http.StatusOK, meDocument)

	resp := configure(context.Background(), t, str(srv.URL), str(" fvpat_x "), nullStr)

	if resp.Diagnostics.HasError() {
		t.Fatalf("Configure with a padded token: %+v", resp.Diagnostics)
	}
	if *auth != "Bearer fvpat_x" {
		t.Errorf("ping Authorization = %q, want the trimmed token", *auth)
	}
}
