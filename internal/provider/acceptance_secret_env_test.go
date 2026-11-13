package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/config"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// TestAccInstanceSecretEnv is the real-wire proof for write-only
// secrets (#764): apply a secret, prove the value is absent from
// terraform.tfstate and the saved plan, rotate with a version bump and
// read the running app's own reported environment, then remove the key
// and watch it disappear.
//
// The running-app proof is Vikunja's /api/v1/info, whose
// auth.local.registration_enabled is env-derived and not persisted.
// No cluster credentials are needed, so the nightly acceptance lane
// proves rotation and removal too.
//
// Values travel as sensitive variables, never config literals, and
// every assertion reports key names or counts only — a leaked value in
// a failure message is itself a bug.
func TestAccInstanceSecretEnv(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC not set — the acceptance lane runs nightly/tags/dispatch, never per-PR (wallet cost)")
	}
	const (
		address = "fluxvale_instance.secret"
		regKey  = "VIKUNJA_SERVICE_ENABLEREGISTRATION"
		regOn   = "true"
		regOff  = "false"
	)
	secretValueOne := accRandomSecret(t)
	secretValueTwo := accRandomSecret(t)

	var id string
	checkID := func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[address]
		if !ok {
			return fmt.Errorf("%s is not in state", address)
		}
		if id == "" {
			id = rs.Primary.ID
			return nil
		}
		if rs.Primary.ID != id {
			return fmt.Errorf("instance id changed across a secret update: %q -> %q", id, rs.Primary.ID)
		}
		return nil
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: accProtoV6(),
		PreCheck:                 func() { accPreCheck(t) },
		CheckDestroy:             accInstancesGone(t),
		Steps: []resource.TestStep{
			{
				Config:          accSecretEnvConfig(true, accSecretBlock(regOn, 1)),
				ConfigVariables: accSecretVars(t, secretValueOne),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(address, "status", "running"),
					resource.TestCheckResourceAttr(address, "secret_env_vars_keys.#", "2"),
					resource.TestCheckResourceAttr(address, "secret_env_vars_keys.0", "ACC_SECRET"),
					resource.TestCheckResourceAttr(address, "secret_env_vars_keys.1", regKey),
					resource.TestCheckNoResourceAttr(address, "secret_env_vars.ACC_SECRET"),
					checkID,
					accStateHasNoSecret(t, address, secretValueOne),
					accRunningRegistrationIs(t, address, true),
				),
			},
			{
				// Staging's app ResourceQuota equals one pod, so the
				// platform's rolling restart cannot surge. Stop first,
				// rotate (the env PATCH writes the Secret while
				// stopped), then start onto the new environment.
				Config:          accSecretEnvConfig(false, accSecretBlock(regOff, 2)),
				ConfigVariables: accSecretVars(t, secretValueTwo),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(address, "status", "stopped"),
					resource.TestCheckResourceAttr(address, "secret_env_vars_keys.#", "2"),
					accStateHasNoSecret(t, address, secretValueOne),
					accStateHasNoSecret(t, address, secretValueTwo),
					checkID,
				),
			},
			{
				Config:          accSecretEnvConfig(true, accSecretBlock(regOff, 2)),
				ConfigVariables: accSecretVars(t, secretValueTwo),
				Check: resource.ComposeTestCheckFunc(
					accRunningRegistrationIs(t, address, false),
					accWaitStatus(t, address, "running"),
					checkID,
				),
			},
			{
				Config:          accSecretEnvConfig(false, "  secret_env_vars = {}\n  secret_env_vars_version = 3"),
				ConfigVariables: accSecretVars(t, secretValueTwo),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(address, "status", "stopped"),
					resource.TestCheckResourceAttr(address, "secret_env_vars_keys.#", "0"),
					accStateHasNoSecret(t, address, secretValueOne),
					accStateHasNoSecret(t, address, secretValueTwo),
					checkID,
				),
			},
			{
				Config:          accSecretEnvConfig(true, "  secret_env_vars = {}\n  secret_env_vars_version = 3"),
				ConfigVariables: accSecretVars(t, secretValueTwo),
				Check: resource.ComposeTestCheckFunc(
					accRunningRegistrationIs(t, address, true),
					accWaitStatus(t, address, "running"),
					checkID,
				),
			},
		},
	})
}

// The values here are deliberately not secret: the state-grep proof
// uses ACC_SECRET (random per run); registration is the running app's
// observable signal and is not persisted by Vikunja.
func accSecretBlock(registration string, version int) string {
	return fmt.Sprintf(`  secret_env_vars = {
    ACC_SECRET                         = var.secret_value
    VIKUNJA_SERVICE_ENABLEREGISTRATION = %q
  }
  secret_env_vars_version = %d`, registration, version)
}

