# fluxvale_instance

Deploys a catalog app version.

The name, the version identity, and the organization are create-only (the API has no rename/version-change/org-change routes), so changing them replaces the instance. The allocation (`cpu_cores`/`memory_mb`/`storage_gb`) resizes in place: the API's resize is stopped-first — the provider sequences stop → resize → start (CPU/RAM both ways; storage is up-only, a shrink is the server's 400). `running`/`always_on`/`exposure` update in place via their PATCH verbs.

## Example Usage

```hcl
resource "fluxvale_instance" "memos" {
  app         = "memos"
  app_version = "0.27.0"
  organization = "my-org"          # optional: id or slug; falls back to the provider block, then your sole org
  cpu_cores   = 1
  memory_mb   = 512
  storage_gb  = 10
  running     = true
  exposure    = "private"          # behind the access gateway
}
```

## Notes

- `env_vars` is write-only: the API never echoes it, so plan can't see server-side drift by design, and the configured value is kept verbatim in state.
- `organization` (id or slug) falls back to the provider-level `organization`, then to your sole organization — several memberships without a name is an error, never a silent pick. The resolved `organization_id` (and `app_version_id`) are computed and ride the wire.
- The `app`/`app_version` slugs you pin are preconditioned against the catalog at plan — bumping the version replaces the instance.

## Import

`terraform import fluxvale_instance.x <id>`. Everything the API echoes converges — including `organization_id` and `app_version_id` (requested via a sparse fieldset on the read). Only `app`/`app_version` (the slug pair) and `env_vars` have no wire echo — keep your config matching what you deployed, or plan shows a replacement.
