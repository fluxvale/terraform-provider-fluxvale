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

	"github.com/fluxvale/terraform-provider-fluxvale/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	frameworkresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/config"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// Acceptance tests: REAL resources against a REAL FluxVale org
// (#482's acceptance lane). NEVER per-PR — every step creates real,
// billing platform rows, so this runs nightly, on v* tags, and on
// demand (.github/workflows/acceptance.yml); the secret it
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
//	                          optional — the legacy (auth_key) gateway class skips
//	                          without the pair (the gateway persists after destroy
//	                          — no API delete route — and bills monthly)
//	TF_ACC_TAILNET_DOMAIN + TF_ACC_TAILSCALE_API_TOKEN
//	                          optional — the #836 mint gateway class skips
//	                          without the pair (same standing charge; the Tailscale token
//	                          needs the auth_keys scope)
//	TF_ACC_TAILNET_DOMAIN + TF_ACC_TAILSCALE_CLIENT_ID +
//	TF_ACC_TAILSCALE_CLIENT_SECRET
//	                          optional — the #836 lane-B OAuth-pair gateway
//	                          class skips without the trio (same standing charge;
//	                          the OAuth client needs the platform scopes —
//	                          auth_keys, dns, devices:routes write,
//	                          devices:core:read — and the fluxvale-gateway tag)
//	TF_ACC_DOMAIN             optional — the custom-domain test skips without it
//	TF_ACC_HOSTNAME_DOMAIN    optional — the instance-hostname class skips
//	                          without it; it names a standing VERIFIED custom
//	                          domain in the org (not the pending row the
//	                          TF_ACC_DOMAIN class registers and destroys per
//	                          run); the token needs instances:write (implies
//	                          read — mint, list, remove) and gateways:read
//	                          (via gateways:write — the domain-list resolver
//	                          the precheck drives)
//	TF_ACC_POCKET_ID_INSTANCE_ID
//	                          optional — the PocketID SSO test skips without it; it
//	                          names a custody-ready public PocketID instance in the
//	                          org, and the platform's pocketid_sso_wiring flag must
//	                          be on (seeded enabled)
//	TF_ACC_SSO_APP_SLUG       optional — the SSO test's app, default "vaultwarden"
//	                          (must carry an OIDC recipe in its version)
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
		t.Fatalf("TF_ACC_API_URL and TF_ACC_PAT must be set — mint a PAT on the platform " +
			"(scopes instances:write backups:write gateways:write catalog:read organizations:read organizations:write; " +
			"the org must have paid once for backups)")
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
			t.Skipf("%s skipped — %s is not set (mint it per the scopes in .github/workflows/acceptance.yml preflight)", test, e)
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

