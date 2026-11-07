package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fluxvale/fluxvale/apps/tfprovider/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	frameworkresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/config"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// Acceptance tests: REAL resources against a REAL FluxVale org
// (#482's acceptance lane). NEVER per-PR — every step creates real,
// billing platform rows, so this runs nightly, on tf-v* tags, and on
// demand (.github/workflows/tfprovider-acceptance.yml); the secret it
// needs minting is listed in that workflow's comments.
//
// Environment:
//
//	TF_ACC                    set (=1) — the framework's own gate; unset skips
//	TF_ACC_API_URL            required — staging (https://staging.fluxvale.com)
//	TF_ACC_PAT                required — fails loud when unset (smoke.yml's pattern)
//	TF_ACC_APP_SLUG           optional catalog app, default "forgejo"
//	TF_ACC_BACKUP_ENDPOINT/BUCKET/ACCESS_KEY_ID/SECRET_ACCESS_KEY
//	                          optional R2 target — the backups test skips without
//	                          the full set (the org must have paid once for backups)
//	TF_ACC_TAILNET_DOMAIN + TF_ACC_TAILSCALE_AUTH_KEY
//	                          optional — the gateway test skips without the pair
//	                          (the gateway persists after destroy — no API delete
//	                          route — and bills monthly)
//	TF_ACC_DOMAIN             optional — the custom-domain test skips without it
//
// The driver: plugin-testing needs a real terraform-shaped CLI in
// PATH or TF_ACC_TERRAFORM_PATH — the workflow points the latter at
// the mise-pinned tofu, sets TF_ACC_TERRAFORM_VERSION, and pins
// TF_ACC_PROVIDER_NAMESPACE/HOST to tofu's registry address (the
// documented pair for driving OpenTofu). The real-tofu smoke gate
// (e2e_test.go) is the cheap sibling: fake API, every PR.
const (
	accVarDecls = `
variable "api_url" {
  type = string
}

variable "token" {
  type      = string
  sensitive = true
}

provider "fluxvale" {
  api_url = var.api_url
  token   = var.token
}
`
)

// accProtoV6 serves the REAL provider (this package) to plugin-testing
// over gRPC — the same RPC path tofu drives.
func accProtoV6() map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		// New returns a factory (main.go's serve shape) — call it once
		// for the provider the protocol server wraps.
		"fluxvale": providerserver.NewProtocol6WithError(New("acc")()),
	}
}

// accVars feeds api_url/token as variables, not interpolated literals:
// the PAT stays out of the config string plugin-testing may print on
// failure.
func accVars(t *testing.T) config.Variables {
	t.Helper()
	return config.Variables{
		"api_url": config.StringVariable(os.Getenv("TF_ACC_API_URL")),
		"token":   config.StringVariable(os.Getenv("TF_ACC_PAT")),
	}
}

// accPreCheck fails loud, smoke-style, when the credential half is
// missing — a nightly that proves nothing must RED, not green-skip.
func accPreCheck(t *testing.T) {
	t.Helper()
	if os.Getenv("TF_ACC_API_URL") == "" || os.Getenv("TF_ACC_PAT") == "" {
		t.Fatalf("TF_ACC_API_URL and TF_ACC_PAT must be set — mint a PAT per docs/deployment.md " +
			"(scopes instances:write backups:write gateways:write + catalog:read; the org must have paid once for backups)")
	}
}

// accClient is the check-side client (destroy assertions).
func accClient(t *testing.T) *client.Client {
	t.Helper()
	accPreCheck(t)
	return client.New(os.Getenv("TF_ACC_API_URL"), os.Getenv("TF_ACC_PAT"), "acc-check", nil)
}

// accSkip skips the test when its optional credential class is absent,
// with the mint pointer — the PAT halves fail loud instead (see
// accPreCheck): the nightly must not silently narrow to nothing.
func accSkip(t *testing.T, test string, envs ...string) {
	t.Helper()
	for _, e := range envs {
		if os.Getenv(e) == "" {
			t.Skipf("%s skipped — %s is not set (the PA mints it per docs/deployment.md)", test, e)
		}
	}
}

