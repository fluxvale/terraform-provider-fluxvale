# FluxVale Terraform/OpenTofu provider

A thin skin over the FluxVale JSON:API (`/api/v1`, PAT auth) —
[ADR-0019](../../docs/adr/00019-machine-first-api-cli-mcp.md) Am. 4.
Policy, paywall, and scopes enforce server-side; the provider
re-implements nothing.

**Status**: P4–P5 of #482 — the instance resource, the catalog data
sources, and the instance family's backups + access-gateway resources,
on the scaffold (#482 P3). `fluxvale_custom_domain` lands in a
follow-up slice.

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
vet + test + build, Go version parsed from the root `mise.toml` pin.