// TestAccOrganization creates a real team organization, renames it in
// place, and verifies import by slug. It never funds the org, so the
// final destroy removes it — the resource's real-server contract
// (actor-owned create, name-only rename, slug import, empty teardown).
func TestAccOrganization(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC not set — the acceptance lane runs nightly/tags/dispatch, never per-PR (wallet cost)")
	}
	if !accHasResource("fluxvale_organization") {
		t.Skip("fluxvale_organization is not registered in this provider build")
	}

	slug := fmt.Sprintf("acc-org-%d", time.Now().UnixNano())

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: accProtoV6(),
		PreCheck:                 func() { accPreCheck(t) },
		CheckDestroy: func(s *terraform.State) error {
			c := accClient(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			for addr, rs := range s.RootModule().Resources {
				if rs.Type != "fluxvale_organization" {
					continue
				}
				_, err := c.GetOrganization(ctx, rs.Primary.ID)
				if err == nil {
					return fmt.Errorf("organization %s (%s) still exists after destroy", rs.Primary.ID, addr)
				}
				var apiErr *client.APIError
				if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
					return fmt.Errorf("checking organization %s: %w", rs.Primary.ID, err)
				}
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: accVarDecls + fmt.Sprintf(`
resource "fluxvale_organization" "acc" {
  name = "Acceptance Org"
  slug = %q
}
`, slug),
				ConfigVariables: accVars(t),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("fluxvale_organization.acc", "slug", slug),
					resource.TestCheckResourceAttr("fluxvale_organization.acc", "name", "Acceptance Org"),
				),
			},
			{
				// The rename is an in-place PATCH: same id, same slug.
				Config: accVarDecls + fmt.Sprintf(`
resource "fluxvale_organization" "acc" {
  name = "Acceptance Org Renamed"
  slug = %q
}
`, slug),
				ConfigVariables: accVars(t),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("fluxvale_organization.acc", "name", "Acceptance Org Renamed"),
					resource.TestCheckResourceAttr("fluxvale_organization.acc", "slug", slug),
				),
			},
			{
				ResourceName:      "fluxvale_organization.acc",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
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

// accEnvInstanceConfig renders the env-update instance with the given
// env_vars block (empty omits the attribute — the null-map clear).
func accEnvInstanceConfig(envBlock string) string {
	return accVarDecls + `
variable "app_slug" {
  type = string
}

data "fluxvale_app_versions" "test" {
  app = var.app_slug
}

resource "fluxvale_instance" "env" {
  name        = "acc-env"
  app         = var.app_slug
  app_version = data.fluxvale_app_versions.test.app_versions[0].version
  cpu_cores   = 0.5
  memory_mb   = 256
  storage_gb  = 1
` + envBlock + `
}
`
}

// accAppVars is accVars plus the catalog app slug the instance configs
// resolve their version through.
func accAppVars(t *testing.T) config.Variables {
	t.Helper()
	v := accVars(t)
	v["app_slug"] = config.StringVariable(accAppSlug())
	return v
}

// TestAccInstanceEnvUpdates proves the in-place env merge on the real
// wire (#731, ADR-0052): create with two vars, change one + add one +
// remove one, then omit the map — the instance id never changes and no
// step plans a replacement. Runs nightly; the fake-server tests carry
// the per-PR proof.
func TestAccInstanceEnvUpdates(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC not set — the acceptance lane runs nightly/tags/dispatch, never per-PR (wallet cost)")
	}

	var id string
	checkID := func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources["fluxvale_instance.env"]
		if !ok {
			return fmt.Errorf("fluxvale_instance.env is not in state")
		}
		if id == "" {
			id = rs.Primary.ID
			return nil
		}
		if rs.Primary.ID != id {
			return fmt.Errorf("instance id changed across an env update: %q -> %q", id, rs.Primary.ID)
		}
		return nil
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: accProtoV6(),
		PreCheck:                 func() { accPreCheck(t) },
		CheckDestroy:             accInstancesGone(t),
		Steps: []resource.TestStep{
			{
				Config:          accEnvInstanceConfig(`  env_vars    = { SMOKE = "1", DROP = "bye" }`),
				ConfigVariables: accAppVars(t),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("fluxvale_instance.env", "status", "running"),
					resource.TestCheckResourceAttr("fluxvale_instance.env", "env_vars.SMOKE", "1"),
					resource.TestCheckResourceAttr("fluxvale_instance.env", "env_vars.DROP", "bye"),
					checkID,
				),
			},
			{
				Config:          accEnvInstanceConfig(`  env_vars    = { SMOKE = "2", ADDED = "yes" }`),
				ConfigVariables: accAppVars(t),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("fluxvale_instance.env", "env_vars.SMOKE", "2"),
					resource.TestCheckResourceAttr("fluxvale_instance.env", "env_vars.ADDED", "yes"),
					checkID,
				),
			},
			{
				Config:          accEnvInstanceConfig(""),
				ConfigVariables: accAppVars(t),
				Check: resource.ComposeTestCheckFunc(
					checkID,
					func(s *terraform.State) error {
						rs, ok := s.RootModule().Resources["fluxvale_instance.env"]
						if !ok {
							return fmt.Errorf("fluxvale_instance.env is not in state")
						}
						if n := rs.Primary.Attributes["env_vars.%"]; n != "" && n != "0" {
							return fmt.Errorf("env_vars still carries %s keys after the null-map apply", n)
						}
						return nil
					},
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

// TestAccAccessGatewayMint is the #836 mint class: the gateway enables
// with api_token alone — no auth key anywhere — and the platform mints
// the join key from it (the token needs the auth_keys scope; the
// tailnet's ACL must own tag:fluxvale-gateway). The same standing-
// charge caveats as the legacy class apply — the row persists (no API
// delete route) and bills monthly — and later runs adopt the existing
// gateway through the create's conflict path, so the nightly stays
// idempotent.
func TestAccAccessGatewayMint(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC not set — the acceptance lane runs nightly/tags/dispatch, never per-PR (wallet cost)")
	}
	accSkip(t, "gateway mint", "TF_ACC_TAILNET_DOMAIN", "TF_ACC_TAILSCALE_API_TOKEN")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: accProtoV6(),
		PreCheck:                 func() { accPreCheck(t) },
		// CheckDestroy is deliberately nil, as in the legacy class:
		// destroy removes the resource from state only — there is no
		// API delete route to assert against.
		Steps: []resource.TestStep{{
			Config: accVarDecls + `
variable "tailnet_domain" {
  type = string
}

variable "tailscale_api_token" {
  type      = string
  sensitive = true
}

resource "fluxvale_access_gateway" "g" {
  tailnet_domain = var.tailnet_domain
  api_token      = var.tailscale_api_token
}
`,
			ConfigVariables: config.Variables{
				"api_url":             accVars(t)["api_url"],
				"token":               accVars(t)["token"],
				"tailnet_domain":      config.StringVariable(os.Getenv("TF_ACC_TAILNET_DOMAIN")),
				"tailscale_api_token": config.StringVariable(os.Getenv("TF_ACC_TAILSCALE_API_TOKEN")),
			},
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttrWith("fluxvale_access_gateway.g", "hostname", func(v string) error {
					if len(v) < 3 || v[:3] != "fv-" {
						return fmt.Errorf("hostname = %q, want the fv- prefix", v)
					}
					return nil
				}),
				resource.TestCheckResourceAttrSet("fluxvale_access_gateway.g", "id"),
				resource.TestCheckResourceAttrSet("fluxvale_access_gateway.g", "organization_id"),
				// The mint class's proof: the gateway reaches active —
				// the platform minted the join key from the token and
				// the node joined the tailnet.
				accGatewayActive(t),
			),
		}},
	})
}

