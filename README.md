# FluxVale Terraform/OpenTofu provider

A thin skin over the FluxVale JSON:API (`/api/v1`, PAT auth) —
[ADR-0019](../../docs/adr/00019-machine-first-api-cli-mcp.md) Am. 4.
Policy, paywall, and scopes enforce server-side; the provider
re-implements nothing.

**Status**: P4–P6 of #482 — the instance resource, the catalog data
sources, and the instance family's backups + access-gateway +
custom-domain resources, on the scaffold (#482 P3).

## Resources and data sources

- `fluxvale_instance` — deploys a catalog app version. The name, the
  version identity, and the organization are create-only (the API has
  no rename/version-change/org-change routes), so changing them
  replaces the instance. The allocation (`cpu_cores`/`memory_mb`/
  `storage_gb`) resizes in place: the API's resize is stopped-first —
  the provider sequences stop → resize → start (CPU/RAM both ways;
  storage is up-only, a shrink is the server's 400). `running`/
  `always_on`/`exposure` update in place via their PATCH verbs.
  - `env_vars` is write-only: the API never echoes it, so plan can't
    see server-side drift by design, and the configured value is kept
    verbatim in state.
  - `organization` (id or slug) falls back to the provider-level
    `organization`, then to your sole organization — several
    memberships without a name is an error, never a silent pick. The
    resolved `organization_id` (and `app_version_id`) are computed and
    ride the wire.
  - Import: `terraform import fluxvale_instance.x <id>`. Everything the
    API echoes converges — including `organization_id` and
    `app_version_id` (requested via a sparse fieldset on the read).
    Only `app`/`app_version` (the slug pair) and `env_vars` have no
    wire echo — keep your config matching what you deployed, or plan
    shows a replacement.
- `fluxvale_instance_backups` — an instance's backup destination (R2
  first). A routed save converges the cluster server-side (the
  credential Secret, the restic repo URL, the quota margin) — with one
  deliberate platform exception: a save riding `enabled: true→false`
  (the pause) writes the row only, so a coordinates/secret change
  boarding a paused row heals on the next save or re-enable. Errors
  surface verbatim: 400s (attribute/guard refusals), 403s (scope/
  policy), 404s.
  - `secret_access_key` is write-only: set ⇒ applied (or rotated) with
    the save; unset ⇒ the stored credential is kept. A FIRST create
    without it lands quota-only (the API's permissive shape) and
    backups won't run until a later save supplies it. Kept verbatim in
    state — the API never echoes it.
  - Updates PATCH by settings-row id; reads (and imports) go by
    `instance_id` — the row id refreshes if the row is ever re-created
    out-of-band.
  - Import: `terraform import fluxvale_instance_backups.x
    <instance_id>`. `secret_access_key` stays null after import; your
    config's value (or its absence) is the rotation decision on the
    next apply.
- `fluxvale_access_gateway` — the org's singleton private-access
  gateway. The API's gateway surface is create, the singleton read,
  the attach/detach verbs, and `PATCH /access-gateways/:id/resume`;
  the provider deliberately does not expose resume — a suspended
  gateway is the zero-balance ladder's out-of-band platform state
  (resume via the panel or MCP, watch `state`), not something to
  flip from a config file.
  - `destroy` removes the resource from state only: the API exposes NO
    gateway-delete route, so the platform keeps the gateway running
    (and billing). The destroy warning states this; and since the
    identity attributes are create-only, a replacement would apply
    into a guaranteed conflict — replacements are rejected at plan
    time. To actually remove a gateway, opt out via the panel's
    Access tab, then `terraform apply -replace=…` (or re-add).
  - `instances` (a set of instance ids) converges via the attach/detach
    verbs — one call per added/removed id. Attaching waits the gateway
    to `active` (create lands `provisioning`; the observer promotes
    within about a minute). The set is config-authoritative: the API
    has no attachment list, so out-of-band panel attach/detach is
    invisible to the provider.
  - A `suspended` gateway (the zero-balance ladder's out-of-band state)
    is deliberately unmanaged — resume via the panel or MCP
    (`resume_gateway`); watch `state`/`status_message`.
  - Import: `terraform import fluxvale_access_gateway.x
    <org_id_or_slug>`. The read fills the wire mirror and seeds
    `organization` from the resolution, so a config that leaves it
    unset converges. `auth_key` is Optional (required only when this
    resource ENABLES a gateway — the wire never echoes it, so imports
    manage attachments with it null) and `instances` stays null after
    import; a config value that doesn't match what's stored plans a
    replacement, and replacements are rejected at plan time.
- `fluxvale_custom_domain` — your own domain on the private-access
  edge (`apps.example.com`). Register lands the row `pending` and
  mints the wildcard Certificate in the same transaction; verification
  is issuance — the platform's observer pass (about a 15-minute
  cadence) promotes `pending → verified` once your DNS delegation
  answers the DNS-01 challenge, and fails the row past its issuance
  window (~72h). Default create returns at `pending`; refreshes show
  the progression via `state`. Set `wait_for_verification = true` to
  poll instead — bounded by `timeouts.create` (default 45m). A timeout
  leaves the row tainted but tracked (it keeps verifying server-side):
  `tofu untaint` — or `terraform untaint` — keeps the registration,
  and the following refresh shows the promoted state; without
  untainting, the next apply destroys and re-registers. `failed` fails
  the create fast (it's terminal: remove and re-register).
  - **`verified` is NOT resolution.** Promotion mints the domain's
    serving arms, but the split-DNS claim — the tailnet-side route
    that makes the name ANSWER — has no API surface yet; claiming is a
    separate platform capability on a later slice. This resource
    registers, verifies (DNS-01), and tears down, nothing more.
  - `resume` is deliberately not exposed. A `suspended` domain is
    legacy state: domains are free (ADR-0040 Am. 2), nothing current
    suspends one — the state and its resume survive for old rows only.
    The raw `PATCH /api/v1/access-domains/:id/resume` is the only
    resume surface (no domains panel, no MCP tool). Watch `state`.
  - Names are unique platform-wide, across ALL organizations: the
    duplicate 400 surfaces verbatim (the name may belong to another
    org). The platform canonicalizes (trim + downcase); the provider
    sends the canonical name on the wire and decides replacement on
    canonical forms, so any config casing converges — the plan and
    state keep the form you wrote. Only a genuinely different
    canonical name replaces (hand-edited state included — the
    canonical row is the truth).
  - `organization` accepts id or slug; the plan normalizes a set value
    to the resolved id, so a slug config converges with an imported
    id. Only a genuinely different organization replaces.
  - Destroy is the platform's full teardown behind one synchronous
    DELETE (hostname pre-check → tailnet unclaim → row destroy →
    CoreDNS re-render → Certificate safe-delete); a 404 is
    already-gone. A domain still serving instance hostnames is
    refused (the platform's 400): remove those first.
  - Import: `terraform import fluxvale_custom_domain.x <id>`. The read
    goes by id (no org resolution — a multi-org token imports fine,
    and a revoked membership still refreshes) and fills the wire
    mirror: `domain` (the canonical name), `kind` (`custom` for
    provider-created rows; an imported id may be a system-minted
    `platform` row), `state`, `organization_id` — and seeds
    `organization` with the wire's id, so a config that leaves it
    unset (or names it by slug) converges.
- `fluxvale_apps` — the catalog. `fluxvale_app_versions` — one app's
  versions, by `app` (slug) or `app_id`.

The provider is a skin: business rules (paywall, scopes, allocation
bounds) enforce server-side — its 400s (attribute/guard refusals),
403s (scope/policy), and 404s surface verbatim, never pre-empted
client-side.

## Authentication

The provider needs a FluxVale personal access token (`fvpat_…`):

- `token` attribute in the provider block, or
- the `FLUXVALE_PAT` environment variable (preferred — no secrets in
  code; placeholders only in `.tf` files).

Configure pings `/api/v1/me` (scopeless — any live token works), so a
dead token fails at plan time with the server's wording.

## Installing

Manual-first (ADR-0019 Am. 4): release zips into a local filesystem
mirror — the copy-paste recipe lives in
[docs/install.md](docs/install.md).

## Dev loop (dev_overrides)

The registry isn't live yet; use [dev_overrides](https://developer.hashicorp.com/terraform/plugin/debug-vendor):

```sh
cd apps/tfprovider && go build -o ~/.tfplugins/terraform-provider-fluxvale .
```

`~/.terraformrc` (OpenTofu: `~/.tofurc`):

```hcl
provider_installation {
  dev_overrides {
    "registry.terraform.io/fluxvale/fluxvale" = "/home/you/.tfplugins"
  }
  direct {}
}
```

Then point a config at the provider — no `terraform init` needed:

```hcl
terraform {
  required_providers {
    fluxvale = { source = "registry.terraform.io/fluxvale/fluxvale" }
  }
}

provider "fluxvale" {
  # api_url = "https://staging.fluxvale.com"  # staging override
}
```

Or run it debugger-attached: `go run . -debug` and follow the printed
`TF_REATTACH_PROVIDERS` export.

## CI

Path-gated lane in `.github/workflows/ci.yml` (`tfprovider`): gofmt +
vet + test + build (Go parsed from the root `mise.toml` pin), the
release-tag gate's fixture test, and the **real-tofu smoke** — a real
OpenTofu CLI driving the real compiled binary (plan → apply → destroy
against a fake API, `TFPROVIDER_E2E=1`; skipped without it). It catches
what in-process tests cannot: it was this gate that caught the
reserved-`provider` root attribute and the plan-time-unknown
organization seed. Releases: tag `tf-v*` →
`.github/workflows/tfprovider-release.yml` (registry-shaped zips,
checksums, GitHub Release; dispatch is the dry run). Acceptance
against a real org: nightly/dispatch/tag lane in
`.github/workflows/tfprovider-acceptance.yml` — never per-PR.
