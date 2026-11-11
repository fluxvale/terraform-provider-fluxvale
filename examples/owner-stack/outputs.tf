output "instance_urls" {
  description = "Edge URL per public instance (the platform's wildcard domain: https://<slug>.fluxvale.app). Private instances answer through the gateway instead."
  value = {
    for k, i in fluxvale_instance.apps : k => "https://${i.slug}.fluxvale.app"
    if i.exposure == "public"
  }
}

output "gateway_host" {
  description = "The gateway's MagicDNS name — the private instances answer through it (per-instance addresses: the panel's Access tab)."
  value       = "${fluxvale_access_gateway.owner.hostname}.${var.tailnet_domain}"
}

output "instance_ids" {
  description = "Platform ids per instance — also the terraform import arguments."
  value       = { for k, i in fluxvale_instance.apps : k => i.id }
}

output "domain_states" {
  description = "Verification state per registered custom domain (verified = issuance, NOT resolution — personal-stack's README). Empty until the dogfood pairings land."
  value       = { for k, d in fluxvale_custom_domain.apps : k => d.state }
}

output "backups_bucket" {
  description = "The R2 bucket every instance's backups land in."
  value       = cloudflare_r2_bucket.backups.name
}