// TestAccAccessGatewayPair is the #836 lane-B pair class: the gateway
// enables with the OAuth client pair alone — ts_client_id +
// ts_client_secret, no auth key or console token anywhere — and the
// platform exchanges the pair for a tailnet-scoped token, then mints
// the join key from it. The client must carry the platform scopes
// (auth_keys, dns, devices:routes write, devices:core:read) and the
// fluxvale-gateway tag (the minted key's tag; an unowned tag fails
// the join, not the mint). The same standing-charge caveats as the
// other gateway classes apply — the row persists (no API delete
// route) and bills monthly — and later runs adopt the existing
// gateway through the create's conflict path, so the nightly stays
// idempotent.
func TestAccAccessGatewayPair(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC not set — the acceptance lane runs nightly/tags/dispatch, never per-PR (wallet cost)")
	}
	accSkip(t, "gateway pair",
		"TF_ACC_TAILNET_DOMAIN", "TF_ACC_TAILSCALE_CLIENT_ID", "TF_ACC_TAILSCALE_CLIENT_SECRET")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: accProtoV6(),
		PreCheck:                 func() { accPreCheck(t) },
		// CheckDestroy is deliberately nil, as in the other gateway
		// classes: destroy removes the resource from state only — there
		// is no API delete route to assert against.
		Steps: []resource.TestStep{{
			Config: accVarDecls + `
variable "tailnet_domain" {
  type = string
}

variable "tailscale_client_id" {
  type = string
}

variable "tailscale_client_secret" {
  type      = string
  sensitive = true
}

resource "fluxvale_access_gateway" "g" {
  tailnet_domain   = var.tailnet_domain
  ts_client_id     = var.tailscale_client_id
  ts_client_secret = var.tailscale_client_secret
}
`,
			ConfigVariables: config.Variables{
				"api_url":                 accVars(t)["api_url"],
				"token":                   accVars(t)["token"],
				"tailnet_domain":          config.StringVariable(os.Getenv("TF_ACC_TAILNET_DOMAIN")),
				"tailscale_client_id":     config.StringVariable(os.Getenv("TF_ACC_TAILSCALE_CLIENT_ID")),
				"tailscale_client_secret": config.StringVariable(os.Getenv("TF_ACC_TAILSCALE_CLIENT_SECRET")),
			},
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttrWith("fluxvale_access_gateway.g", "hostname", func(v string) error {
					if len(v) < 3 || v[:3] != "fv-" {
						return fmt.Errorf("hostname = %q, want the fv- prefix", v)
					}
					return nil
				}),
				resource.TestCheckResourceAttrSet("fluxvale_access_gateway.g", "id"),
				resource.TestCheckResourceAttrSet("fluxvale_access_gateway.g", "organization_id"),
				// The pair class's proof: the gateway reaches active —
				// the platform exchanged the pair and minted the join
				// key, and the node joined the tailnet.
				accGatewayActive(t),
			),
		}},
	})
}

