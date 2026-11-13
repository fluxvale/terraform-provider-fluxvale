package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/config"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// TestAccInstanceSecretEnv is the real-wire proof for write-only
// secrets (#764): apply a secret, prove the value is absent from
// terraform.tfstate, rotate with a version bump and read the running
// app's environment, then remove the key and watch it disappear.
//
// The value travels as a sensitive variable, never a config literal,
// and every assertion reports key names or counts only — a leaked
// value in a failure message is itself a bug.
//
// The running-app proof needs cluster read access and skips without
// TF_ACC_KUBECTL_CONTEXT (CI's acceptance lane has the API token but
// not the staging kubeconfig). Everything else runs with the normal
// acceptance env.
func TestAccInstanceSecretEnv(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC not set — the acceptance lane runs nightly/tags/dispatch, never per-PR (wallet cost)")
	}

	const address = "fluxvale_instance.secret"
	secretOne := accRandomSecret(t)
	secretTwo := accRandomSecret(t)

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
				Config: accSecretEnvConfig(`  secret_env_vars = { ACC_SECRET = var.secret_value }
  secret_env_vars_version = 1`),
				ConfigVariables: accSecretVars(t, secretOne),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(address, "status", "running"),
					resource.TestCheckResourceAttr(address, "secret_env_vars_keys.#", "1"),
					resource.TestCheckResourceAttr(address, "secret_env_vars_keys.0", "ACC_SECRET"),
					resource.TestCheckNoResourceAttr(address, "secret_env_vars.ACC_SECRET"),
					checkID,
					accStateHasNoSecret(t, address, secretOne),
					accRunningEnvIs(t, address, "ACC_SECRET", secretOne),
				),
			},
			{
				// Staging's app ResourceQuota equals one pod, so the
				// platform's rolling restart cannot surge. Stop first,
				// rotate (the env PATCH writes the Secret while
				// stopped), then start onto the new environment.
				Config: accSecretEnvConfig(`  running = false
  secret_env_vars = { ACC_SECRET = var.secret_value }
  secret_env_vars_version = 2`),
				ConfigVariables: accSecretVars(t, secretTwo),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(address, "status", "stopped"),
					resource.TestCheckResourceAttr(address, "secret_env_vars_keys.#", "1"),
					resource.TestCheckResourceAttr(address, "secret_env_vars_keys.0", "ACC_SECRET"),
					// The first value must not survive in state either.
					accStateHasNoSecret(t, address, secretOne),
					accStateHasNoSecret(t, address, secretTwo),
					checkID,
				),
			},
			{
				Config: accSecretEnvConfig(`  running = true
  secret_env_vars = { ACC_SECRET = var.secret_value }
  secret_env_vars_version = 2`),
				ConfigVariables: accSecretVars(t, secretTwo),
				Check: resource.ComposeTestCheckFunc(
					accRunningEnvIs(t, address, "ACC_SECRET", secretTwo),
					accWaitStatus(t, address, "running"),
					checkID,
				),
			},
			{
				Config: accSecretEnvConfig(`  running = false
  secret_env_vars = {}
  secret_env_vars_version = 3`),
				ConfigVariables: accSecretVars(t, secretTwo),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(address, "status", "stopped"),
					resource.TestCheckResourceAttr(address, "secret_env_vars_keys.#", "0"),
					accStateHasNoSecret(t, address, secretOne),
					accStateHasNoSecret(t, address, secretTwo),
					checkID,
				),
			},
			{
				Config: accSecretEnvConfig(`  running = true
  secret_env_vars = {}
  secret_env_vars_version = 3`),
				ConfigVariables: accSecretVars(t, secretTwo),
				Check: resource.ComposeTestCheckFunc(
					accRunningEnvAbsent(t, address, "ACC_SECRET"),
					accWaitStatus(t, address, "running"),
					checkID,
				),
			},
		},
	})
}

func accSecretEnvConfig(secretBlock string) string {
	return accVarDecls + `
variable "app_slug" {
  type = string
}

variable "secret_value" {
  type      = string
  sensitive = true
}

variable "organization" {
  type    = string
  default = ""
}

data "fluxvale_app_versions" "test" {
  app = var.app_slug
}

resource "fluxvale_instance" "secret" {
  name         = "acc-secret-env"
  organization = var.organization != "" ? var.organization : null
  app         = var.app_slug
  app_version = data.fluxvale_app_versions.test.app_versions[0].version
  cpu_cores   = 0.5
  memory_mb   = 256
  storage_gb  = 1

  env_vars = { ACC_PLAIN = "1" }

` + secretBlock + `
}
`
}

