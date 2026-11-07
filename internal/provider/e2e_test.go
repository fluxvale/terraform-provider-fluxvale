package provider

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestRealTofuSmoke is the real-tofu gate: a REAL OpenTofu CLI driving
// the REAL compiled binary end to end — in TWO phases.
//
// Phase 1 (dev_overrides): plan, apply, destroy against a scripted
// fake API, exit codes and wire calls asserted. This is the phase that
// catches what in-process tests cannot: the reserved-`provider` root
// attribute (it sat green in the pipeline for two slices until this
// gate's first run caught it) and the unknown-organization class —
// schema validation and provider configuration run on the RPC path,
// which only a real CLI exercises.
//
// Phase 2 (filesystem mirror): the documented install path from
// docs/install.md, exercised for real — the compiled binary zipped
// into the registry's PACKED layout (zip directly in the type dir, NO
// version directory; the binary inside named terraform-provider-fluxvale_v<version>),
// a `filesystem_mirror` CLI config, then `tofu init` + `tofu validate`
// against the mirror alone (no registry contact). This is the phase
// that catches the docs-rot class: a mirror tree the docs teach but
// tofu refuses.
//
// Gated by TFPROVIDER_E2E=1 (set by ci.yml's tfprovider lane; skipped
// locally without it). Requires `tofu` on PATH — `mise install
// opentofu` (the mise.toml pin) — and works offline: no registry
// contact in either phase.
//
// NB: phase 1 has deliberately NO `tofu init` — that is the
// documented dev-override workflow (OpenTofu warns that init is
// unnecessary there, and tofu 1.13's init actively errors querying
// the registry for a provider that isn't published).
func TestRealTofuSmoke(t *testing.T) {
	if os.Getenv("TFPROVIDER_E2E") == "" {
		t.Skip("TFPROVIDER_E2E not set — the real-tofu gate runs in CI and via TFPROVIDER_E2E=1 go test -run TestRealTofu")
	}

	tofu, err := exec.LookPath("tofu")
	if err != nil {
		t.Fatalf("tofu not on PATH — mise install opentofu (the mise.toml pin): %v", err)
	}

	// The binary under test, built from the module root (two dirs up
	// from this package) — never a stale checkout copy.
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("module root: %v", err)
	}
	binDir := t.TempDir()
	bin := filepath.Join(binDir, "terraform-provider-fluxvale")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	// The -version proof on the built binary: the flag the release
	// workflow asserts per leg — a rename or wiring break reds HERE,
	// on a PR, not at tag time.
	out, err := exec.Command(bin, "-version").Output()
	if err != nil {
		t.Fatalf("%s -version: %v", bin, err)
	}
	if want := "terraform-provider-fluxvale " + Version; strings.TrimSpace(string(out)) != want {
		t.Fatalf("%s -version = %q, want exactly %q", bin, strings.TrimSpace(string(out)), want)
	}

	fake := newSmokeFake(t)
	work := t.TempDir()

	// dev_overrides: the CLI config lives outside the module; the
	// override dir is the one holding the binary.
	clipath := filepath.Join(work, "tofurc")
	cfg := fmt.Sprintf(`provider_installation {
  dev_overrides {
    "registry.terraform.io/fluxvale/fluxvale" = %q
  }
  direct {}
}
`, binDir)
	if err := os.WriteFile(clipath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write CLI config: %v", err)
	}

	module := fmt.Sprintf(`
terraform {
  required_providers {
    fluxvale = {
      source = "registry.terraform.io/fluxvale/fluxvale"
    }
  }
}

provider "fluxvale" {
  api_url = %q
  token   = "fvpat_e2e"
}

# The catalog, by slug — the data path a real config rides.
data "fluxvale_app_versions" "test" {
  app = "forgejo"
}

resource "fluxvale_instance" "a" {
  name        = "smoke-a"
  app         = "forgejo"
  app_version = data.fluxvale_app_versions.test.app_versions[0].version
  cpu_cores   = 0.5
  memory_mb   = 256
  storage_gb  = 1
}

# Different size, private, with env vars and a backups row.
resource "fluxvale_instance" "b" {
  name        = "smoke-b"
  app         = "forgejo"
  app_version = data.fluxvale_app_versions.test.app_versions[0].version
  cpu_cores   = 1.0
  memory_mb   = 512
  storage_gb  = 2
  exposure    = "private"
  env_vars    = { SMOKE = "1" }
}

# The example stack's headline posture: always-on (never sleeps).
resource "fluxvale_instance" "c" {
  name        = "smoke-c"
  app         = "forgejo"
  app_version = data.fluxvale_app_versions.test.app_versions[0].version
  cpu_cores   = 0.5
  memory_mb   = 256
  storage_gb  = 1
  always_on   = true
}

resource "fluxvale_instance_backups" "b" {
  instance_id       = fluxvale_instance.b.id
  endpoint          = "https://acct123.r2.cloudflarestorage.com"
  bucket            = "smoke-bucket"
  access_key_id     = "AKIAIOSFODNN7EXAMPLE"
  secret_access_key = "smoke-secret"
  prefix            = "fluxvale/smoke"
}

resource "fluxvale_access_gateway" "g" {
  tailnet_domain = "smoke.ts.net"
  auth_key       = "tskey-smoke"
  instances      = [fluxvale_instance.b.id]
}
`, fake.URL)
	if err := os.WriteFile(filepath.Join(work, "main.tf"), []byte(module), 0o600); err != nil {
		t.Fatalf("write module: %v", err)
	}

	runTofu := func(cliConfig string, args ...string) string {
		t.Helper()
		t.Logf("phase 1: tofu %s", strings.Join(args, " "))
		// A per-command deadline well under the go-test timeout: a
		// wedged apply (the provider's own poll deadline is 10m) dumps
		// its buffered output HERE instead of the panic trace.
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, tofu, args...)
		cmd.Dir = work
		cmd.Env = append(os.Environ(), "TF_CLI_CONFIG_FILE="+cliConfig, "TF_INPUT=0")
		out, err := cmd.CombinedOutput()
		t.Logf("phase 1: tofu %s done (err=%v)", strings.Join(args, " "), err)
		if err != nil {
			t.Fatalf("tofu %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}

	plan := runTofu(clipath, "plan", "-out=tfplan")
	if !strings.Contains(plan, "5 to add") {
		t.Fatalf("plan did not add the five resources:\n%s", plan)
	}
	runTofu(clipath, "apply", "tfplan")
	runTofu(clipath, "destroy", "-auto-approve")

	// The wire story: create → posture PATCHes (exposure on b,
	// always-on on c) → deploy → backup row → gateway + attach;
	// destroy: forget-only gateway, row DELETE, all three instances
	// DELETEd to a polled 404.
	fake.wantCounts(t,
		countExact("POST", "/api/v1/instances", 3),
		countSuffix("PATCH", "/exposure", 1),
		countSuffix("PATCH", "/always-on", 1),
		countSuffix("PATCH", "/deploy", 3),
		countSuffix("POST", "/backup-setting", 1),
		countExact("POST", "/api/v1/access-gateways", 1),
		countContains("POST", "/attachments", 1),
		countPrefix("DELETE", "/api/v1/instances/", 3),
		countPrefix("DELETE", "/api/v1/backup-settings/", 1),
		countContains("DELETE", "/attachments/", 0), // destroy is a state-only forget
	)

	// The write BODIES, not just counts:
	//
	// Both sized instances' create bodies rode the plan's allocations
	// and the write-only env vars (cpu_cores ships as the wire's
	// decimal string). By NAME, not index: tofu plans independent
	// instances concurrently, so the POSTs arrive in race order.
	fake.wantCreateAttrs(t, "smoke-a", "cpu_cores", "0.5", "memory_mb", "256", "storage_gb", "1")
	fake.wantCreateAttrs(t, "smoke-b", "cpu_cores", "1", "memory_mb", "512", "storage_gb", "2")
	fake.wantCreateAttrs(t, "smoke-c", "cpu_cores", "0.5", "memory_mb", "256", "storage_gb", "1")
	envVars := fattrs(t, fake.createNamed(t, "smoke-b"))["env_vars"]
	if m, ok := envVars.(map[string]any); !ok || m["SMOKE"] != "1" {
		t.Fatalf("instance b create env_vars = %#v, want SMOKE=1", envVars)
	}

	// The exposure PATCH carried the planned value — the private
	// instance's posture is a request body, not an assumption.
	fake.wantActionAttrs(t, "PATCH", "/exposure", "exposure", "private")

	// The always-on PATCH carried the headline posture — the example
	// stack's "always-on forge" claim, now exercised.
	fake.wantActionAttrs(t, "PATCH", "/always-on", "always_on", true)

	// The attach POST paired the private instance's actual id.
	fake.wantAttachInstance(t, "smoke-b")

	// The backup POST carried the composed R2 coordinates from the
	// module's config, write-only secret included (the request must
	// ride it even though the API never echoes it back).
	fake.wantBackupAttrs(t,
		"endpoint", "https://acct123.r2.cloudflarestorage.com",
		"bucket", "smoke-bucket",
		"prefix", "fluxvale/smoke",
		"access_key_id", "AKIAIOSFODNN7EXAMPLE",
		"secret_access_key", "smoke-secret",
	)

	// The credential rode as the documented bearer header.
	me := fake.nthReq(t, "GET", "/api/v1/me")
	if me.auth != "Bearer fvpat_e2e" {
		t.Fatalf("/me Authorization = %q, want Bearer fvpat_e2e", me.auth)
	}

	// The destroy sequence has an order, not just counts: each
	// instance's DELETE is followed by at least one GET on the same id
	// — the wait-gone poll proving the teardown finished.
	fake.wantDeleteThenPoll(t, "inst-1", "inst-2", "inst-3")

	// The version wiring, end to end: the binary's User-Agent carries
	// the const (internal/provider/version.go) — the release workflow's
	// -version proof and this are the same source.
	ua := me.userAgent
	if !strings.HasPrefix(ua, "terraform-provider-fluxvale/"+Version) {
		t.Fatalf("User-Agent = %q, want the terraform-provider-fluxvale/%s prefix", ua, Version)
	}

	// Phase 2: the documented mirror path (docs/install.md), for real.
	// The compiled binary zipped under the registry's inner name into
	// the PACKED layout — zip DIRECTLY in the type dir, no version
	// directory; then init + validate against the mirror alone.
	mirrorDir := t.TempDir()
	typeDir := filepath.Join(mirrorDir, "registry.terraform.io", "fluxvale", "fluxvale")
	if err := os.MkdirAll(typeDir, 0o755); err != nil {
		t.Fatalf("mirror type dir: %v", err)
	}
	zipPath := filepath.Join(typeDir, fmt.Sprintf(
		"terraform-provider-fluxvale_%s_%s_%s.zip", Version, runtime.GOOS, runtime.GOARCH))
	zipBinary(t, bin, zipPath, "terraform-provider-fluxvale_v"+Version)

	mirrorCfg := filepath.Join(mirrorDir, "tofurc")
	if err := os.WriteFile(mirrorCfg, []byte(fmt.Sprintf(`provider_installation {
  filesystem_mirror {
    path    = %q
    include = ["registry.terraform.io/fluxvale/fluxvale"]
  }
  direct {}
}
`, mirrorDir)), 0o600); err != nil {
		t.Fatalf("write mirror CLI config: %v", err)
	}

	mirrorWork := t.TempDir()
	mirrorModule := fmt.Sprintf(`
terraform {
  required_providers {
    fluxvale = {
      source = "registry.terraform.io/fluxvale/fluxvale"
    }
  }
}

provider "fluxvale" {
  api_url = %q
  token   = "fvpat_e2e"
}

data "fluxvale_apps" "catalog" {}
`, fake.URL)
	if err := os.WriteFile(filepath.Join(mirrorWork, "main.tf"), []byte(mirrorModule), 0o600); err != nil {
		t.Fatalf("write mirror module: %v", err)
	}

	runIn := func(dir, cliConfig string, args ...string) string {
		t.Helper()
		t.Logf("phase 2: tofu %s (dir %s)", strings.Join(args, " "), dir)
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, tofu, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "TF_CLI_CONFIG_FILE="+cliConfig, "TF_INPUT=0")
		out, err := cmd.CombinedOutput()
		t.Logf("phase 2: tofu %s done (err=%v)", strings.Join(args, " "), err)
		if err != nil {
			t.Fatalf("tofu %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	runIn(mirrorWork, mirrorCfg, "init", "-backend=false")
	runIn(mirrorWork, mirrorCfg, "validate")
	// init against the mirror must have written the lock (it resolved
	// the provider from the packed zip, not the registry).
	if _, err := os.Stat(filepath.Join(mirrorWork, ".terraform.lock.hcl")); err != nil {
		t.Fatalf("mirror init wrote no lock file — the provider did not resolve from the mirror: %v", err)
	}
}

// zipBinary writes the binary into a zip at innerName — the registry
// convention: flat at the archive root, `terraform-provider-fluxvale_v<version>`.
func zipBinary(t *testing.T, bin, zipPath, innerName string) {
	t.Helper()
	src, err := os.Open(bin)
	if err != nil {
		t.Fatalf("open binary: %v", err)
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		t.Fatalf("stat binary: %v", err)
	}
	dst, err := os.OpenFile(zipPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o755)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	defer dst.Close()
	w := zip.NewWriter(dst)
	// The entry must CARRY the executable bit: tofu unpacks preserving
	// the zip entry's mode, and a non-executable provider binary is a
	// fork/exec permission error at validate time.
	hdr, err := zip.FileInfoHeader(info)
	if err != nil {
		t.Fatalf("zip header: %v", err)
	}
	hdr.Name = innerName
	hdr.Method = zip.Deflate
	entry, err := w.CreateHeader(hdr)
	if err != nil {
		t.Fatalf("zip entry: %v", err)
	}
	if n, err := io.Copy(entry, src); err != nil || n != info.Size() {
		t.Fatalf("zip copy: %d/%d bytes: %v", n, info.Size(), err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
}

// --- the smoke fake ---------------------------------------------------
// A scripted FluxVale API in the same discipline as fake_test.go, but
// deliberately self-contained: this file must not share its fixtures or
// mutate them (fake_test.go carries in-flight changes on other slices).

type smokeFake struct {
	*httptest.Server

	mu   sync.Mutex
	reqs []smokeReq

	// per-script GET queues — instance ids (seeded per create) and the
	// org-gateway singleton; the last entry repeats, the poll loops'
	// steady state. Entries are LAZY: rendered at serve time, so the
	// instance docs echo the posture the PATCH verbs applied.
	gets map[string][]func() smokeResp

	// per-instance row state the PATCH verbs mutate, echoed on reads —
	// the real API converges the row, so reads must not contradict
	// what the PATCHes applied
	posture map[string]*smokePosture

	// instance creates, by config name — the fake's row-id contract
	// (the smoke module's names are stable)
	createIDs map[string]string
}

type smokePosture struct {
	exposure string
	alwaysOn bool
}

type smokeReq struct {
	method, path, auth, userAgent string
	body                          map[string]any
}

type smokeResp struct {
	status int
	body   string
}

type smokeCount struct {
	// how to match: exact path, prefix, suffix, or contains (verb
	// actions ride /instances/{id}/{action}, so suffix is how you
	// count them; /attachments/ URLs END with the instance id, so
	// contains is the only honest match there)
	mode, method, path string
	n                  int
}

func countExact(method, path string, n int) smokeCount {
	return smokeCount{"exact", method, path, n}
}

func countPrefix(method, path string, n int) smokeCount {
	return smokeCount{"prefix", method, path, n}
}

func countSuffix(method, path string, n int) smokeCount {
	return smokeCount{"suffix", method, path, n}
}

func countContains(method, path string, n int) smokeCount {
	return smokeCount{"contains", method, path, n}
}

func newSmokeFake(t *testing.T) *smokeFake {
	f := &smokeFake{
		gets: map[string][]func() smokeResp{
			// The org-gateway script: create lands :provisioning; the
			// attach waits the observer's promotion to :active, then the
			// final read + destroy refresh ride the repeating active.
			"org-gateway": {
				func() smokeResp { return r200(smokeGatewayDoc("provisioning")) },
				func() smokeResp { return r200(smokeGatewayDoc("active")) },
			},
			// The backups row: create POSTs it; destroy's refresh READS
			// it by instance (reads go by instance_id) before the DELETE.
			"inst-2/backup-setting": {
				func() smokeResp { return r200(smokeBackupDoc()) },
			},
		},
		// Instance GET scripts are seeded per create (see the POST
		// handler): apply's wait + final read, then destroy's refresh +
		// the polled 404 — the last entry repeats for safety.
		posture:   map[string]*smokePosture{},
		createIDs: map[string]string{},
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/api/v1/me", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, nil)
		f.json(w, http.StatusOK, `{"data": {"id": "user-1", "type": "user", "attributes": {"email": "owner@example.com"}}}`)
	})
	mux.HandleFunc("/api/v1/organizations", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, nil)
		f.json(w, http.StatusOK, `{"data": [{"id": "org-1", "type": "organization", "attributes": {"slug": "acme", "name": "Acme"}}]}`)
	})
	mux.HandleFunc("/api/v1/apps", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, nil)
		f.json(w, http.StatusOK, `{"data": [{"id": "app-1", "type": "app", "attributes": {"name": "Forgejo", "slug": "forgejo", "file_access": false}}]}`)
	})
	mux.HandleFunc("/api/v1/apps/", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, nil)
		if strings.HasSuffix(r.URL.Path, "/versions") {
			f.json(w, http.StatusOK, `{"data": [{"id": "ver-1", "type": "app_version", "attributes": {
				"app_id": "app-1", "version": "1.0", "image": "reg/app:1.0", "port": 3000,
				"healthcheck_path": "/", "storage_mount_path": "/data",
				"default_cpu_cores": "0.5", "default_memory_mb": 256, "default_storage_gb": 1,
				"default_env_vars": {}, "published_at": null}}]}`)
			return
		}
		f.json(w, http.StatusOK, `{"data": {"id": "app-1", "type": "app", "attributes": {"name": "Forgejo", "slug": "forgejo", "file_access": false}}}`)
	})

	// Instance create: the response ECHOES the request — the real API
	// returns the row it created, and tofu plans independent instances
	// concurrently, so a response queue would pop in race order (the
	// first run of this gate caught exactly that swap). The GET script
	// is seeded as LAZY docs — rendered at serve time so reads echo the
	// posture the PATCH verbs actually applied (a doc baked now would
	// contradict the apply's consistency check).
	mux.HandleFunc("POST /api/v1/instances", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.record(r, body)
		attrs, _ := body["data"].(map[string]any)["attributes"].(map[string]any)
		id := f.instanceID(t, fmt.Sprint(attrs["name"]))
		f.mu.Lock()
		f.gets[id] = []func() smokeResp{
			func() smokeResp { return r200(f.instanceDoc(id, "running")) },
			func() smokeResp { return r200(f.instanceDoc(id, "running")) },
			func() smokeResp { return r200(f.instanceDoc(id, "running")) },
			func() smokeResp { return smokeResp{http.StatusNotFound, `{"errors": [{"title": "Not Found"}]}`} },
		}
		f.posture[id] = &smokePosture{exposure: "public", alwaysOn: false}
		f.mu.Unlock()
		f.json(w, http.StatusCreated, f.createDoc(id, attrs))
	})

	// Instance detail: per-id script for GETs; the posture verbs
	// MUTATE the fake's row state (exposure/always-on) — reads echo
	// what was applied. The verb paths are /instances/{id}/{action} —
	// strip the ACTION or the row key is wrong (the hardened fake's
	// body assertions caught exactly that). The posture/deploy/stop
	// PATCH verbs answer a full parseable doc (the create path
	// discards it, the client still parses).
	mux.HandleFunc("/api/v1/instances/", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		f.record(r, body)
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/instances/")
		parts := strings.SplitN(rest, "/", 2)
		id := parts[0]
		action := ""
		if len(parts) == 2 {
			action = parts[1]
		}
		switch r.Method {
		case http.MethodGet:
			f.scripted(w, rest)
		case http.MethodDelete:
			f.json(w, http.StatusOK, f.instanceDoc(id, "pending"))
		case http.MethodPatch:
			if attrs := fattrsOf(body); attrs != nil && action != "" {
				f.mu.Lock()
				if p := f.posture[id]; p != nil {
					if v, ok := attrs["exposure"].(string); ok {
						p.exposure = v
					}
					if v, ok := attrs["always_on"].(bool); ok {
						p.alwaysOn = v
					}
				}
				f.mu.Unlock()
			}
			f.json(w, http.StatusOK, f.instanceDoc(id, "running"))
		default:
			f.json(w, http.StatusMethodNotAllowed, `{"errors": [{"title": "Method Not Allowed"}]}`)
		}
	})

	// Backups: the routed settings family — create + delete cover the
	// smoke's path (reads are update/import-only).
	mux.HandleFunc("POST /api/v1/instances/{id}/backup-setting", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.record(r, body)
		f.json(w, http.StatusCreated, smokeBackupDoc())
	})
	mux.HandleFunc("/api/v1/backup-settings/", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, nil)
		if r.Method == http.MethodDelete {
			f.json(w, http.StatusOK, `{"meta": {"message": "deleted"}}`)
			return
		}
		f.json(w, http.StatusMethodNotAllowed, `{"errors": [{"title": "Method Not Allowed"}]}`)
	})

	// Gateway: singleton read (scripted), create, pairing verbs.
	mux.HandleFunc("GET /api/v1/organizations/{org}/access-gateway", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, nil)
		f.scripted(w, "org-gateway")
	})
	mux.HandleFunc("POST /api/v1/access-gateways", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.record(r, body)
		f.json(w, http.StatusCreated, smokeGatewayDoc("provisioning"))
	})
	mux.HandleFunc("POST /api/v1/access-gateways/{id}/attachments", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.record(r, body)
		instanceID, _ := body["data"].(map[string]any)["instance_id"].(string)
		f.json(w, http.StatusCreated, fmt.Sprintf(`{"gateway_id": "gw-1", "instance_id": %q, "route_key": "8443"}`, instanceID))
	})
	mux.HandleFunc("DELETE /api/v1/access-gateways/{id}/attachments/{iid}", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, nil)
		f.json(w, http.StatusOK, `{"success": true}`)
	})

	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func r200(body string) smokeResp { return smokeResp{http.StatusOK, body} }

