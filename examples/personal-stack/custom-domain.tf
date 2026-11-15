resource "fluxvale_custom_domain" "personal" {
  domain = var.custom_domain

  # Wait out verification: DNS-01 against the `_acme-challenge` CNAME
  # from data.fluxvale_domain_delegation (see examples/cloudflare-stack;
  # the private lane needs no host record), promoted pending → verified
  # by the platform's ~15-minute observer cadence (timeouts.create
  # bounds the wait). Verified is issuance; the gateway's api_token save
  # (or a later verification/attach) is the claim that makes the name
  # answer on your tailnet. See this example's README.
  wait_for_verification = true

  timeouts {
    create = "45m"
  }
}

variable "custom_domain" {
  description = "The domain to register on the private-access edge (e.g. apps.example.com) — canonicalized server-side (trim + downcase)."
  type        = string
}

output "domain_state" {
  description = "The custom domain's verification state. verified = issuance, NOT resolution — see the README."
  value       = fluxvale_custom_domain.personal.state
}
