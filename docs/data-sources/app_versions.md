# fluxvale_app_versions (data source)

One app's versions, by `app` (slug) or `app_id`.

## Example Usage

```hcl
data "fluxvale_app_versions" "kavita" {
  app = "kavita"
}

resource "fluxvale_instance" "kavita" {
  app         = "kavita"
  app_version = data.fluxvale_app_versions.kavita.app_versions[0].version
}
```

## Attributes

The list `app_versions` is ordered newest first; each element carries `id` (the `app_version_id` `fluxvale_instance` accepts), `app_id`, and `version`.

## Notes

Version pins on `fluxvale_instance` are deliberate: the catalog's row order is a version-string sort, not a release order, so no "first row" shortcut exists — resolve the version you mean (like the `latest_version` above) or pin the exact slug.
