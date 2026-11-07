output "public_instance_url" {
  description = "The public instance's edge URL — the platform's wildcard domain (instances_base_domain, default fluxvale.app) with the instance slug."
  value       = "https://${fluxvale_instance.public.slug}.fluxvale.app"
}

output "private_instance_slug" {
  description = "The private instance's slug — private exposure answers only through the gateway."
  value       = fluxvale_instance.private.slug
}

output "gateway_tailnet_host" {
  description = "The gateway's MagicDNS name — the private instance is reachable via the gateway at <hostname>.<tailnet_domain>."
  value       = "${fluxvale_access_gateway.personal.hostname}.${var.tailnet_domain}"
}

output "instance_ids" {
  description = "Both instances' platform ids (also the terraform import arguments)."
  value = {
    public  = fluxvale_instance.public.id
    private = fluxvale_instance.private.id
  }
}
