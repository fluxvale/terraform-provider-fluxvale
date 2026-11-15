# A team organization and its first app in one apply. The org is the
# ownership and billing boundary; the instance's organization reference
# is the dependency edge — the org exists before the instance, and
# Terraform destroys the instance before the org. PLACEHOLDERS ONLY:
# set the variables (terraform.tfvars.example → terraform.tfvars) and
# export FLUXVALE_PAT before applying.

terraform {
  required_providers {
    fluxvale = {
      source = "registry.terraform.io/fluxvale/fluxvale"
    }
  }
}

provider "fluxvale" {
  api_url = var.api_url
  # No token here: export FLUXVALE_PAT instead (no secrets in files).
  # No provider-level "organization": naming the managed org in the
  # provider block would make provider configuration depend on the
  # resource. Name it per resource below.
}

resource "fluxvale_organization" "team" {
  name = var.org_name
  slug = var.org_slug

  # An org is the root of everything you deploy. Keep it from an
  # accidental destroy: with this block set, `tofu destroy` fails at
  # plan time and nothing is destroyed. Remove it only when you mean to
  # tear the whole stack down.
  lifecycle {
    prevent_destroy = true
  }
}

data "fluxvale_app_versions" "app" {
  app = var.app_slug
}

resource "fluxvale_instance" "app" {
  organization   = fluxvale_organization.team.slug
  name           = var.instance_name
  app_version_id = data.fluxvale_app_versions.app.app_versions[0].id
  cpu_cores      = var.cpu_cores
  memory_mb      = var.memory_mb
  storage_gb     = var.storage_gb
}