func (f *smokeFake) record(r *http.Request, body map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, smokeReq{
		method: r.Method, path: r.URL.Path,
		auth: r.Header.Get("Authorization"), userAgent: r.Header.Get("User-Agent"),
		body: body,
	})
}

// scripted pops the script's next response; the last entry repeats so
// poll loops and refresh reads hit a steady state. The lock is
// RELEASED before the entry renders — a lazy instance doc reads the
// posture map, and non-reentrant locking would wedge the fake (the
// self-deadlock this test's own first hardened run caught).
func (f *smokeFake) scripted(w http.ResponseWriter, id string) {
	f.mu.Lock()
	script := f.gets[id]
	if len(script) == 0 {
		f.mu.Unlock()
		f.json(w, http.StatusInternalServerError, fmt.Sprintf(`{"errors": [{"title": "no GET script for %s"}]}`, id))
		return
	}
	respFn := script[0]
	if len(script) > 1 {
		f.gets[id] = script[1:]
	}
	f.mu.Unlock()
	resp := respFn()
	f.json(w, resp.status, resp.body)
}

func (f *smokeFake) json(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/vnd.api+json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func (f *smokeFake) all() []smokeReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]smokeReq(nil), f.reqs...)
}

// instanceID maps a create body's name to the fake's row id — stable
// per instance, so the per-id GET scripts match.
func (f *smokeFake) instanceID(t *testing.T, name string) string {
	t.Helper()
	ids := map[string]string{
		"smoke-a": "inst-1",
		"smoke-b": "inst-2",
		"smoke-c": "inst-3",
	}
	id, ok := ids[name]
	if !ok {
		t.Fatalf("unexpected instance name %q — the smoke module's names are the fake's contract", name)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createIDs[name] = id
	return id
}

// idOf returns the row id the fake assigned to a create by name.
func (f *smokeFake) idOf(t *testing.T, name string) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.createIDs[name]
	if !ok {
		t.Fatalf("no recorded create id for %q", name)
	}
	return id
}

