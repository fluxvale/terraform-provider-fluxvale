---
page_title: "fluxvale_instance"
---

# fluxvale_instance (data source)

Read one existing instance — one managed in another stack, or one you
do not manage with Terraform — by `id` or by `slug`, exactly one.

The lookup is the API's own: your token's memberships decide what is
visible, so an instance in an organization you do not belong to reads
as not found.

## Example Usage

```hcl
# By id — always unambiguous.
data "fluxvale_instance" "wiki" {
  id = "5f0c2a9e-0f6d-4b6a-9c2f-1d3e5a7b9c01"
}

# By slug — the DNS subdomain. A slug is unique only within a
# cluster, so pass the organization context when your token spans
# several.
data "fluxvale_instance" "wiki_by_slug" {
  slug         = "velvety-otter-1234"
  organization = "acme" # id or slug; the provider-level default is the fallback
}
```

## Argument Reference

- `id` (Optional) — the instance id. Exactly one of `id` / `slug`.
- `slug` (Optional) — the instance's slug. Exactly one of `id` / `slug`.
- `organization` (Optional) — the slug lookup's organization context,
  by id or slug. Falls back to the provider-level `organization`.
  Ignored for an id lookup (an id is unique). With neither this nor the
  provider default set, the lookup runs without an organization filter.

## Attributes Reference

Computed:

- `id` — the instance id.
- `slug` — the instance's slug.
- `name` — the display name.
- `namespace` — the `fluxvale-app-<id>` namespace (null until deployed).
- `status` — the state machine's status.
- `exposure` — `"public"` or `"private"`.
- `always_on` — whether the instance opts out of scale-to-zero.
- `image` — the app version's image, set at create (not deploy-derived).
- `port` — the app version's port, set at create (not deploy-derived).
- `deployed_at` — when the current deploy was enqueued (null before the
  first deploy).
- `organization_id` — the owning organization's id.
- `app_version_id` — the instance's app version id, set at create.

## Notes

**Exactly one lookup form.** Setting both `id` and `slug`, or neither,
is an error at read time — the provider refuses before calling the API.
Empty (or whitespace-only) strings count as unset.

**Ambiguous slugs.** A slug is unique only per cluster
(`[:slug, :cluster_id]`), not globally. A slug that still matches more
than one visible row is the API's `400` (`multiple_results`) — pass
`organization` to narrow it, or use the instance `id`. The provider
reports an `Ambiguous instance slug` diagnostic with that remedy and
includes the server's detail.

**Not found.** An unknown id, an unknown slug, and an instance in an
organization you are not a member of are all the API's `404`; the
provider reports `Instance not found` and names the lookup (id, or slug
with its organization context).

**Sparse fieldset.** The provider requests the detail field set plus
`organization_id` and `app_version_id`, so both ids are filled when the
server has them. `image`, `port`, and `app_version_id` are set at create
from the app version; only `namespace` and `deployed_at` are null on a
pending row. Absent FK ids map to null rather than an error.
