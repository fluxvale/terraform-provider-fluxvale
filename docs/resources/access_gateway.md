---
page_title: "fluxvale_access_gateway"
---

# fluxvale_access_gateway

The org's singleton private-access gateway.

The API's gateway surface is create, the singleton read, the attach/detach verbs, and `PATCH /access-gateways/:id/resume`; the provider deliberately does not expose resume — a suspended gateway is the zero-balance ladder's out-of-band platform state (resume via the panel or MCP, watch `state`), not something to flip from a config file.

## Example Usage

```hcl
resource "fluxvale_access_gateway" "org" {
  tailnet_domain = var.tailnet_domain
  auth_key       = var.tailscale_auth_key
  instances      = [fluxvale_instance.memos.id]

  api_token           = var.tailscale_api_token
  rotate_api_token_on = var.tailscale_api_token_version
}
```

## Argument Reference

- `tailnet_domain` (Required) — the tailnet the gateway joins.
- `auth_key` (Optional, Sensitive) — the join credential, required only when this resource ENABLES a gateway (the wire never echoes it, so imports manage attachments with it null).
- `api_token` (Optional, Sensitive, Write-only) — the Tailscale API access token the platform uses to claim verified custom domains. Mint it from a tailnet owner/admin account that can manage DNS and device routes (the `dns`, `devices:routes`, and `devices:core:read` permissions); `auth_keys` is not required, and keys expire within 90 days. It never enters state or plan and the API never echoes it. On create it is sent once, after the gateway reaches `active`; the save attempts to claim every verified domain — read `claimed_domains` for the result. Requires Terraform 1.12+ or OpenTofu 1.13+ and a provider release containing this attribute; older CLIs leave it out and paste the token in the panel.
- `rotate_api_token_on` (Optional) — the token's rotation trigger, an arbitrary string (a date, a version, a nonce). Changing it re-sends the current `api_token` in place; an unchanged value never re-sends, because a write-only value cannot produce a plan diff. Set `api_token` whenever this changes.
- `organization` (Optional) — id or slug; falls back to the provider-level `organization`, then to your sole org. The singleton is resolved per org.
- `instances` (Optional, set of instance ids) — converges via the attach/detach verbs, one call per added/removed id. Attaching waits the gateway to `active` (create lands `provisioning`; the observer promotes within about a minute). The set is config-authoritative: the API has no attachment list, so out-of-band panel attach/detach is invisible to the provider.

## Attributes Read

`id`, `organization_id`, `hostname`, `access_provider`, `state`, `status_message`, `monthly_cents`, `provisioned_at`, `token_set_at`, `claimed_domains` — the wire's mirror. The token value is never readable; `token_set_at` is the rotation clock (null when no save has stamped it — an older token can predate the stamp) and `claimed_domains` is the claim registry's readable half.

## Notes

- `destroy` removes the resource from state only: the API exposes NO gateway-delete route, so the platform keeps the gateway running (and billing). The destroy warning states this; and since the identity attributes are create-only, a replacement would apply into a guaranteed conflict — replacements are rejected at plan time. To actually remove a gateway, opt out via the panel's Access tab, then `terraform apply -replace=…` (or re-add).
- A `suspended` gateway is deliberately unmanaged — resume via the panel or MCP (`resume_gateway`); watch `state`/`status_message`.
- The token save is the claim event. A gateway can be created before any domain exists; the next verification or attach claims the new domain. An imported gateway has no `api_token` in state (write-only); set it plus a fresh `rotate_api_token_on` on the first apply that should manage the token.

## Import

`terraform import fluxvale_access_gateway.x <org_id_or_slug>`. The read fills the wire mirror and seeds `organization` from the resolution, so a config that leaves it unset converges. `auth_key`, `api_token`, and `rotate_api_token_on` stay null; `instances` stays null after import. A config value that doesn't match what's stored plans a replacement, and replacements are rejected at plan time.
