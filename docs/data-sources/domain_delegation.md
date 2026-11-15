---
page_title: "fluxvale_domain_delegation"
---

# fluxvale_domain_delegation (Data Source)

The DNS delegation records a custom domain needs **before** it is
registered. The private-access lane needs exactly one
`_acme-challenge` CNAME; there is no host record. The platform's
cert-manager DNS-01 solver runs with `dns01.cnameStrategy: Follow`, so
the CA follows that CNAME into the platform's validation namespace and
writes the TXT there.

This is a data source, not a field on `fluxvale_custom_domain`, on
purpose. The platform API keeps a computed mirror for the panel and API
callers; the provider resource deliberately does not expose it, because
Terraform does not publish a resource's computed values until its create
returns, and a domain created with `wait_for_verification = true` waits
for the DNS-01 challenge this record answers. The data source is
row-free, so the graph is acyclic and one apply works.

## Example Usage

```hcl
data "cloudflare_zone" "this" {
  filter = {
    name = "example.com"
  }
}

data "fluxvale_domain_delegation" "apps" {
  domain = "apps.example.com"
}

resource "cloudflare_dns_record" "challenge" {
  zone_id = data.cloudflare_zone.this.zone_id
  name    = data.fluxvale_domain_delegation.apps.dns_records[0].name
  type    = data.fluxvale_domain_delegation.apps.dns_records[0].type
  content = data.fluxvale_domain_delegation.apps.dns_records[0].value
  proxied = false
  ttl     = 300
}

resource "fluxvale_custom_domain" "apps" {
  domain                = "apps.example.com"
  wait_for_verification = true
  depends_on            = [cloudflare_dns_record.challenge]
}
```

The example targets Cloudflare provider v5. On v4 the record resource is
`cloudflare_record` and the zone data source takes `name` directly
instead of `filter`; the record fields are otherwise the same.
`proxied = false` is mandatory — a proxied CNAME breaks the delegation.

## Argument Reference

- `domain` (Required) — the domain you will register, e.g.
  `apps.example.com`. The platform canonicalizes it (trim + downcase) and
  refuses an invalid name or FluxVale's own platform domain.

## Attribute Reference

- `dns_records` — a list of `{name, type, value}` records. The private
  lane carries one `CNAME`. Bind a DNS provider's `name`/`content` to
  it; do not synthesise a host record.