// wantCounts asserts every count spec; a miss dumps the full traffic
// list (triage data, not a bare number).
func (f *smokeFake) wantCounts(t *testing.T, want ...smokeCount) {
	t.Helper()
	for _, w := range want {
		got := 0
		for _, r := range f.all() {
			if r.method != w.method {
				continue
			}
			switch w.mode {
			case "exact":
				if r.path == w.path {
					got++
				}
			case "prefix":
				if strings.HasPrefix(r.path, w.path) {
					got++
				}
			case "suffix":
				if strings.HasSuffix(r.path, w.path) {
					got++
				}
			case "contains":
				if strings.Contains(r.path, w.path) {
					got++
				}
			}
		}
		if got != w.n {
			t.Errorf("%s ~%q (%s): %d calls, want %d\nfull traffic:\n%s", w.method, w.path, w.mode, got, w.n, f.trafficDump())
		}
	}
}

func (f *smokeFake) trafficDump() string {
	var b strings.Builder
	for _, r := range f.all() {
		fmt.Fprintf(&b, "%s %s\n", r.method, r.path)
	}
	return b.String()
}

// createNamed returns the instance create whose body carries the
// name — order-independent (independent creates apply concurrently).
func (f *smokeFake) createNamed(t *testing.T, name string) smokeReq {
	t.Helper()
	for _, r := range f.all() {
		if r.method != "POST" || r.path != "/api/v1/instances" {
			continue
		}
		if attrs, ok := fattrsOf(r.body)["name"].(string); ok && attrs == name {
			return r
		}
	}
	t.Fatalf("no instance create named %q among the recorded traffic", name)
	return smokeReq{}
}

