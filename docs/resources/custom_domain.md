---
page_title: "fluxvale_custom_domain"
---

# fluxvale_custom_domain

Your own domain on the private-access edge (`apps.example.com`).

Register lands the row `pending` and mints the wildcard Certificate in the same transaction; verification is issuance — the platform's observer pass (about a 15-minute cadence) promotes `pending → verified` once your DNS delegation answers the DNS-01 challenge, and fails the row past its issuance window (~72h). Default create returns at `pending`; refreshes show the progression via `state`. Set `wait_for_verification = true` to poll instead — bounded by `timeouts.create` (default 45m). A timeout leaves the row tainted but tracked (it keeps verifying server-side): `tofu untaint` — or `terraform untaint` — keeps the registration, and the following refresh shows the promoted state; without untainting, the next apply destroys and re-registers. `failed` fails the create fast (it's terminal: remove and re-register).

## Example Usage

```hcl
data "fluxvale_domain_delegation" "apps" {
  domain = "apps.example.com"
}

# Create the returned CNAME at your DNS host first (Cloudflare:
# proxied = false); see the data source page and the cloudflare-stack
# example. The private lane needs one _acme-challenge record and no
# host record. This resource has no dns_records attribute: the data
# source is the only pre-registration source.

resource "fluxvale_custom_domain" "apps" {
  domain                = "apps.example.com"
  wait_for_verification = true
}
```

## Argument Reference

- `domain` (Required) — your own domain (`apps.example.com`). Names are unique platform-wide, across ALL organizations; the platform canonicalizes (trim + downcase), so any config casing converges and only a genuinely different canonical name replaces.
- `organization` (Optional) — id or slug; the plan normalizes a set value to the resolved id, so a slug config converges with an imported id. Only a genuinely different organization replaces.
- `wait_for_verification` (Optional, bool) — poll `pending → verified` instead of returning at `pending`; bounded by `timeouts.create` (default 45m).

## Attributes Read

`id`, `kind` (`custom` for provider-created rows; `platform` for system-minted ones), `state`, `organization_id`.

## Notes


- **`verified` is NOT resolution by itself.** Promotion means the wildcard Certificate issued (DNS-01). The name answers only after the org's gateway claims it: saving the gateway's Tailscale API token claims the verified domains it can, and a later verification or attach claims too. The claim joins the apply that creates the gateway or bumps `rotate_api_token_on` — order the gateway after this resource so the verified row exists first. Off tailnet the name does not resolve.
- `resume` is deliberately not exposed. A `suspended` domain is legacy state: domains are free, nothing current suspends one — the state and its resume survive for old rows only. Watch `state`.
- Names are unique platform-wide, across ALL organizations: the duplicate 400 surfaces verbatim (the name may belong to another org). The platform canonicalizes (trim + downcase); the provider sends the canonical name on the wire and decides replacement on canonical forms, so any config casing converges — the plan and state keep the form you wrote. Only a genuinely different canonical name replaces (hand-edited state included — the canonical row is the truth).
- `organization` accepts id or slug; the plan normalizes a set value to the resolved id, so a slug config converges with an imported id. Only a genuinely different organization replaces.
- Destroy is the platform's full teardown behind one synchronous DELETE (hostname pre-check → tailnet unclaim → row destroy → CoreDNS re-render → Certificate safe-delete); a 404 is already-gone. A domain still serving instance hostnames is refused (the platform's 400): remove those first.

## Import

`terraform import fluxvale_custom_domain.x <id>`. The read goes by id (no org resolution — a multi-org token imports fine, and a revoked membership still refreshes) and fills the wire mirror: `domain` (the canonical name), `kind` (`custom` for provider-created rows; an imported id may be a system-minted `platform` row), `state`, `organization_id` — and seeds `organization` with the wire's id, so a config that leaves it unset (or names it by slug) converges.