// accGatewayActive polls the singleton read until the gateway serves:
// the create lands :provisioning and the observer (about a minute of
// cadence) is the only promoter to :active.
func accGatewayActive(t *testing.T) func(*terraform.State) error {
	t.Helper()
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources["fluxvale_access_gateway.g"]
		if !ok {
			return fmt.Errorf("fluxvale_access_gateway.g is not in state")
		}
		c := accClient(t)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		for {
			gw, err := c.GetOrgGateway(ctx, rs.Primary.Attributes["organization_id"])
			if err != nil {
				return fmt.Errorf("reading the gateway: %w", err)
			}
			switch gw.State {
			case "active":
				return nil
			case "needs_attention", "suspended", "destroying":
				msg := "no message"
				if gw.StatusMessage != nil {
					msg = *gw.StatusMessage
				}
				return fmt.Errorf("gateway landed in %s: %s", gw.State, msg)
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("gateway still %q after 10m", gw.State)
			case <-time.After(15 * time.Second):
			}
		}
	}
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

// accSsoAppSlug is the SSO acceptance class's app — a version that
// declares an OIDC recipe (vaultwarden/vikunja in the seeded catalog).
// It is deliberately separate from TF_ACC_APP_SLUG: the other classes'
// app need not be SSO-capable.
func accSsoAppSlug() string {
	if s := os.Getenv("TF_ACC_SSO_APP_SLUG"); s != "" {
		return s
	}
	return "vaultwarden"
}