func (f *smokeFake) nthReq(t *testing.T, method, path string, nth ...int) smokeReq {
	t.Helper()
	want := 0
	if len(nth) > 0 {
		want = nth[0]
	}
	seen := 0
	for _, r := range f.all() {
		if r.method == method && r.path == path {
			if seen == want {
				return r
			}
			seen++
		}
	}
	t.Fatalf("no %s %s #%d among the recorded traffic", method, path, want)
	return smokeReq{}
}

// fattrs digs the attributes object out of a recorded JSON:API body.
func fattrs(t *testing.T, r smokeReq) map[string]any {
	t.Helper()
	data, ok := r.body["data"].(map[string]any)
	if !ok {
		t.Fatalf("%s %s: body has no data object: %#v", r.method, r.path, r.body)
	}
	attrs, _ := data["attributes"].(map[string]any)
	return attrs
}

// fattrsOf is the nil-tolerant variant for handler-internal use.
func fattrsOf(body map[string]any) map[string]any {
	data, _ := body["data"].(map[string]any)
	attrs, _ := data["attributes"].(map[string]any)
	return attrs
}

// wantCreateAttrs asserts attribute values on the named instance's
// create body, string-compared through fmt.Sprint (numbers and decimal
// strings print the same way for the fixtures used here).
func (f *smokeFake) wantCreateAttrs(t *testing.T, name string, pairs ...string) {
	t.Helper()
	attrs := fattrs(t, f.createNamed(t, name))
	for i := 0; i+1 < len(pairs); i += 2 {
		key, want := pairs[i], pairs[i+1]
		if got := fmt.Sprint(attrs[key]); got != want {
			t.Errorf("create %q %s = %#v, want %s", name, key, attrs[key], want)
		}
	}
}

