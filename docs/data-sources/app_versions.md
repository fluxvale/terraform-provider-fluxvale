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

Version pins on `fluxvale_instance` are deliberate: the catalog's row order is a version-string sort, not a release order, so no "first row" shortcut exists — resolve the version you mean (like the `[0]` above, newest first) or pin the exact string.