// TestAccPocketIdSso exercises the typed sign-in wiring on the real
// wire: create a fresh app instance, connect it to the org's
// custody-ready PocketID, read the wiring back, rotate the client
// secret by bumping rotate_secret_on (the client id and instance id
// must not move), then let destroy disconnect. Skips as a class
// without TF_ACC_POCKET_ID_INSTANCE_ID (the PocketID instance's id);
// the platform's pocketid_sso_wiring flag must be on (seeded enabled),
// and the PocketID instance must be public with held-key custody.
func TestAccPocketIdSso(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC not set — the acceptance lane runs nightly/tags/dispatch, never per-PR (wallet cost)")
	}
	accSkip(t, "pocket id sso", "TF_ACC_POCKET_ID_INSTANCE_ID")

	var firstClientID, firstSecretID string
	checkWiring := func(s *terraform.State) error {
		wiringRS, ok := s.RootModule().Resources["fluxvale_pocket_id_sso.sso"]
		if !ok {
			return fmt.Errorf("fluxvale_pocket_id_sso.sso is not in state")
		}
		instanceRS, ok := s.RootModule().Resources["fluxvale_instance.sso"]
		if !ok {
			return fmt.Errorf("fluxvale_instance.sso is not in state")
		}
		instanceID := instanceRS.Primary.ID
		if wiringRS.Primary.ID != instanceID {
			return fmt.Errorf("wiring id = %q, want the instance id %q (the one-wiring key)",
				wiringRS.Primary.ID, instanceID)
		}
		if got := wiringRS.Primary.Attributes["instance_id"]; got != instanceID {
			return fmt.Errorf("instance_id = %q, want %q", got, instanceID)
		}
		clientID := wiringRS.Primary.Attributes["pocket_id_client_id"]
		secretID := wiringRS.Primary.Attributes["pocket_id_secret_id"]
		if clientID == "" || secretID == "" {
			return fmt.Errorf("client/secret ids are empty (%q/%q) — the wiring did not read back", clientID, secretID)
		}
		if wiringRS.Primary.Attributes["wired_at"] == "" {
			return fmt.Errorf("wired_at is empty — the wiring did not read back")
		}
		if firstClientID == "" {
			firstClientID, firstSecretID = clientID, secretID
			return nil
		}
		if clientID != firstClientID {
			return fmt.Errorf("client id changed across a rotation: %q -> %q (rotation must not mint a new client)",
				firstClientID, clientID)
		}
		if secretID == firstSecretID {
			return fmt.Errorf("secret id did not rotate: still %q", secretID)
		}
		return nil
	}

	ssoConfig := func(rotate string) string {
		return accVarDecls + `
variable "app_slug" {
  type = string
}

variable "pocket_id_instance_id" {
  type = string
}

data "fluxvale_app_versions" "sso" {
  app = var.app_slug
}

resource "fluxvale_instance" "sso" {
  name        = "acc-pocket-id-sso"
  app         = var.app_slug
  app_version = data.fluxvale_app_versions.sso.app_versions[0].version
  cpu_cores   = 0.5
  memory_mb   = 256
  storage_gb  = 1
}

resource "fluxvale_pocket_id_sso" "sso" {
  instance_id           = fluxvale_instance.sso.id
  pocket_id_instance_id = var.pocket_id_instance_id
  rotate_secret_on      = "` + rotate + `"
}
`
	}

	// instanceOnlyConfig removes the SSO resource while keeping the
	// instance: the apply's only side effect is Delete → disconnect.
	// The variable declarations stay (an unused variable is legal) so
	// the same ConfigVariables feed every step.
	instanceOnlyConfig := accVarDecls + `
variable "app_slug" {
  type = string
}

variable "pocket_id_instance_id" {
  type = string
}

data "fluxvale_app_versions" "sso" {
  app = var.app_slug
}

resource "fluxvale_instance" "sso" {
  name        = "acc-pocket-id-sso"
  app         = var.app_slug
  app_version = data.fluxvale_app_versions.sso.app_versions[0].version
  cpu_cores   = 0.5
  memory_mb   = 256
  storage_gb  = 1
}
`

	// checkDisconnected proves the SSO resource's own Delete did the
	// disconnect: the wiring is 404 while the instance still answers.
	checkDisconnected := func(s *terraform.State) error {
		if _, ok := s.RootModule().Resources["fluxvale_pocket_id_sso.sso"]; ok {
			return fmt.Errorf("fluxvale_pocket_id_sso.sso is still in state — the step did not remove only the wiring")
		}
		rs, ok := s.RootModule().Resources["fluxvale_instance.sso"]
		if !ok {
			return fmt.Errorf("fluxvale_instance.sso is not in state")
		}
		c := accClient(t)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if _, err := c.GetInstance(ctx, rs.Primary.ID); err != nil {
			return fmt.Errorf("instance %s disappeared while removing only the wiring: %w", rs.Primary.ID, err)
		}
		if _, err := c.GetPocketIdWiring(ctx, rs.Primary.ID); err == nil {
			return fmt.Errorf("wiring for instance %s still exists after removing only the SSO resource", rs.Primary.ID)
		} else {
			var apiErr *client.APIError
			if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
				return fmt.Errorf("checking the disconnected wiring for instance %s: %w", rs.Primary.ID, err)
			}
		}
		return nil
	}

	ssoVars := config.Variables{
		"api_url":               accVars(t)["api_url"],
		"token":                 accVars(t)["token"],
		"app_slug":              config.StringVariable(accSsoAppSlug()),
		"pocket_id_instance_id": config.StringVariable(os.Getenv("TF_ACC_POCKET_ID_INSTANCE_ID")),
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: accProtoV6(),
		PreCheck:                 func() { accPreCheck(t) },
		CheckDestroy: func(s *terraform.State) error {
			// Dependents destroy before their instance: by here the
			// disconnect ran and the wiring (then the instance) is gone.
			if err := accInstancesGone(t)(s); err != nil {
				return err
			}
			rs, ok := s.RootModule().Resources["fluxvale_instance.sso"]
			if !ok {
				return fmt.Errorf("fluxvale_instance.sso is not in state")
			}
			c := accClient(t)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			_, err := c.GetPocketIdWiring(ctx, rs.Primary.ID)
			if err == nil {
				return fmt.Errorf("the PocketID wiring for instance %s still exists after destroy", rs.Primary.ID)
			}
			var apiErr *client.APIError
			if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
				return fmt.Errorf("checking the disconnected wiring for instance %s: %w", rs.Primary.ID, err)
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config:          ssoConfig("v1"),
				ConfigVariables: ssoVars,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("fluxvale_instance.sso", "status", "running"),
					resource.TestCheckResourceAttr("fluxvale_pocket_id_sso.sso", "rotate_secret_on", "v1"),
					resource.TestCheckResourceAttrSet("fluxvale_pocket_id_sso.sso", "pocket_id_client_id"),
					resource.TestCheckResourceAttrSet("fluxvale_pocket_id_sso.sso", "pocket_id_secret_id"),
					resource.TestCheckResourceAttrSet("fluxvale_pocket_id_sso.sso", "wired_at"),
					checkWiring,
				),
			},
			{
				Config:          ssoConfig("v2"),
				ConfigVariables: ssoVars,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("fluxvale_pocket_id_sso.sso", "rotate_secret_on", "v2"),
					checkWiring,
				),
			},
			{
				// Destroy ONLY the wiring: the instance stays, so the
				// 404 below can only come from the resource's Delete.
				Config:          instanceOnlyConfig,
				ConfigVariables: ssoVars,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("fluxvale_instance.sso", "status", "running"),
					checkDisconnected,
				),
			},
		},
	})
}

