---
page_title: "fluxvale_instance"
---

# fluxvale_instance

Deploys a catalog app version.

The name, the version identity, and the organization are create-only (the API has no rename/version-change/org-change routes), so changing them replaces the instance. The allocation (`cpu_cores`/`memory_mb`/`storage_gb`) resizes in place: the API's resize is stopped-first — the provider sequences stop → resize → start (CPU/RAM both ways; storage is up-only, a shrink is the server's 400). `running`/`always_on`/`exposure` update in place via their PATCH verbs.

## Example Usage

```hcl
data "fluxvale_app_versions" "memos" {
  app = "memos"
}

resource "fluxvale_instance" "memos" {
  name         = "Memos"
  app          = "memos"
  app_version  = data.fluxvale_app_versions.memos.app_versions[0].version
  organization = "my-org" # optional: id or slug; falls back to the provider block, then your sole org
  cpu_cores    = 1
  memory_mb    = 512
  storage_gb   = 10
  running      = true
  exposure     = "private" # behind the access gateway
}
```

## Argument Reference

- `name` (Required) — the instance's display name. Create-only: changing it replaces the instance.
- `app` / `app_version` (Optional) — the catalog app slug and the version string; the slug form of `app_version_id`. Create-only. Either pin the exact version or resolve it through the [`fluxvale_app_versions`](../data-sources/app_versions.md) data source — the catalog order is semver-newest-first, but pin deliberately: a pin survives re-ordering and new releases.
- `app_version_id` (Optional) — the id form; computed when the slug pair is set.
- `cpu_cores` (Required, Float64) / `memory_mb` (Required) / `storage_gb` (Required) — the allocation. Resizes in place (storage up-only).
- `env_vars` (Optional, map, write-only) — the API never echoes it, so plan can't see server-side drift by design, and the configured value is kept verbatim in state.
- `running` (Optional, bool) / `always_on` (Optional, bool) / `exposure` (Optional) — update in place.
- `organization` (Optional) — id or slug; falls back to the provider-level `organization`, then to your sole organization (several memberships without a name is an error, never a silent pick).

## Attributes Read

`id`, `slug`, `namespace`, `status`, `port`, `image`, `deployed_at`, `organization_id`, `app_version_id` — the wire's echo, computed.

## Import

`terraform import fluxvale_instance.x <id>`. Everything the API echoes converges — including `organization_id` and `app_version_id` (requested via a sparse fieldset on the read). Only `app`/`app_version` (the slug pair) and `env_vars` have no wire echo — keep your config matching what you deployed, or plan shows a replacement.