func accSecretEnvConfig(running bool, secretBlock string) string {
	return accVarDecls + fmt.Sprintf(`
variable "secret_value" {
  type      = string
  sensitive = true
}

variable "organization" {
  type    = string
  default = ""
}

data "fluxvale_app_versions" "test" {
  app = "vikunja"
}

resource "fluxvale_instance" "secret" {
  name         = "acc-secret-env"
  organization = var.organization != "" ? var.organization : null
  app          = "vikunja"
  app_version  = data.fluxvale_app_versions.test.app_versions[0].version
  cpu_cores    = 0.5
  memory_mb    = 512
  storage_gb   = 5

  env_vars = { ACC_PLAIN = "1" }

  running = %t
%s
}
`, running, secretBlock)
}

func accSecretVars(t *testing.T, secret string) config.Variables {
	t.Helper()
	v := accVars(t)
	v["secret_value"] = config.StringVariable(secret)
	v["organization"] = config.StringVariable(os.Getenv("TF_ACC_ORGANIZATION"))
	return v
}

// accStateHasNoSecret mirrors `grep -c '<value>' terraform.tfstate`:
// zero occurrences in the state's attribute tree, and — when
// TF_ACC_TEMP_DIR is set — in the actual state file plugin-testing
// wrote. Only the count is logged; never the value.
func accStateHasNoSecret(t *testing.T, address, value string) resource.TestCheckFunc {
	t.Helper()
	return func(s *terraform.State) error {
		raw, err := json.Marshal(s)
		if err != nil {
			return fmt.Errorf("marshal state: %w", err)
		}
		count := bytes.Count(raw, []byte(value))
		if count != 0 {
			return fmt.Errorf("state attribute tree contains the secret value %d time(s) for %s — write-only leaked", count, address)
		}

		fileCount := 0
		filesChecked := 0
		if base := os.Getenv("TF_ACC_TEMP_DIR"); base != "" {
			matches, _ := filepath.Glob(filepath.Join(base, "plugintest*", "work*", "terraform.tfstate"))
			for _, path := range matches {
				blob, err := os.ReadFile(path)
				if err != nil {
					continue
				}
				filesChecked++
				fileCount += bytes.Count(blob, []byte(value))
			}
		}
		if fileCount != 0 {
			return fmt.Errorf("terraform.tfstate contains the secret value %d time(s) for %s — write-only leaked", fileCount, address)
		}
		t.Logf("state_secret_grep=%d (attribute tree) file_grep=%d files_checked=%d %s", count, fileCount, filesChecked, address)
		return nil
	}
}

// accWaitStatus polls the API until the row reaches want — the start
// verb returns :starting, and the next step's stop needs :running.
func accWaitStatus(t *testing.T, address, want string) resource.TestCheckFunc {
	t.Helper()
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[address]
		if !ok {
			return fmt.Errorf("%s is not in state", address)
		}
		c := accClient(t)
		deadline := time.Now().Add(5 * time.Minute)
		last := ""
		for time.Now().Before(deadline) {
			in, err := c.GetInstance(context.Background(), rs.Primary.ID)
			if err == nil {
				last = in.Status
				if in.Status == want {
					return nil
				}
				if in.Status == "error" || in.Status == "lost" {
					return fmt.Errorf("instance %s entered %q while waiting for %q", rs.Primary.ID, in.Status, want)
				}
			}
			time.Sleep(3 * time.Second)
		}
		return fmt.Errorf("instance %s did not reach %q (last %q)", rs.Primary.ID, want, last)
	}
}

// accRunningRegistrationIs is the running-app proof: Vikunja reports
// auth.local.registration_enabled on the public /api/v1/info route,
// directly from its env. Polling covers the start/deploy window
// without cluster access.
func accRunningRegistrationIs(t *testing.T, address string, want bool) resource.TestCheckFunc {
	t.Helper()
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[address]
		if !ok {
			return fmt.Errorf("%s is not in state", address)
		}
		slug := rs.Primary.Attributes["slug"]
		if slug == "" {
			return fmt.Errorf("%s has no slug in state", address)
		}
		url := "https://" + slug + ".fluxvale.app/api/v1/info"
		client := &http.Client{Timeout: 10 * time.Second}
		deadline := time.Now().Add(5 * time.Minute)
		last := false
		for time.Now().Before(deadline) {
			resp, err := client.Get(url)
			if err == nil {
				var body struct {
					Auth struct {
						Local struct {
							RegistrationEnabled bool `json:"registration_enabled"`
						} `json:"local"`
					} `json:"auth"`
				}
				decErr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body)
				resp.Body.Close()
				if decErr == nil {
					last = body.Auth.Local.RegistrationEnabled
					if last == want {
						t.Logf("running_env=VIKUNJA_SERVICE_ENABLEREGISTRATION matches the applied version")
						return nil
					}
				}
			}
			time.Sleep(5 * time.Second)
		}
		return fmt.Errorf("running app did not report the applied registration setting (last %t, want %t)", last, want)
	}
}

func accRandomSecret(t *testing.T) string {
	t.Helper()
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("random secret: %v", err)
	}
	return "acc-" + hex.EncodeToString(buf[:])
}
