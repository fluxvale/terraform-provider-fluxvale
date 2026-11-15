terraform {
  required_version = ">= 1.12.0" # OpenTofu: >= 1.13.0 (write-only api_token)

  required_providers {
    fluxvale = {
      source = "registry.terraform.io/fluxvale/fluxvale"
      # fluxvale_domain_delegation and the write-only api_token first
      # ship in the release after 0.4.0 — pin that tag once it exists.
      version = ">= 0.4"
    }
    cloudflare = {
      source  = "cloudflare/cloudflare"
      version = "~> 5.0"
    }
  }
}

# The FluxVale token comes from FLUXVALE_PAT — never a .tf value.
provider "fluxvale" {
  api_url = var.api_url
}

# The Cloudflare token comes from CLOUDFLARE_API_TOKEN — never a .tf
# value. It needs Zone:Read and DNS:Edit on the zone below.
provider "cloudflare" {}

data "cloudflare_zone" "this" {
  filter = {
    name = var.zone_name
  }
}

# The pre-registration delegation preview. It is deliberately a data
# source: the platform API keeps a computed mirror for the panel, but
# the provider resource does not expose it, because Terraform does not
# publish a resource's computed values until its create returns — and
# the domain's create waits for the DNS-01 challenge this record
# answers. The data source is row-free, so the graph is acyclic and one
# apply works.
data "fluxvale_domain_delegation" "apps" {
  domain = var.custom_domain
}

resource "cloudflare_dns_record" "challenge" {
  zone_id = data.cloudflare_zone.this.zone_id
  name    = data.fluxvale_domain_delegation.apps.dns_records[0].name
  type    = data.fluxvale_domain_delegation.apps.dns_records[0].type
  content = data.fluxvale_domain_delegation.apps.dns_records[0].value

  # A proxied CNAME breaks the delegation: the CA must follow the CNAME
  # to the platform's validation namespace and read the TXT there.
  proxied = false
  ttl     = 300
}

resource "fluxvale_custom_domain" "apps" {
  domain                = var.custom_domain
  wait_for_verification = true

  timeouts {
    create = "45m"
  }

  depends_on = [cloudflare_dns_record.challenge]
}

# The gateway token is write-only: it never enters state or plan. The
# save attempts to claim every verified domain in the organization,
# which is why this resource waits for the domain above; read
# claimed_domains for the result. `rotate_api_token_on` is the rotation
# signal — bump it whenever the token changes.
resource "fluxvale_access_gateway" "gateway" {
  tailnet_domain = var.tailnet_domain
  auth_key       = var.tailscale_auth_key

  api_token           = var.tailscale_api_token
  rotate_api_token_on = var.tailscale_api_token_version

  depends_on = [fluxvale_custom_domain.apps]
}
