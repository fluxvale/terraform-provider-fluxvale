---
page_title: "fluxvale_organization"
---

# fluxvale_organization

A team organization — the ownership and billing boundary for every instance, domain, and gateway.

Creating the org makes the authenticated user its owner, in the same transaction. It does not fund the org: a team org has no wallet and no welcome credit, so the first deploy still meets the platform's top-up gate.

```hcl
resource "fluxvale_organization" "team" {
  name = "Acme"
  slug = "acme"

  lifecycle {
    prevent_destroy = true
  }
}

resource "fluxvale_instance" "reading" {
  organization   = fluxvale_organization.team.slug
  name           = "Reading"
  app_version_id = data.fluxvale_app_versions.kavita.app_versions[0].id
  cpu_cores      = 0.5
  memory_mb      = 512
  storage_gb     = 20
}
```

The instance's `organization` reference is the dependency edge: the org exists before the instance, and Terraform destroys the instance before the org.

## Argument Reference

- `name` (Required) — the display name, at most 100 characters. Updates in place.
- `slug` (Required) — the org's address: 2–39 characters of lowercase letters, digits, and hyphens, starting and ending with a letter or digit (`^[a-z0-9][a-z0-9-]*[a-z0-9]$`). Create-only in this provider: the machine API's rename is name-only, so a changed slug plans a replacement — and the provider **rejects** that plan, because a non-empty org's destroy is refused and an empty one would still lose its id and billing history. To follow a panel rename, `terraform state rm` this address and `terraform import` the new slug.

  The account may own at most 10 organizations, personal org included; the 11th create is a policy 403.

## Attributes Read

- `id` — the organization id, used for refresh, update, delete, and the provider's internal references.

## Notes

- **Reserved slugs are refused for every token.** The platform's brand reservations (`fluxvale`, `docs`, `app`, …) and the route sentinel `new` apply to Terraform exactly as they apply to the panel, and a token is never an operator act — a platform admin's token still gets the refusal. Create a reserved brand org in the panel as a platform admin, then `terraform import`. The error surfaces verbatim (`fluxvale API: status 400: is reserved`).
- **Delete only deletes an empty org.** It refuses a personal org, an org that still owns instances, custom domains, a private-access gateway, or has members who are not owners, with the platform's own message (for example `This organization still has 1 instance. Delete it first.`). Invites and derived platform rows do not block.
- **An org that has ever had a wallet row is retained.** Billing records outlive the organization (the platform's [privacy policy](https://fluxvale.com/policies/privacy) states the retention). The row appears on the first visit to the org's home or billing page, a top-up, or a deploy — so even a never-funded org becomes permanently undeletable once a member opens it. `prevent_destroy` is the right posture for any org you care about.
- **`prevent_destroy` fails the whole destroy at plan time.** Nothing under the org is destroyed — the instances survive. Remove the block to tear everything down, or leave it and run `tofu destroy -exclude=fluxvale_organization.team` to destroy the rest of the stack while keeping the org.
- **The provider's replacement guard is value-based.** `tofu apply -replace=fluxvale_organization.team`, or a slug whose value is unknown at plan time, can still plan a replacement: an empty org is then deleted, a non-empty one hits the guard. Do not force replacement on an org you care about.
- **The provider-level `organization` does not apply to this resource.** It is not org-scoped. Leave the provider-level default unset in a stack that manages its own org; naming the org per resource (`organization = fluxvale_organization.team.slug`) avoids a provider configuration that depends on a resource.
- **Token scopes.** Creating, renaming, and deleting need `organizations:write`; reads and imports need `organizations:read`. Every resource resolves its org through the org list, so all of them need `organizations:read` — a token minted before this resource existed lacks it and 403s on the first org resolution.

## Import

`terraform import fluxvale_organization.team acme`

The slug is the import key; the read resolves it and fills `id`, `name`, and `slug`. A token that is not a member gets the same `404` as a missing id — the platform never reveals a hidden org's existence, so a revoked membership and a deleted org are indistinguishable. The next apply plans a create, which fails while the slug is still taken. A token without `organizations:read` gets the platform's missing-scope 403.