// TestAccInstanceHostname exercises the custom-address lifecycle on
// the real wire: mint under the org's standing verified domain, read
// back, remove while the instance lives (the step boundary destroy),
// re-mint, and import by the two-part id. Skips as a class without
// TF_ACC_HOSTNAME_DOMAIN — the name of a VERIFIED custom domain
// standing in the staging org (the nightly's TF_ACC_DOMAIN class
// registers and destroys a fresh pending row, which the mint refuses;
// a standing verified domain is a one-time runbook prerequisite).
//
// Runbook — the platform administrator's by-hand run (this class
// never runs per-PR; the nightly carries it once the secret lands):
//
//	export TF_ACC=1
//	export TF_ACC_API_URL=https://staging.fluxvale.com
//	export TF_ACC_PAT=<the six-scope staging token>
//	export TF_ACC_HOSTNAME_DOMAIN=<the standing verified domain's NAME, e.g. apps.staging.example.com>
//	export TF_ACC_PROVIDER_NAMESPACE=hashicorp        # plugin-testing registers the in-process
//	export TF_ACC_PROVIDER_HOST=registry.opentofu.org # provider under its default address; tofu resolves
//	                                                  # the implicit provider against registry.opentofu.org
//	export TF_ACC_TERRAFORM_PATH="$(mise which tofu)"
//	export TF_ACC_TERRAFORM_VERSION="$(mise x -- tofu version | head -1 | sed 's/^OpenTofu v//')"
//	mise x -- go test ./internal/provider -run TestAccInstanceHostname -count=1 -v
//
// The token needs the instances:write and gateways:write scopes (the
// six-scope staging token carries both). The prerequisite is one-time
// platform-administrator setup: one verified custom domain standing
// in the staging org — register it, delegate the _acme-challenge
// CNAME at the DNS host with proxied = false (the
// fluxvale_domain_delegation data source renders it), and wait out
// the observer's ~15-minute pass. The standing domain is reused by
// every run of this class and never destroyed by it.
func TestAccInstanceHostname(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC not set — the acceptance lane runs nightly/tags/dispatch, never per-PR (wallet cost)")
	}
	if !accHasResource("fluxvale_instance_hostname") {
		t.Skip("fluxvale_instance_hostname is not registered in this provider build")
	}
	accSkip(t, "instance hostname", "TF_ACC_HOSTNAME_DOMAIN")

	// The label is lowercase alnum + hyphens and unique per run: the
	// platform refuses a duplicate (access_domain_id, label), and a
	// prior run's leftover row must never read as this run's
	// duplicate. One label serves the whole class — step 3 removes
	// the row before step 4 re-mints it.
	label := fmt.Sprintf("acc-%d", time.Now().UnixNano())
	// The verified domain's access-domain id, resolved per class
	// (never per step): the config interpolates it, and PreCheck
	// re-asserts it fail-loud against the live org.
	domainID := accHostnameDomainID(t)

	// The class's instance — the TestAccInstanceTwoSizes shape (the
	// catalog resolves the version, small allocation), pinned public:
	// the custom address serves the public edge.
	instanceConfig := accVarDecls + fmt.Sprintf(`
data "fluxvale_app_versions" "test" {
  app = %q
}

resource "fluxvale_instance" "h" {
  name        = "acc-hostname"
  app         = %q
  app_version = data.fluxvale_app_versions.test.app_versions[0].version
  cpu_cores   = 0.5
  memory_mb   = 256
  storage_gb  = 1
  exposure    = "public"
}
`, accAppSlug(), accAppSlug())

	hostnameConfig := instanceConfig + fmt.Sprintf(`
resource "fluxvale_instance_hostname" "h" {
  instance_id      = fluxvale_instance.h.id
  access_domain_id = %q
  label            = %q
}
`, domainID, label)

	// The step-boundary destroy of ONLY the address, proved
	// mid-flight: the provider's DELETE ran while the instance
	// stands — no row carries the label, and the instance still
	// answers (a gone instance would make the list trivially empty).
	checkAddressRemoved := func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources["fluxvale_instance.h"]
		if !ok {
			return fmt.Errorf("fluxvale_instance.h is not in state")
		}
		c := accClient(t)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if _, err := c.GetInstance(ctx, rs.Primary.ID); err != nil {
			return fmt.Errorf("instance %s disappeared while removing only the address: %w", rs.Primary.ID, err)
		}
		rows, err := c.ListInstanceHostnames(ctx, rs.Primary.ID)
		if err != nil {
			return fmt.Errorf("listing the addresses of instance %s: %w", rs.Primary.ID, err)
		}
		for _, row := range rows {
			if row.Label == label {
				return fmt.Errorf("address %s (label %s) still exists on instance %s after the step-boundary destroy", row.ID, row.Label, rs.Primary.ID)
			}
		}
		return nil
	}

	// The create/read/re-mint assertions: the identity reads back
	// exactly as minted, and the row id is non-empty.
	hostnameChecks := resource.ComposeTestCheckFunc(
		resource.TestCheckResourceAttr("fluxvale_instance_hostname.h", "label", label),
		resource.TestCheckResourceAttrPair("fluxvale_instance_hostname.h", "instance_id", "fluxvale_instance.h", "id"),
		resource.TestCheckResourceAttr("fluxvale_instance_hostname.h", "access_domain_id", domainID),
		resource.TestCheckResourceAttrSet("fluxvale_instance_hostname.h", "id"),
	)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: accProtoV6(),
		PreCheck: func() {
			accPreCheck(t)
			accHostnameDomainID(t)
		},
		CheckDestroy: func(s *terraform.State) error {
			// The instance's own destroy cascades its address rows (the
			// platform deletes hostnames with their instance), so by the
			// final state this is trivially true — unless the destroy
			// ordering broke and a row outlived its instance. The
			// isolated remove proof is step 3's mid-flight check, not
			// this.
			c := accClient(t)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			for addr, rs := range s.RootModule().Resources {
				if rs.Type != "fluxvale_instance_hostname" {
					continue
				}
				instanceID := rs.Primary.Attributes["instance_id"]
				rows, err := c.ListInstanceHostnames(ctx, instanceID)
				if err != nil {
					return fmt.Errorf("listing the addresses of instance %s: %w", instanceID, err)
				}
				for _, row := range rows {
					if row.ID == rs.Primary.ID {
						return fmt.Errorf("instance hostname %s (%s) still exists after destroy", rs.Primary.ID, addr)
					}
				}
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				// Create + read: the mint under the verified domain.
				Config:          hostnameConfig,
				ConfigVariables: accVars(t),
				Check:           hostnameChecks,
			},
			{
				// Refresh consistency: the same config re-applies with
				// nothing to do — the read round-trips the row.
				Config:          hostnameConfig,
				ConfigVariables: accVars(t),
				Check:           hostnameChecks,
			},
			{
				// Remove while the instance lives: the config drops ONLY
				// the hostname resource, so the step boundary's destroy
				// is the real remove proof.
				Config:          instanceConfig,
				ConfigVariables: accVars(t),
				Check:           checkAddressRemoved,
			},
			{
				// Re-mint: re-create after removal — the
				// plan-that-rebuilds promise.
				Config:          hostnameConfig,
				ConfigVariables: accVars(t),
				Check:           hostnameChecks,
			},
			{
				// Import by the two-part id: the read surface is
				// instance-scoped (there is no GET by hostname id), so
				// the id is <instance_id>/<hostname_id> — a bare
				// ImportState would pass only the row id, which the
				// parser refuses. (The org class's bare import step is
				// the currently-red shape; do not repeat it.)
				ResourceName:      "fluxvale_instance_hostname.h",
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateIdFunc: func(s *terraform.State) (string, error) {
					instanceRS, ok := s.RootModule().Resources["fluxvale_instance.h"]
					if !ok {
						return "", fmt.Errorf("fluxvale_instance.h is not in state")
					}
					hostnameRS, ok := s.RootModule().Resources["fluxvale_instance_hostname.h"]
					if !ok {
						return "", fmt.Errorf("fluxvale_instance_hostname.h is not in state")
					}
					return instanceRS.Primary.ID + "/" + hostnameRS.Primary.ID, nil
				},
			},
		},
	})
}

