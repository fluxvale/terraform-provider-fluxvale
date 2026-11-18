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
  - `fluxvale_organization`
  - `fluxvale_instance`
  - `fluxvale_instance_backups`
  - `fluxvale_access_gateway`
  - `fluxvale_custom_domain`
  - `fluxvale_pocket_id_sso`
  - `fluxvale_apps` (data source — the catalog)
  - `fluxvale_app_versions` (data source)
  - `fluxvale_instance` (data source — read by id or slug)
  - `fluxvale_domain_delegation` (data source — the pre-registration DNS records)
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
because they use the protocol's write-only input support, and provider
0.4.0 or later. On an older CLI, leave the three attributes out;
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

## Gateway API token (additive)

`fluxvale_access_gateway` gains `api_token` (write-only, sensitive),
`rotate_api_token_on` (the rotation trigger), `token_set_at` and
`claimed_domains` (computed). Existing configurations need no change.

`api_token` uses the same write-only mechanics as `secret_env_vars`:
**Terraform 1.12+ or OpenTofu 1.13+**, in a provider release containing
the attribute. On an older CLI, leave it out and paste the token in the
panel. The value never enters state or plan; `token_set_at` (the
rotation clock) and the claimed domain names do. On create the token is
sent once, after the gateway serves; afterwards it is sent only when
`rotate_api_token_on` changes. Import leaves both token attributes null.

## Gateway mint enrollment (from 0.6.0)

From 0.6.0, `fluxvale_access_gateway` can be enabled with `api_token`
alone: the platform mints the gateway's join key from the token (it
needs a token from a tailnet owner/admin account (console API tokens carry the account's full permissions; the old `dns`/`devices:routes`,
and `devices:core:read`). `auth_key` remains the documented fallback:
paste a join key minted in the Tailscale admin console instead. No
renames and no changed attribute semantics — the stable names above are
untouched, and existing `auth_key` configurations keep working as
before.

## Gateway OAuth client pair (from 0.7.0)

From 0.7.0, `fluxvale_access_gateway` gains `ts_client_id` (plain —
public identity, kept in state), `ts_client_secret` (write-only,
sensitive), and `rotate_ts_client_secret_on` — the PREFERRED enrollment
credential: an OAuth client created in the Tailscale admin console
(Settings → OAuth clients) carrying the scopes the platform needs
(`auth_keys`, `dns`, `devices:routes` write, `devices:core:read`) and
the `fluxvale-gateway` tag. The platform exchanges the pair for a
short-lived, tailnet-scoped token and mints the join key from it; the
secret is never a direct Bearer. No renames: `api_token` (the
console-token alternate) and `auth_key` (the legacy fallback) keep
their names and semantics, and every stable name above is untouched.
`ts_client_secret` uses the write-only mechanics — **Terraform 1.12+
or OpenTofu 1.13+**, in a provider release containing the attribute; on
an older CLI, leave the pair out and paste it in the panel. The value
never enters state or plan; rotating it is a trigger bump
(`rotate_ts_client_secret_on`), an in-place PATCH — never a
replacement.

**1.0 freeze**: when the first non-owner user writes `.tf` against this
provider. From then on, removals and renames are versioned decisions.