func accAppSlug() string {
	if s := os.Getenv("TF_ACC_APP_SLUG"); s != "" {
		return s
	}
	return "forgejo"
}

// accHasResource asks the real provider Metadata whether a type is
// registered — the same RPC tofu drives. A test whose resource lands
// on a later slice skips on its absence instead of failing against a
// provider that can't serve it (TestAccCustomDomain waits for #548).
func accHasResource(typeName string) bool {
	p := New("acc")()
	req := frameworkresource.MetadataRequest{ProviderTypeName: "fluxvale"}
	for _, factory := range p.Resources(context.Background()) {
		resp := &frameworkresource.MetadataResponse{}
		factory().Metadata(context.Background(), req, resp)
		if resp.TypeName == typeName {
			return true
		}
	}
	return false
}

// accInstancesGone is CheckDestroy for instance-backed tests: every
// instance in state must 404 (destroy already waited the teardown).
func accInstancesGone(t *testing.T) func(*terraform.State) error {
	t.Helper()
	return func(s *terraform.State) error {
		c := accClient(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		for addr, rs := range s.RootModule().Resources {
			// The dot pins the bare instance resource — backups addresses
			// are fluxvale_instance_backups.* and have no instance id.
			if !strings.HasPrefix(addr, "fluxvale_instance.") {
				continue
			}
			_, err := c.GetInstance(ctx, rs.Primary.ID)
			var apiErr *client.APIError
			if err == nil {
				return fmt.Errorf("instance %s (%s) still exists after destroy", rs.Primary.ID, addr)
			}
			if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
				return fmt.Errorf("checking instance %s: %w", rs.Primary.ID, err)
			}
		}
		return nil
	}
}

// TestAccAppsDataSource reads the real catalog — the cheapest real-org
// proof that configure + a data source round-trip.
func TestAccAppsDataSource(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC not set — the acceptance lane runs nightly/tags/dispatch, never per-PR (wallet cost)")
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: accProtoV6(),
		PreCheck:                 func() { accPreCheck(t) },
		Steps: []resource.TestStep{{
			Config: accVarDecls + `
data "fluxvale_apps" "catalog" {}
`,
			ConfigVariables: accVars(t),
			Check: resource.ComposeTestCheckFunc(
				func(s *terraform.State) error {
					rs, ok := s.RootModule().Resources["data.fluxvale_apps.catalog"]
					if !ok {
						return fmt.Errorf("data.fluxvale_apps.catalog is not in state")
					}
					if n := rs.Primary.Attributes["apps.#"]; n == "" || n == "0" {
						return fmt.Errorf("the catalog read empty (apps.# = %q)", n)
					}
					return nil
				},
			),
		}},
	})
}

// TestAccInstanceTwoSizes creates two instances of one app at
// different sizes — the catalog-resolved identity, the allocation, and
// the async deploy wait, for real — then destroys both and asserts the
// wire agrees.
func TestAccInstanceTwoSizes(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC not set — the acceptance lane runs nightly/tags/dispatch, never per-PR (wallet cost)")
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: accProtoV6(),
		PreCheck:                 func() { accPreCheck(t) },
		CheckDestroy:             accInstancesGone(t),
		Steps: []resource.TestStep{
			{
				Config: accVarDecls + `
variable "app_slug" {
  type = string
}

data "fluxvale_app_versions" "test" {
  app = var.app_slug
}

# Two instances, different sizes: the catalog data source resolves the
# version (newest first), the slug form feeds the instances.
resource "fluxvale_instance" "small" {
  name        = "acc-small"
  app         = var.app_slug
  app_version = data.fluxvale_app_versions.test.app_versions[0].version
  cpu_cores   = 0.5
  memory_mb   = 256
  storage_gb  = 1
}

resource "fluxvale_instance" "big" {
  name        = "acc-big"
  app         = var.app_slug
  app_version = data.fluxvale_app_versions.test.app_versions[0].version
  cpu_cores   = 1.0
  memory_mb   = 512
  storage_gb  = 2
}
				`,
				ConfigVariables: config.Variables{
					"api_url":  accVars(t)["api_url"],
					"token":    accVars(t)["token"],
					"app_slug": config.StringVariable(accAppSlug()),
				},
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("fluxvale_instance.small", "name", "acc-small"),
					resource.TestCheckResourceAttr("fluxvale_instance.small", "cpu_cores", "0.5"),
					resource.TestCheckResourceAttr("fluxvale_instance.small", "memory_mb", "256"),
					resource.TestCheckResourceAttr("fluxvale_instance.small", "storage_gb", "1"),
					resource.TestCheckResourceAttr("fluxvale_instance.small", "status", "running"),
					resource.TestCheckResourceAttr("fluxvale_instance.big", "cpu_cores", "1"),
					resource.TestCheckResourceAttr("fluxvale_instance.big", "memory_mb", "512"),
					resource.TestCheckResourceAttr("fluxvale_instance.big", "storage_gb", "2"),
					resource.TestCheckResourceAttr("fluxvale_instance.big", "status", "running"),
					resource.TestCheckResourceAttrSet("fluxvale_instance.small", "organization_id"),
				),
			},
		},
	})
}

