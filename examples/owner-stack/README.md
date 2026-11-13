# The owner stack (one root module)

The owner's personal production stack, codified: one R2 bucket
feeding every instance's backups, the tailnet access gateway for the
private instances, and the instances tying it together — the #482
rider. This is a **skeleton**: placeholder variables only, nothing
real in git, nothing applies until the owner fills in
`terraform.tfvars` and runs it himself. The pairing step below is the
checklist for exactly that.

The single-instance teaching example with explicit resource pairs
lives next door in
[examples/personal-stack](../personal-stack/README.md).

## What it stands up

- **1 R2 bucket** (cloudflare provider) — every instance's backups
  land in it under its own prefix (`fluxvale/<name>/`).
- **3 instances** (`var.instances`, keyed by name) — defaults are
  three representative shapes pinned to catalog versions: a public
  always-on forgejo, memos and kavita private behind the gateway.
  Versions are explicit pins (bumping replaces the instance — the
  volume caveat under "Run and verify"; the catalog's row order is
  semver-newest-first (#700), but a pin preconditions against the
  catalog at plan and survives re-ordering — no "first row" shortcut
  in code, on purpose).
- **Encrypted state and plan** — the root module declares OpenTofu's
  `encryption` block with `enforced = true`, keyed by
  `TF_VAR_state_passphrase` from the keychain or a secret manager.
  This stack needs OpenTofu 1.13+ and provider 0.4.0+ (release pending); Terraform has no
  native state encryption (see the provider's [Secrets in Terraform](https://github.com/fluxvale/terraform-provider-fluxvale/blob/main/docs/guides/secrets.md)
  guide for the write-only input and backend-encryption path).
- **Write-only secrets** — `instances.<name>.secret_env_vars` plus
  `secret_env_vars_version` ride the same in-place env PATCH as
  `env_vars`, but their values never enter state or plan files. Bump
  the version to rotate; drop a key and bump to remove.
- **1 backups row per instance** — endpoint composed from the
  Cloudflare account id, bucket from the TF-created bucket, per
  instance prefix and retention.
- **The access gateway** — joins the tailnet, attaches every
  `exposure = "private"` instance (the attach waits for the gateway
  to become active).
- **Custom-domain registrations per instance**
  (`var.instances.<name>.domain`) — setting a domain registers the
  NAME on the private-access edge; nothing serves it yet (there is no
  instance-domain attachment in the platform — that link is the
  dogfood plan's to build). The dogfood pairings — forgejo →
  `git.fluxvale.com`, umami → `analytics.fluxvale.com` — ride as
  documented placeholder rows; the defaults register nothing.

The dependency graph IS the apply order — one `tofu apply` handles
it, no manual steps: backups rows follow their instance and the
bucket; the attach calls follow the gateway and each private
instance.

## The pairing checklist — what you bring

| Value | Where it comes from | Goes where |
| --- | --- | --- |
| `FLUXVALE_PAT` | fluxvale.com → Settings → Developer → API tokens (scopes: `instances:write backups:write gateways:write catalog:read`) | `export FLUXVALE_PAT=fvpat_…` — **only** the environment, never a file, never pasted into the repo or notes |
| `state_passphrase` | your keychain or secret manager — a long passphrase (16+ chars) for state/plan encryption | `export TF_VAR_state_passphrase=…` — never in `terraform.tfvars`, never in git |
| `cloudflare_api_token` | Cloudflare dashboard → API tokens (R2 edit on the account) | `terraform.tfvars` |
| `cloudflare_account_id` | Cloudflare dashboard → account overview | `terraform.tfvars` |
| `r2_access_key_id` / `r2_secret_access_key` | Cloudflare dashboard → R2 → Manage API Tokens (object read/write) | `terraform.tfvars` |
| `r2_bucket_name` | decision — default `fluxvale-backups`, unique in the account | optional, `terraform.tfvars` |
| `tailnet_domain` | Tailscale admin console — the MagicDNS suffix (`<tailnet>.ts.net`) | `terraform.tfvars` |
| `tailscale_auth_key` | Tailscale admin console → Settings → Keys (an auth key that can enroll a device) | `terraform.tfvars` — set BEFORE the first apply when enabling (with it null, the apply half-lands: bucket, instances, backups, then fails at the gateway create); leave null only when adopting an existing gateway by import |
| `instances` | the catalog (listing one-liner below) + app/version/size choices | optional, `terraform.tfvars` |
| `instances[].domain` | the dogfood plan — `git.fluxvale.com` (forgejo) and `analytics.fluxvale.com` (umami) are the placeholder pairings | optional, `terraform.tfvars` — left unset until that plan lands |

The token scopes match the workflow's acceptance preflight
(`.github/workflows/acceptance.yml`); mint once, reuse.

To see the catalog while picking apps and version pins (after
`tofu init` and with `FLUXVALE_PAT` exported — the console reads the
live catalog):

```sh
tofu console
> [for a in data.fluxvale_apps.catalog.apps : a.slug]
> [for v in data.fluxvale_app_versions.all["memos"].app_versions : v.version]
```

NB the versions list is semver-newest-first (the API's own order
since #700 — `3.4.1` sits above `3.4.1-bundled`, a pre-release
sorts below its release). Reading `app_versions[0]` as "newest" is
now correct; the instances above still pin explicitly because a pin
survives re-ordering and new releases.

## Install the providers (once)

This module composes two providers, and both install from their
registries: plain `tofu init` pulls fluxvale and cloudflare.

For an offline or air-gapped run, the fluxvale zips also install from a
[filesystem mirror](../../docs/guides/install.md): download the release
zip + the versioned `*_SHA256SUMS` (+ `*_SHA256SUMS.sig`), verify, drop
the zip into `~/.tfplugins-mirror`, point `~/.tofurc` at it — §1–3
walks it.

## Secrets and encryption

The module demonstrates the provider's write-only input. Put a secret in
`secret_env_vars`, set `secret_env_vars_version = 1`, and keep the value
in a sensitive variable or your CI secret store:

```hcl
instances = {
  # …the other rows…
  umami = {
    app                    = "umami"
    version                = "3.4.1-bundled"
    # …allocation and posture…
    secret_env_vars         = { APP_SECRET = "…" }
    secret_env_vars_version = 1
  }
}
```

The value never appears in `terraform.tfstate` or the saved plan. Prove
it after an apply — on this stack the state is encrypted, so read the
decrypted form:

```sh
tofu state pull | grep -c 'the-secret-value'
# 0
```

**Rotate** by changing the value and bumping
`secret_env_vars_version = 2`; the provider re-sends every secret key
and the running instance rolls. If the app's quota cannot fit a second
pod during that rollout, stop it first (`running = false`), apply the
bump, then start it again (`running = true`). **Remove** a key by
deleting it from the map and bumping again. Removing the last key may drop both attributes
or keep `secret_env_vars = {}` with a final bump. A key present in both
`env_vars` and `secret_env_vars` is a plan-time error. The computed
`secret_env_vars_keys` list shows what is managed without showing values.

The root module enforces OpenTofu state and plan encryption. Retrieve a
passphrase from the keychain (never write it to `terraform.tfvars`):

```sh
export TF_VAR_state_passphrase="$(security find-generic-password -w -s fluxvale-state)"
# Linux: secret-tool lookup service fluxvale-state
# 1Password: op read 'op://Private/fluxvale-state/passphrase'
```

`enforced = true` means a missing passphrase refuses the run; it never
silently writes plaintext. Existing unencrypted state needs the one-time
`unencrypted` fallback migration in the provider's
[Secrets in Terraform](https://github.com/fluxvale/terraform-provider-fluxvale/blob/main/docs/guides/secrets.md)
guide. Losing the passphrase makes the state unrecoverable — back it up.

The write-only attributes need provider 0.4.0+ (the release containing
#764, pending) and OpenTofu 1.13+ or Terraform 1.12+. The `enc:v1:` marker is an
accepted design, not a shipped fallback — do not rely on it yet.

## Run and verify

```sh
cd examples/owner-stack
cp terraform.tfvars.example terraform.tfvars   # then fill it in
export FLUXVALE_PAT=fvpat_…                    # never in a file
export TF_VAR_state_passphrase=…               # from the keychain/secret manager
tofu init                                      # registry defaults (fluxvale + cloudflare); mirror optional
tofu plan
tofu apply
tofu output
```

Rehearse against staging first if wanted: `api_url =
"https://staging.fluxvale.com"` plus a staging PAT in the env.

Verified when:

- `tofu plan` shows 1 bucket, N instances, N backup rows, the
  gateway (or an adoption import — see below), nothing unexpected.
  One known wart from the SECOND plan onward: once the gateway
  exists, its unset `organization` attribute re-plans unknown against
  the stored id and the plan shows a spurious gateway replacement
  (provider bug, [fluxvale#693](https://github.com/fluxvale/fluxvale/issues/693));
  the plan cannot actually replace the singleton. Naming the org
  explicitly — uncomment `organization` on the gateway resource and
  set your org slug — makes it stop. The same class can show as
  in-place churn on registered custom-domain rows.
- `tofu apply` completes; `tofu output` prints the public URLs and
  the gateway host.
- `tofu state pull | grep -c '<the-secret>'` prints `0`; the state file
  starts with OpenTofu's encryption metadata, and the plan file is
  encrypted too.
- The panel shows the instances running, backups rows enabled, and
  the Access tab shows the gateway active with the private instances
  attached.
- From a device on the tailnet, the private instances answer through
  the gateway.

Two honest caveats: **the gateway is an org singleton that bills
monthly, needs the org wallet paid AND funded, and has no API
delete** — destroy removes it from state only; opt out via the
panel's Access tab. It applies unconditionally, so remove the
resource to skip it entirely; to adopt an existing gateway, set
`tailscale_auth_key` null first, then
`terraform import fluxvale_access_gateway.owner <org_id_or_slug>`.
And **backups need the org to have paid once** — a 403 on the first
backups save is the wallet, not a bug.

A third, for upgrades: **bumping an instance's version pin replaces
it, and the teardown deletes the old instance's app-data volume** —
restore from the R2 backups or the data is gone.

## Deliberately not decided here

The custom-domain resource IS composed (per-instance `domain`
attribute) — what's pending is the dogfood initiative's decisions
(domains, OIDC, money gates; a separate lane). Until it lands the
pairings stay placeholder rows: no domain registers on apply. Two
known platform facts for that conversation: the first
`fluxvale_custom_domain` create passes the org-wallet gate
(`ActorOrgHasPaid`), and `verified` is NOT resolution — the
split-DNS claim has no API surface yet
([examples/personal-stack](../personal-stack/README.md) documents
both).
