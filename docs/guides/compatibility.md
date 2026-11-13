---
page_title: "Compatibility contract"
---

# Compatibility contract

STABLE from day one — renaming any of these is a breaking change, never
done silently in 0.x:

- Provider config attribute names: `api_url`, `token`, `organization`
- The `FLUXVALE_PAT` environment variable
- The documented `api_url` default: `https://fluxvale.com`
- Resource/data-source **type names** — the v0 set:
  - `fluxvale_instance`
  - `fluxvale_instance_backups`
  - `fluxvale_access_gateway`
  - `fluxvale_custom_domain`
  - `fluxvale_pocket_id_sso`
  - `fluxvale_apps` (data source — the catalog)
  - `fluxvale_app_versions` (data source)
  - `fluxvale_instance` (data source — read by id or slug)
- Attribute names that mirror the `/api/v1` field names they map

ALLOWED TO MOVE while 0.x: computed/status fields, new schema
attributes, defaults, and anything not listed above. Dogfooding-first —
the owner is user #1.

## `env_vars` (from 0.3.0)

`env_vars` updates in place from 0.3.0; it is write-only on the wire —
never echoed — but a normal Terraform attribute, not a Terraform
write-only attribute. Plaintext values still live in state.

## `secret_env_vars` (additive, #764)

`secret_env_vars`, `secret_env_vars_version`, and
`secret_env_vars_keys` are additive. Existing configurations need no
change, and the provider's existing Terraform floor is unchanged.

The new attributes require **Terraform 1.12+ or OpenTofu 1.13+**,
because they use the protocol's write-only input support, and a provider
release containing #764 (0.4.0 or later, pending release). On an older CLI, leave the three attributes out;
`env_vars` plus state encryption at rest keeps working exactly as
before. The `enc:v1:` marker is an accepted design, not a shipped
fallback.

`secret_env_vars` values never enter state or plan files. The version
integer and the computed, sorted key list do — they are not secrets.
Bump the version on every value or key-set change (add, remove, or a
move between the two maps); a key-set change with an unchanged version
is refused. A key present in both `env_vars` and `secret_env_vars` is a
plan-time error. Import leaves the key list null (the API cannot
classify existing env keys); the first apply that sets
`secret_env_vars` records it, and keys never set that way stay
untracked.

The platform's rolling restart needs room for a second pod. On an app
whose quota allows only one, stop it (`running = false`) before the
bump, apply, then start it again; otherwise the rollout can mark the
instance `error`.

**1.0 freeze**: when the first non-owner user writes `.tf` against this
provider. From then on, removals and renames are versioned decisions.
