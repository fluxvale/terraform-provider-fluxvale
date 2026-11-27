---
page_title: "fluxvale_instance_hostname"
---

# fluxvale_instance_hostname

One custom address for an instance — a label such as `git` under a verified custom domain your organization owns, minted as one platform row. Identity is create-only: the member API has no update route, so changing `instance_id`, `access_domain_id`, or `label` replaces the row (never the instance — the instance's own lifecycle is untouched). Serving is the platform's rule, not an attribute: today an address serves only when the domain is verified, the org's gateway has claimed it, and the instance is private and attached to that gateway; a public-serving arm is on the platform's roadmap — until then a public instance's address is recorded and serves nothing yet.

## Example Usage

```hcl
resource "fluxvale_custom_domain" "apps" {
  domain                = "apps.example.com"
  wait_for_verification = true
}

resource "fluxvale_instance" "git" {
  # … name, app_version_id, allocation, exposure = "private" …
}

# git.apps.example.com — the row under the domain, on the instance.
resource "fluxvale_instance_hostname" "git" {
  instance_id      = fluxvale_instance.git.id
  access_domain_id = fluxvale_custom_domain.apps.id
  label            = "git"
}
```

## Argument Reference

- `instance_id` (Required) — the instance's id; create-only, replaces the row.
- `access_domain_id` (Required) — the access-domain id of a verified custom domain owned by the instance's organization; the platform refuses a platform domain, an unverified domain, and a domain from another organization — all 400s surface verbatim; create-only, replaces the row.
- `label` (Required) — the DNS label below the domain (`git` → `git.<your-domain>`); the platform's grammar is lowercase alnum + inner hyphens, 1-63 chars, first and last char alnum — the 400 surfaces verbatim (the provider adds no plan-time check by design); create-only, replaces the row.

## Attributes Read

`id` — the hostname row id, the remove path's key and the second half of the import id.

## Notes

- **Serving is the platform's rule.** The row records the address; nothing on it reports whether the name answers. Today an address serves only when the domain is verified, the org's gateway claims it, and the instance is private and attached to the gateway — the custom domains guide's rule; a public-serving arm is on the platform's roadmap, and until it lands a public instance's address is recorded and serves nothing yet. Compose the name yourself: `"${fluxvale_instance_hostname.git.label}.${var.custom_domain}"` (there is deliberately no FQDN attribute — the wire row carries only its identity and where it sits).
- **Addresses are unique across the whole platform.** Uniqueness is one label per domain, and domain names are platform-wide unique across organizations — label+domain composes to an address no other organization can hold. The duplicate refusal surfaces verbatim.
- **A 400 or 409 may leave the row standing.** The platform answers 400 when the address minted but the app could not be told (restart refused, or the delivery failed), and 400 or 409 on a duplicate on the same instance (the platform's own tests accept either status there). On either status the provider re-reads the instance's list once and adopts the identical standing row — the same domain and label, on this instance — with the server's message as a warning; with no such row standing, the refusal surfaces verbatim.
- **The row's lifecycle rides its owners.** Destroying the instance destroys its rows (the platform cascades) — the provider's remove answers 404 and treats it as already gone. Removing a domain that still carries hostnames is refused (the platform's 400): the address rows go first, and Terraform orders them first when the config references the domain (as the example does). The platform-minted default address (`<slug>.<platform-domain>`) is not this resource's surface: import it and destroy refuses with the platform's words.
- **Every refusal surfaces verbatim** — the 404 for a foreign instance or unreadable domain, the 400s for the domain guards, the 403 for a token without `instances:write` (which also covers list and remove; `instances:read` alone lists but cannot mint or remove).

## Import

`terraform import fluxvale_instance_hostname.x <instance_id>/<hostname_id>` — the instance whose list carries the row, then the row id, one slash between (there is no GET by hostname id, so the read is instance-scoped). The config-side `import` block form:

```hcl
import {
  to = fluxvale_instance_hostname.git
  id = "<instance_id>/<hostname_id>"
}
```

The read fills `label` and `access_domain_id` from the platform row; a config whose three attributes match the imported row yields no diff.