// TestAccInstanceBackups is the R2 wiring from the example stack, for
// real: one instance, one settings row fed by env-provided R2
// coordinates. Skips as a class without the full set — and the org
// must have paid ONCE for backups (the platform's paywall is the
// server's; a 403 here is that gate, not a bug).
func TestAccInstanceBackups(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC not set — the acceptance lane runs nightly/tags/dispatch, never per-PR (wallet cost)")
	}
	accSkip(t, "backups",
		"TF_ACC_BACKUP_ENDPOINT", "TF_ACC_BACKUP_BUCKET",
		"TF_ACC_BACKUP_ACCESS_KEY_ID", "TF_ACC_BACKUP_SECRET_ACCESS_KEY")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: accProtoV6(),
		PreCheck:                 func() { accPreCheck(t) },
		CheckDestroy:             accInstancesGone(t), // the settings row cascades with its instance
		Steps: []resource.TestStep{{
			Config: accVarDecls + `
variable "app_slug" {
  type = string
}

variable "backup_endpoint" {
  type = string
}

variable "backup_bucket" {
  type = string
}

variable "backup_access_key_id" {
  type = string
}

variable "backup_secret_access_key" {
  type      = string
  sensitive = true
}

data "fluxvale_app_versions" "test" {
  app = var.app_slug
}

resource "fluxvale_instance" "b" {
  name        = "acc-backups"
  app         = var.app_slug
  app_version = data.fluxvale_app_versions.test.app_versions[0].version
  cpu_cores   = 0.5
  memory_mb   = 256
  storage_gb  = 1
}

resource "fluxvale_instance_backups" "b" {
  instance_id       = fluxvale_instance.b.id
  endpoint          = var.backup_endpoint
  bucket            = var.backup_bucket
  access_key_id     = var.backup_access_key_id
  secret_access_key = var.backup_secret_access_key
  prefix            = "fluxvale/acc"
  retention_keep    = 7
}
			`,
			ConfigVariables: config.Variables{
				"api_url":                  accVars(t)["api_url"],
				"token":                    accVars(t)["token"],
				"app_slug":                 config.StringVariable(accAppSlug()),
				"backup_endpoint":          config.StringVariable(os.Getenv("TF_ACC_BACKUP_ENDPOINT")),
				"backup_bucket":            config.StringVariable(os.Getenv("TF_ACC_BACKUP_BUCKET")),
				"backup_access_key_id":     config.StringVariable(os.Getenv("TF_ACC_BACKUP_ACCESS_KEY_ID")),
				"backup_secret_access_key": config.StringVariable(os.Getenv("TF_ACC_BACKUP_SECRET_ACCESS_KEY")),
			},
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("fluxvale_instance_backups.b", "bucket", os.Getenv("TF_ACC_BACKUP_BUCKET")),
				resource.TestCheckResourceAttr("fluxvale_instance_backups.b", "endpoint", os.Getenv("TF_ACC_BACKUP_ENDPOINT")),
				resource.TestCheckResourceAttr("fluxvale_instance_backups.b", "prefix", "fluxvale/acc"),
				resource.TestCheckResourceAttr("fluxvale_instance_backups.b", "retention_keep", "7"),
				resource.TestCheckResourceAttr("fluxvale_instance_backups.b", "enabled", "true"),
				resource.TestCheckResourceAttrSet("fluxvale_instance_backups.b", "id"),
			),
		}},
	})
}

