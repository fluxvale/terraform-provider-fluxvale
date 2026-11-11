# fluxvale_access_gateway

The org's singleton private-access gateway.

The API's gateway surface is create, the singleton read, the attach/detach verbs, and `PATCH /access-gateways/:id/resume`; the provider deliberately does not expose resume — a suspended gateway is the zero-balance ladder's out-of-band platform state (resume via the panel or MCP, watch `state`), not something to flip from a config file.

## Example Usage

```hcl
resource "fluxvale_access_gateway" "org" {
  tailnet_domain      = var.tailnet_domain
  tailscale_auth_key  = var.tailscale_auth_key
  instances           = [fluxvale_instance.memos.id]
}
```

## Notes

- `destroy` removes the resource from state only: the API exposes NO gateway-delete route, so the platform keeps the gateway running (and billing). The destroy warning states this; and since the identity attributes are create-only, a replacement would apply into a guaranteed conflict — replacements are rejected at plan time. To actually remove a gateway, opt out via the panel's Access tab, then `terraform apply -replace=…` (or re-add).
- `instances` (a set of instance ids) converges via the attach/detach verbs — one call per added/removed id. Attaching waits the gateway to `active` (create lands `provisioning`; the observer promotes within about a minute). The set is config-authoritative: the API has no attachment list, so out-of-band panel attach/detach is invisible to the provider.
- A `suspended` gateway is deliberately unmanaged — resume via the panel or MCP (`resume_gateway`); watch `state`/`status_message`.

## Import

`terraform import fluxvale_access_gateway.x <org_id_or_slug>`. The read fills the wire mirror and seeds `organization` from the resolution, so a config that leaves it unset converges. `auth_key` is Optional (required only when this resource ENABLES a gateway — the wire never echoes it, so imports manage attachments with it null) and `instances` stays null after import; a config value that doesn't match what's stored plans a replacement, and replacements are rejected at plan time.
