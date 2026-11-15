# Cloudflare + FluxVale custom domain

One stack: the `_acme-challenge` delegation record, the domain
registration and verification, and the gateway token claim.

## What it creates

- `data.fluxvale_domain_delegation.apps` — the row-free preview of the
  records the domain needs. This is a **data source**, not a field on
  the domain resource, on purpose: the platform API keeps a computed
  mirror for the panel, but the provider resource does not expose it,
  because Terraform publishes a resource's computed values only when
  its create returns — and `fluxvale_custom_domain` with
  `wait_for_verification = true` waits for the DNS-01 challenge this
  record answers. The data source breaks that cycle, so one apply
  works.
- `cloudflare_dns_record.challenge` — the `_acme-challenge` CNAME,
  DNS-only (`proxied = false`).
- `fluxvale_custom_domain.apps` — the registration; create returns
  once the observer sees the wildcard Certificate Ready.
- `fluxvale_access_gateway.gateway` — the org's gateway with the
  Tailscale API token. The token save attempts to claim every verified
  domain, so this resource waits for the domain; read `claimed_domains`
  for the result.

The private lane needs **one** record. There is no host CNAME: the name
answers only through the claiming gateway's tailnet (the resolver
answers gateway-sourced queries with the gateway's tailnet IP; off
tailnet it does not resolve).

## Run it

```sh
cp terraform.tfvars.example terraform.tfvars
# edit the four values
export FLUXVALE_PAT='...'
export CLOUDFLARE_API_TOKEN='...'
export TF_VAR_tailscale_api_token='...'
tofu apply
```

The Tailscale API token must come from a tailnet owner/admin account
that can manage DNS and device routes (`dns`, `devices:routes`,
`devices:core:read`); `auth_keys` is not required. It is write-only: it
never enters state or plan. Bump `tailscale_api_token_version` whenever
the token changes; the provider re-sends it.

## Expected outputs

```
domain_state     = "verified"
claimed_domains  = ["apps.example.com"]
token_set_at     = "2026-11-..."
```

`verified` is issuance, not resolution. `claimed_domains` non-empty is
the resolution signal: the gateway holds the domain's split-DNS claim.
For an instance to answer, it must be private, attached to the gateway,
and have a hostname minted under the domain.

## Adopting an existing gateway

The API has no gateway-delete route, and an org already has one gateway.
To manage the token on it instead of creating a new one, import by the
org id or slug and leave `auth_key` null:

```hcl
import {
  to = fluxvale_access_gateway.gateway
  id = "<org-id-or-slug>"
}
```

Set `tailnet_domain` to the stored value. The Update then sends the
token only when `rotate_api_token_on` changes; on the first apply set
it to a fresh value (for example a date).

## Teardown

`tofu destroy` removes the domain (full server-side teardown: unclaim,
row, CoreDNS render, Certificate) and the Cloudflare record. The
gateway is removed from state only — the platform keeps it running and
billing; opt out through the panel's Access tab if you really want it
gone. Remove the delegation CNAME when the domain is removed; a
dangling delegation is a takeover path.