func accSecretVars(t *testing.T, secret string) config.Variables {
	t.Helper()
	v := accAppVars(t)
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

// accRunningEnvIs reads the running app's environment through the
// cluster: poll printenv inside the pod until it matches, so a
// terminating old pod cannot mask the new environment. The value
// never enters a log or error.
func accRunningEnvIs(t *testing.T, address, key, want string) resource.TestCheckFunc {
	t.Helper()
	return func(s *terraform.State) error {
		ns, ok := accInstanceNamespace(s, address)
		if !ok {
			return fmt.Errorf("%s has no namespace in state", address)
		}
		if !accHasKubectl() {
			t.Logf("running-env proof skipped for %s — TF_ACC_KUBECTL_CONTEXT is unset", key)
			return nil
		}
		if err := accWaitRunningEnv(ns, key, want); err != nil {
			return err
		}
		t.Logf("running_env=%s matches the applied version", key)
		return nil
	}
}

func accRunningEnvAbsent(t *testing.T, address, key string) resource.TestCheckFunc {
	t.Helper()
	return func(s *terraform.State) error {
		ns, ok := accInstanceNamespace(s, address)
		if !ok {
			return fmt.Errorf("%s has no namespace in state", address)
		}
		if !accHasKubectl() {
			t.Logf("running-env removal proof skipped for %s — TF_ACC_KUBECTL_CONTEXT is unset", key)
			return nil
		}
		if err := accWaitRunningEnvAbsent(ns, key); err != nil {
			return err
		}
		t.Logf("running_env_absent=%s", key)
		return nil
	}
}

func accInstanceNamespace(s *terraform.State, address string) (string, bool) {
	rs, ok := s.RootModule().Resources[address]
	if !ok {
		return "", false
	}
	ns := rs.Primary.Attributes["namespace"]
	return ns, ns != ""
}

func accHasKubectl() bool {
	return os.Getenv("TF_ACC_KUBECTL_CONTEXT") != ""
}

// accWaitRunningEnv polls the pod's own environment until it equals
// the applied value. Rollout status is not used: a terminating old pod
// can hold the Deployment's rollout open after the new pod is already
// serving. The deadline is the proof window; only key names and pod
// statuses appear in errors.
func accWaitRunningEnv(ns, key, want string) error {
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		if got, err := accKubectlPrintenvOptional(ns, key); err == nil && got == want {
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return accRunningEnvTimeout(ns, key, false)
}

func accWaitRunningEnvAbsent(ns, key string) error {
	deadline := time.Now().Add(5 * time.Minute)
	empty := 0
	for time.Now().Before(deadline) {
		got, err := accKubectlPrintenvOptional(ns, key)
		if err == nil && got == "" {
			empty++
			if empty >= 2 {
				return nil
			}
		} else {
			empty = 0
		}
		time.Sleep(5 * time.Second)
	}
	return accRunningEnvTimeout(ns, key, true)
}

func accRunningEnvTimeout(ns, key string, wantAbsent bool) error {
	ctx := os.Getenv("TF_ACC_KUBECTL_CONTEXT")
	status, _ := exec.Command("kubectl", "--context", ctx, "-n", ns, "get", "pods",
		"-o", "custom-columns=NAME:.metadata.name,PHASE:.status.phase,READY:.status.containerStatuses[*].ready,REASON:.status.containerStatuses[*].state.waiting.reason").CombinedOutput()
	what := "did not reach the applied value"
	if wantAbsent {
		what = "did not drop the removed key"
	}
	return fmt.Errorf("running app environment %s %s in %s; pods: %s",
		key, what, ns, strings.TrimSpace(string(status)))
}

// accKubectlPrintenvOptional returns "" when the variable is absent —
// the shell form keeps a missing variable from masquerading as a
// kubectl failure.
func accKubectlPrintenvOptional(ns, key string) (string, error) {
	ctx := os.Getenv("TF_ACC_KUBECTL_CONTEXT")
	cmd := exec.Command("kubectl", "--context", ctx, "-n", ns, "exec", "deploy/app", "--",
		"sh", "-c", `printenv "$1" || true`, "sh", key)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(string(out), "\n"), nil
}

func accRandomSecret(t *testing.T) string {
	t.Helper()
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("random secret: %v", err)
	}
	return "acc-" + hex.EncodeToString(buf[:])
}
