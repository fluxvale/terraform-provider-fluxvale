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
  instances      = [fluxvale_instance.memos.id]

  ts_client_id               = var.tailscale_client_id
  ts_client_secret           = var.tailscale_client_secret
  rotate_ts_client_secret_on = var.tailscale_client_secret_version
}
```

## Enabling the gateway

A fresh create enables the gateway with exactly one enrollment credential — the OAuth client pair (`ts_client_id` + `ts_client_secret`, the preferred shape), `api_token`, or `auth_key`, never a mix (the API refuses any other combination with a 400 whose field-scoped sentence surfaces verbatim).

**The OAuth client pair (preferred).** Create an OAuth client in the Tailscale admin console (Settings → OAuth clients) carrying the scopes the platform needs — `auth_keys`, `dns`, `devices:routes` (write), and `devices:core:read` — scoped to the `fluxvale-gateway` tag. The tag pairing is load-bearing: the platform's minted join keys carry `tag:fluxvale-gateway`, and a mint by an OAuth-derived token must carry one of the client's tags, so the client's tags must include it (your tailnet's tagOwners must own it too, or the node cannot join). The platform exchanges the pair for a short-lived, tailnet-scoped token — the secret is never a direct Bearer — and mints the gateway's single-use, tagged, 1-hour join key from it; revocation is automatic on use. The client id is public identity: it is not sensitive and is kept in state for drift clarity. The secret is write-only — never in state or plan — so keep it in an environment variable (the owner stack exports `TF_VAR_tailscale_client_secret`, the `FLUXVALE_PAT` pattern).

**The alternates.** `api_token` (a console API access token, minted from a tailnet owner/admin account; console tokens are fully permitted — they carry the creating account's permissions, and scope selection belongs to trust credentials, not Keys-page tokens; keys expire within 90 days) enables the same mint. `auth_key` is the legacy fallback: mint a join key in the Tailscale admin console and paste it here.

**Owner runbook.** To rotate the client secret: generate a fresh secret (a new OAuth client if you moved clients), export it as `TF_VAR_tailscale_client_secret`, set it in the resource, and bump `rotate_ts_client_secret_on` — the pair re-stores in place through the credential PATCH, no replacement, the gateway keeps serving. Moving to a whole new OAuth client is the same gesture: set the new `ts_client_id` and secret together and bump the trigger. The write-only secret cannot produce a plan diff — the trigger is the only signal, and an unchanged trigger never re-sends even though the value is in the config on every apply. An imported gateway has no credential in state; set the pair (or token) plus a fresh trigger on the first apply that should manage it. To actually remove a gateway, opt out via the panel's Access tab — the API has no delete route (see Notes).

## Argument Reference

- `tailnet_domain` (Required) — the tailnet the gateway joins.
- `ts_client_id` (Optional) — the Tailscale OAuth client id, the preferred credential's public half. Kept verbatim in state (it is not a secret); set it together with `ts_client_secret` and no `api_token`/`auth_key`. Moving to a new client: set the new id + secret and bump `rotate_ts_client_secret_on`.
- `ts_client_secret` (Optional, Sensitive, Write-only) — the OAuth client's secret, shown once at client creation. The platform exchanges the pair and mints the join key from the exchanged token. It never enters state or plan and the API never echoes it. Requires Terraform 1.12+ or OpenTofu 1.13+ and a provider release containing this attribute; older CLIs leave it out and paste the pair in the panel.
- `rotate_ts_client_secret_on` (Optional) — the pair's rotation trigger, an arbitrary string (a date, a version, a nonce). Changing it re-sends `ts_client_id` and the current `ts_client_secret` in place through the credential PATCH; an unchanged value never re-sends, because a write-only value cannot produce a plan diff. Set `ts_client_secret` whenever this changes.
- `auth_key` (Optional, Sensitive) — the legacy join credential, the two-paste fallback. Set exactly one of the three shapes when this resource ENABLES a gateway (the wire never echoes it, so imports manage attachments with it null). Rotation has no API route (the panel's re-paste is the path), so changing it plans a replacement, and replacements are rejected.
- `api_token` (Optional, Sensitive, Write-only) — the console-token alternate: the Tailscale API access token the platform uses to claim verified custom domains — and, with no `auth_key`, to mint the gateway's join key. Mint it from a tailnet owner/admin account; keys expire within 90 days. It never enters state or plan. On create with no `auth_key` the platform mints the join key from it at create; otherwise it is sent once after the gateway reaches `active`, and the save attempts to claim every verified domain — read `claimed_domains` for the result. Bump `rotate_api_token_on` to send the current token again. Requires Terraform 1.12+ or OpenTofu 1.13+; older CLIs paste the token in the panel.
- `rotate_api_token_on` (Optional) — the token's rotation trigger, same mechanics as `rotate_ts_client_secret_on`.
- `organization` (Optional) — id or slug; falls back to the provider-level `organization`, then to your sole org. The singleton is resolved per org.
- `instances` (Optional, set of instance ids) — converges via the attach/detach verbs, one call per added/removed id. Attaching waits the gateway to `active` (create lands `provisioning`; the observer promotes within about a minute). The set is config-authoritative: the API has no attachment list, so out-of-band panel attach/detach is invisible to the provider.

## Attributes Read

`id`, `organization_id`, `hostname`, `access_provider`, `state`, `status_message`, `monthly_cents`, `provisioned_at`, `token_set_at`, `claimed_domains` — the wire's mirror. The credential values are never readable; `token_set_at` is the rotation clock (null when no save has stamped it — an older credential can predate the stamp) and `claimed_domains` is the claim registry's readable half.

## Notes

- `destroy` removes the resource from state only: the API exposes NO gateway-delete route, so the platform keeps the gateway running (and billing). The destroy warning states this; and since the identity attributes are create-only, a replacement would apply into a guaranteed conflict — replacements are rejected at plan time. To actually remove a gateway, opt out via the panel's Access tab, then `terraform apply -replace=…` (or re-add).
- A `suspended` gateway is deliberately unmanaged — resume via the panel or MCP (`resume_gateway`); watch `state`/`status_message`.
- A credential save is the claim event. A gateway can be created before any domain exists; the next verification or attach claims the new domain. On the minted paths (pair or token alone) the credential rides the create POST and the platform's observer claims once the node is up.

## Import

`terraform import fluxvale_access_gateway.x <org_id_or_slug>`. The read fills the wire mirror and seeds `organization` from the resolution, so a config that leaves it unset converges. `auth_key`, `ts_client_id`, `ts_client_secret`, `rotate_ts_client_secret_on`, `api_token`, and `rotate_api_token_on` stay null; `instances` stays null after import. A config value that doesn't match what's stored plans a replacement, and replacements are rejected at plan time.