// TestAccAccessGateway enables the org's singleton gateway — the one
// resource whose destroy canNOT clean up (the API has no delete route;
// the row persists and bills). Subsequent runs adopt the existing
// gateway (the create's conflict path), so the nightly is idempotent —
// but the FIRST run is a standing monthly charge on the org. The PA
// opts to keep or panel-opt-out between runs.
func TestAccAccessGateway(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC not set — the acceptance lane runs nightly/tags/dispatch, never per-PR (wallet cost)")
	}
	accSkip(t, "gateway", "TF_ACC_TAILNET_DOMAIN", "TF_ACC_TAILSCALE_AUTH_KEY")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: accProtoV6(),
		PreCheck:                 func() { accPreCheck(t) },
		// CheckDestroy is deliberately nil: destroy removes the resource
		// from state only — there is no API delete route to assert
		// against (the resource's own docs).
		Steps: []resource.TestStep{{
			Config: accVarDecls + `
variable "tailnet_domain" {
  type = string
}

variable "tailscale_auth_key" {
  type      = string
  sensitive = true
}

resource "fluxvale_access_gateway" "g" {
  tailnet_domain = var.tailnet_domain
  auth_key       = var.tailscale_auth_key
}
`,
			ConfigVariables: config.Variables{
				"api_url":            accVars(t)["api_url"],
				"token":              accVars(t)["token"],
				"tailnet_domain":     config.StringVariable(os.Getenv("TF_ACC_TAILNET_DOMAIN")),
				"tailscale_auth_key": config.StringVariable(os.Getenv("TF_ACC_TAILSCALE_AUTH_KEY")),
			},
			Check: resource.ComposeTestCheckFunc(
				// The opaque minted node name (fv-<8 hex>) — never derived
				// from org names.
				resource.TestCheckResourceAttrWith("fluxvale_access_gateway.g", "hostname", func(v string) error {
					if len(v) < 3 || v[:3] != "fv-" {
						return fmt.Errorf("hostname = %q, want the fv- prefix", v)
					}
					return nil
				}),
				resource.TestCheckResourceAttrSet("fluxvale_access_gateway.g", "id"),
				resource.TestCheckResourceAttrSet("fluxvale_access_gateway.g", "organization_id"),
			),
		}},
	})
}

// TestAccCustomDomain registers the org's custom domain WITHOUT
// waiting for verification (the wait is bounded at 45m and the
// observer's cadence is ~15m — not nightly material; a dedicated
// dispatch with wait_for_verification = true is the deliberate
// rehearsal). VERIFIED IS NOT RESOLUTION — see the example README.
func TestAccCustomDomain(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC not set — the acceptance lane runs nightly/tags/dispatch, never per-PR (wallet cost)")
	}
	if !accHasResource("fluxvale_custom_domain") {
		t.Skip("custom_domain lands with #548 — the provider does not register the type on this branch yet")
	}
	accSkip(t, "custom domain", "TF_ACC_DOMAIN")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: accProtoV6(),
		PreCheck:                 func() { accPreCheck(t) },
		// CheckDestroy nil until #548's client lands on main: the
		// resource's delete is a synchronous row teardown, asserted
		// client-side once the method exists.
		Steps: []resource.TestStep{{
			Config: accVarDecls + `
variable "domain" {
  type = string
}

resource "fluxvale_custom_domain" "personal" {
  domain                = var.domain
  wait_for_verification = false
}
`,
			ConfigVariables: config.Variables{
				"api_url": accVars(t)["api_url"],
				"token":   accVars(t)["token"],
				"domain":  config.StringVariable(os.Getenv("TF_ACC_DOMAIN")),
			},
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("fluxvale_custom_domain.personal", "kind", "custom"),
				resource.TestCheckResourceAttr("fluxvale_custom_domain.personal", "state", "pending"),
			),
		}},
	})
}
