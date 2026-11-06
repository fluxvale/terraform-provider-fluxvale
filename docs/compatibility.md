# Compatibility contract

STABLE from day one — renaming any of these is a breaking change, never
done silently in 0.x:

- Provider config attribute names: `api_url`, `token`, `organization`
- The `FLUXVALE_PAT` environment variable
- The documented `api_url` default: `https://fluxvale.com`
- Resource/data-source **type names** — the v0 set, landing in
  follow-up slices (#482):
  - `fluxvale_instance`
  - `fluxvale_instance_backups`
  - `fluxvale_access_gateway`
  - `fluxvale_custom_domain`
  - `fluxvale_apps` (data source — the catalog)
  - `fluxvale_app_versions` (data source)
- Attribute names that mirror the `/api/v1` field names they map

ALLOWED TO MOVE while 0.x: computed/status fields, new schema
attributes, defaults, and anything not listed above. Dogfooding-first —
the owner is user #1.

**1.0 freeze**: when the first non-owner user writes `.tf` against this
provider. From then on, removals and renames are versioned decisions.
