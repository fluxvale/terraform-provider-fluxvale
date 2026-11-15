output "delegation_records" {
  description = "The DNS records the domain needs at its DNS host (one _acme-challenge CNAME in the private lane)."
  value       = data.fluxvale_domain_delegation.apps.dns_records
}

output "domain_state" {
  description = "verified means the wildcard Certificate issued; claimed_domains below is the resolution signal."
  value       = fluxvale_custom_domain.apps.state
}

output "claimed_domains" {
  description = "The custom domains the gateway currently claims on the tailnet (empty until the claim lands)."
  value       = fluxvale_access_gateway.gateway.claimed_domains
}

output "token_set_at" {
  description = "When the Tailscale API token was last saved — watch this and rotate before the key's 90-day expiry."
  value       = fluxvale_access_gateway.gateway.token_set_at
}