// wantActionAttrs asserts the attributes a verb PATCH carried (the
// action rides the path suffix: /instances/{id}/{action}).
func (f *smokeFake) wantActionAttrs(t *testing.T, method, action string, pairs ...any) {
	t.Helper()
	for _, r := range f.all() {
		if r.method != method || !strings.HasSuffix(r.path, action) {
			continue
		}
		attrs := fattrs(t, r)
		for i := 0; i+1 < len(pairs); i += 2 {
			key, want := pairs[i].(string), pairs[i+1]
			if got := attrs[key]; fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("%s %s %s = %#v, want %v", method, action, key, got, want)
			}
		}
		return
	}
	t.Fatalf("no %s request for action %q among the recorded traffic", method, action)
}

// wantAttachInstance asserts the gateway's pairing POST named the
// named instance's actual row id.
func (f *smokeFake) wantAttachInstance(t *testing.T, name string) {
	t.Helper()
	want := f.idOf(t, name)
	for _, r := range f.all() {
		if r.method != "POST" || !strings.Contains(r.path, "/attachments") {
			continue
		}
		data, _ := r.body["data"].(map[string]any)
		if data["instance_id"] != want {
			t.Errorf("attach POST instance_id = %#v, want %q (instance %s's row id)", data["instance_id"], want, name)
		}
		return
	}
	t.Fatalf("no attach POST among the recorded traffic")
}

