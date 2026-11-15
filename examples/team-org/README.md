# Team organization + first app

A `fluxvale_organization` and a `fluxvale_instance` in one apply. The
org is the ownership and billing boundary; the instance's
`organization = fluxvale_organization.team.slug` reference orders the
graph — org first on apply, instance first on destroy.

## Run it

```sh
cp terraform.tfvars.example terraform.tfvars   # edit org_name/org_slug, app, size
export FLUXVALE_PAT=...                        # or: fluxvale auth exec -- tofu apply
tofu init
tofu plan
tofu apply
```

The token needs `organizations:read` and `organizations:write` (plus
the scopes for everything else you manage). See the platform's
[Terraform credentials](https://fluxvale.com/docs/automation/terraform-credentials)
page for the minting recipe.

## What to expect

- **The org is not funded.** A team org has no wallet and no welcome
  credit, so the first deploy still meets the platform's top-up gate.
  Fund it from its own billing page only when you mean to keep the org
  — opening that page creates the wallet row, and an org with a wallet
  row can never be deleted.
- **Apply order is org → instance.** `tofu apply` prints the org's
  creation before the instance's; the instance's `organization` line
  is the edge, not a naming coincidence.
- **Destroy order is instance → org.** The instance goes first and the
  org last; an org that still owns anything is refused with the
  platform's message.
- **Reserved slugs are refused.** `fluxvale`, `docs`, `app`, `new`, and
  the rest of the platform's reservation list cannot be created through
  the API — even by a platform admin's token. Create a reserved brand
  org in the panel, then `terraform import fluxvale_organization.team
  fluxvale`.

## Destroy

With `prevent_destroy` set, `tofu destroy` fails at plan time and
nothing is destroyed — the instance survives too. Remove the block to
tear everything down, or leave it and run `tofu destroy
-exclude=fluxvale_organization.team` to destroy the rest of the stack
while keeping the org.
The org itself deletes only while empty; one that has had a wallet row
is refused (`This organization has billing history and is retained.`),
because billing records outlive the org. That is the platform's rule,
not a provider bug. Keep `prevent_destroy` on any org you care about.
