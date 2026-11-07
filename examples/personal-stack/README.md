# The owner's personal stack (example)

The shape of the stack this provider exists to manage — #482's
acceptance story, placeholders only. The PA fills
`terraform.tfvars.example → terraform.tfvars` and runs the acceptance
plan/apply/destroy against the owner's real organization.

## What it models

- **A public instance** (forgejo-class, 1.0 CPU / 1024 MB / 5 GB,
  always-on) — the everyday public box.
- **A private instance** (memos-class, 0.5 CPU / 512 MB / 2 GB,
  exposure `private`) — reachable only through the access gateway.
- **The access gateway**, joined to the owner's tailnet, with the
  private instance attached via its `instances` set (the attach waits
  the gateway to `active`).
- **One R2 bucket** created by the cloudflare provider, feeding BOTH
  instances' `fluxvale_instance_backups` via plain Terraform
  interpolation — endpoint composed from the account id, bucket from
  `cloudflare_r2_bucket.backups.name`, keys from vars, per-instance
  prefixes. This composition is the point of the example.
- **The custom domain** — pending until #548 merges: the resource
  lives in `custom-domain.tf.example`; restore it to
  `custom-domain.tf` once the provider registers the type (the file's
  header says so).

App identity flows through the catalog data sources:
`fluxvale_apps` guards the slugs exist; `fluxvale_app_versions`
resolves each version var to the `app_version_id` the instances pin.

## What to fill in

Copy `terraform.tfvars.example` to `terraform.tfvars`, replace every
placeholder. Preferred credential path: leave `fluxvale_pat` null and
`export FLUXVALE_PAT=fvpat_…` — no secrets in files. The rest (R2 keys,
Cloudflare token, Tailscale auth key, domain) come from the owner's
accounts.

## Acceptance

Acceptance is a real `tofu plan` → `tofu apply` → `tofu destroy`
against the owner's real organization — NOT this repo's CI. CI's gate
(`internal/provider/e2e_test.go`: a real OpenTofu CLI driving the real
binary against a fake API) proves the provider drives tofu; only the
owner can prove the org accepts this stack. Rehearse against staging
(`api_url` override), then production. `destroy` is part of the
acceptance: everything here tears down, except the access gateway —
destroying it removes it from state only (the API has no delete route;
opt out via the panel's Access tab).

## `verified` is NOT resolution

The custom domain's `state` reaching `verified` means the wildcard
certificate was issued (DNS-01 answered by your DNS delegation). It
does NOT mean the name resolves: the split-DNS claim that points the
name into the tailnet has no API surface yet — claiming is a separate
platform capability on a later slice. Watch `domain_state`, expect no
traffic until that lands.

## Notes

- Organization resolution: no resource names an `organization` — the
  token belongs to the owner's sole organization and everything
  resolves to it. With several memberships the provider errors rather
  than pick.
- `secret_access_key` is write-only (set ⇒ rotated on every save); the
  R2 keys are account-level credentials minted in the Cloudflare
  dashboard — no TF resource manages them.
- The custom domain arrives as `custom-domain.tf.example` — the
  provider doesn't register `fluxvale_custom_domain` until #548
  merges; restore the file then (its header says so). The `verified
  is NOT resolution` section below applies once it's live.
