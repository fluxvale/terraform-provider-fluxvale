---
page_title: "fluxvale_apps"
---

# fluxvale_apps (data source)

The catalog: every app FluxVale can deploy.

## Example Usage

```hcl
data "fluxvale_apps" "all" {}

output "app_slugs" {
  value = [for app in data.fluxvale_apps.all.apps : app.slug]
}
```

## Argument Reference

None — the data source reads the whole catalog.

## Attributes

Each element of `apps` carries: `id`, `name`, `slug`, `tagline`, `description`, `icon_url`, `source_url`, `docs_url`, `file_access`. Versions live in the [`fluxvale_app_versions`](app_versions.md) data source.