// wantBackupAttrs asserts the backup save carried the composed
// coordinates (and the write-only secret the API will never echo).
func (f *smokeFake) wantBackupAttrs(t *testing.T, pairs ...string) {
	t.Helper()
	var found bool
	for _, r := range f.all() {
		if r.method != "POST" || !strings.HasSuffix(r.path, "/backup-setting") {
			continue
		}
		found = true
		attrs := fattrs(t, r)
		for i := 0; i+1 < len(pairs); i += 2 {
			key, want := pairs[i], pairs[i+1]
			if got := fmt.Sprint(attrs[key]); got != want {
				t.Errorf("backup save %s = %#v, want %s", key, attrs[key], want)
			}
		}
	}
	if !found {
		t.Fatalf("no backup-setting POST among the recorded traffic")
	}
}

// wantDeleteThenPoll asserts the destroy sequence's ORDER: each
// instance's DELETE is followed by at least one GET on the same id —
// the wait-gone poll, i.e. destroy waits for the teardown it asked
// for.
func (f *smokeFake) wantDeleteThenPoll(t *testing.T, ids ...string) {
	t.Helper()
	reqs := f.all()
	for _, id := range ids {
		deleteIdx := -1
		for i, r := range reqs {
			if r.method == "DELETE" && r.path == "/api/v1/instances/"+id {
				deleteIdx = i
			}
		}
		if deleteIdx == -1 {
			t.Errorf("no DELETE for %s among the recorded traffic", id)
			continue
		}
		polled := false
		for _, r := range reqs[deleteIdx+1:] {
			if r.method == "GET" && r.path == "/api/v1/instances/"+id {
				polled = true
				break
			}
		}
		if !polled {
			t.Errorf("instance %s: no GET after its DELETE — the wait-gone poll is missing from the destroy sequence", id)
		}
	}
}

