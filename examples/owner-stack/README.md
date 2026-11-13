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

## Run and verify

```sh
cd examples/owner-stack
cp terraform.tfvars.example terraform.tfvars   # then fill it in
export FLUXVALE_PAT=fvpat_…                    # never in a file
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