// accHostnameDomainID resolves TF_ACC_HOSTNAME_DOMAIN to its
// access-domain id — fail loud, never skip (accPreCheck's stance: a
// nightly that proves nothing must RED, not green-skip). The staging
// user is single-org by design; the named domain must stand in that
// org and be verified — the mint refuses a pending or unverified one.
func accHostnameDomainID(t *testing.T) string {
	t.Helper()
	c := accClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	orgs, err := c.ListOrganizations(ctx)
	if err != nil {
		t.Fatalf("listing the organizations: %v", err)
	}
	if len(orgs) != 1 {
		t.Fatalf("want exactly one organization (the staging user is single-org by design), got %d — "+
			"point TF_ACC_PAT at the staging token", len(orgs))
	}
	domains, err := c.ListAccessDomains(ctx, orgs[0].ID)
	if err != nil {
		t.Fatalf("listing the org %s's access domains: %v", orgs[0].ID, err)
	}
	want := os.Getenv("TF_ACC_HOSTNAME_DOMAIN")
	names := make([]string, 0, len(domains))
	for _, d := range domains {
		if d.Domain == want {
			if d.State != "verified" {
				t.Fatalf("domain %s stands in the org but its state is %q, want \"verified\" — the mint refuses an "+
					"unverified domain. Finish the one-time setup: delegate the _acme-challenge CNAME at the DNS "+
					"host with proxied = false (the fluxvale_domain_delegation data source renders it), then wait "+
					"out the observer's ~15-minute pass.", want, d.State)
			}
			return d.ID
		}
		names = append(names, d.Domain)
	}
	t.Fatalf("domain %q does not stand in the org — its domains are: %s. TF_ACC_HOSTNAME_DOMAIN names a standing "+
		"VERIFIED custom domain (not the pending row the TF_ACC_DOMAIN class registers per run); register it per "+
		"the class's runbook.", want, strings.Join(names, ", "))
	return ""
}