// instanceDoc renders the detail surface the client parses — INCLUDING
// the FK ids the real API echoes on reads via the sparse fieldset
// (organization_id, app_version_id): they fill the computed
// app_version_id, whose plan value is unknown for the slug form. A doc
// without them is an API the provider legitimately fails on.
//
// Name and allocation echo per id, and exposure/always_on come from
// the fake's tracked row state (the real API echoes what the PATCHes
// applied; tofu's apply consistency check holds state to the plan — a
// contradicting doc fails it, which is exactly the drift this gate is
// built to surface).
func (f *smokeFake) instanceDoc(id, status string) string {
	name, cpu, mem, storage := "smoke-a", "0.5", 256, 1
	if id == "inst-2" {
		name, cpu, mem, storage = "smoke-b", "1", 512, 2
	}
	if id == "inst-3" {
		// same size as a, but its own row (the always-on instance)
		name = "smoke-c"
	}
	exposure, alwaysOn := "public", false
	f.mu.Lock()
	if p := f.posture[id]; p != nil {
		exposure, alwaysOn = p.exposure, p.alwaysOn
	}
	f.mu.Unlock()
	return fmt.Sprintf(`{"data": {"id": %q, "type": "instance", "attributes": {
		"name": %q, "slug": "velvety-otter-%s", "namespace": null,
		"status": %q, "exposure": %q, "always_on": %t,
		"image": "reg/app:1.0", "port": 3000, "healthcheck_path": "/",
		"storage_mount_path": "/data", "file_access": false,
		"cpu_cores": %q, "memory_mb": %d, "storage_gb": %d, "deployed_at": null,
		"organization_id": "org-1", "app_version_id": "ver-1"}}}`,
		id, name, strings.TrimPrefix(id, "inst-"), status, exposure, alwaysOn, cpu, mem, storage)
}

