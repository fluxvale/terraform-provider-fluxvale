---
page_title: "fluxvale_app_versions"
---

# fluxvale_app_versions (data source)

One app's versions, by `app` (slug) or `app_id`.

## Example Usage

```hcl
data "fluxvale_app_versions" "kavita" {
  app = "kavita"
}

resource "fluxvale_instance" "kavita" {
  name        = "Kavita"
  app         = "kavita"
  app_version = data.fluxvale_app_versions.kavita.app_versions[0].version
  cpu_cores   = 1
  memory_mb   = 512
  storage_gb  = 10
}
```

## Argument Reference

- `app` (Optional) — the app's slug. Exactly one of `app` / `app_id`.
- `app_id` (Optional) — the app's id.

## Attributes

The list `app_versions` is ordered newest first; each element carries `id` (the `app_version_id` `fluxvale_instance` accepts), `app_id`, `version`, `image`, `port`, `healthcheck_path`, `storage_mount_path`, `default_cpu_cores`, `default_memory_mb`, `default_storage_gb`, `default_env_vars`, `release_notes`, `published_at`.

## Notes

Version pins on `fluxvale_instance` are deliberate even though the list is semver-newest-first (a pre-release sorts below its release): a pin survives re-ordering and new releases. `app_versions[0]` is the newest release when that is what you mean.