// createDoc echoes a create request's attributes back as the pending
// row (the real API's create response): the client maps it into state
// immediately, then the final read refines it.
func (f *smokeFake) createDoc(id string, attrs map[string]any) string {
	envVars := "null"
	if m, ok := attrs["env_vars"].(map[string]any); ok && len(m) > 0 {
		b, _ := json.Marshal(m)
		envVars = string(b)
	}
	return fmt.Sprintf(`{"data": {"id": %q, "type": "instance", "attributes": {
		"name": %q, "slug": "velvety-otter-%s", "namespace": null,
		"status": "pending", "exposure": "public", "always_on": false,
		"image": "reg/app:1.0", "port": 3000, "healthcheck_path": "/",
		"storage_mount_path": "/data", "file_access": false,
		"cpu_cores": %#v, "memory_mb": %#v, "storage_gb": %#v, "deployed_at": null,
		"organization_id": "org-1", "app_version_id": %q, "env_vars": %s}}}`,
		id, attrs["name"], strings.TrimPrefix(id, "inst-"),
		attrs["cpu_cores"], attrs["memory_mb"], attrs["storage_gb"],
		attrs["app_version_id"], envVars)
}

func smokeBackupDoc() string {
	return `{"data": {"id": "bs-1", "type": "backup_setting", "attributes": {
		"instance_id": "inst-2", "endpoint": "https://acct123.r2.cloudflarestorage.com",
		"bucket": "smoke-bucket", "prefix": "fluxvale/smoke",
		"access_key_id": "AKIAIOSFODNN7EXAMPLE", "retention_keep": 7, "enabled": true}}}`
}

func smokeGatewayDoc(state string) string {
	doc := fmt.Sprintf(`{"data": {"id": "gw-1", "type": "access_gateway", "attributes": {
		"organization_id": "org-1", "hostname": "fv-0a1b2c3d", "tailnet_domain": "smoke.ts.net",
		"provider": "tailscale", "state": %q, "status_message": null,
		"monthly_cents": 500`, state)
	if state == "active" {
		doc += `, "provisioned_at": "2026-11-07T10:00:00Z"`
	}
	return doc + `}}}`
}
