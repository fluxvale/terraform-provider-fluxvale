# The owner's production stack as one root module (#482's rider): one
# R2 bucket feeding every instance's backups, the tailnet access
# gateway for the private ones, the instances tying it together, and
# the dogfood custom-domain pairings carried as placeholders.
# SKELETON — placeholder variables only. The real values arrive at the
# pairing step (README.md) and live in a gitignored terraform.tfvars;
# nothing applies until the owner fills them in and runs it himself.

terraform {
  required_version = ">= 1.6.0" # optional()/one() want >= 1.4; 1.6 is the tested floor

  required_providers {
    fluxvale = {
      source  = "registry.terraform.io/fluxvale/fluxvale"
      version = "~> 0.1" # not on any registry — installed per docs/install.md
    }
    cloudflare = {
      source  = "cloudflare/cloudflare"
      version = "~> 5.0" # current major at the time of writing
    }
  }
}

provider "fluxvale" {
  api_url = var.api_url
  # THIS ATTRIBUTE WINS over the FLUXVALE_PAT environment variable,
  # which is the fallback when it's null — the preferred shape (no
  # token in any file). Never hardcode.
  token = var.fluxvale_pat
  # No "organization": the owner's token belongs to his sole
  # organization, so every resource resolves it; with several
  # memberships the provider errors, never picks.
}

provider "cloudflare" {
  api_token = var.cloudflare_api_token
}

# R2: the backups destination. One bucket, per-instance prefixes —
# the access keys are account-level R2 tokens (no TF resource manages
# those) and arrive as vars. The composition — a TF-created bucket
# feeding fluxvale_instance_backups via plain interpolation — is the
# point of this module.
resource "cloudflare_r2_bucket" "backups" {
  account_id = var.cloudflare_account_id
  name       = var.r2_bucket_name
}

# The catalog, for the pairing step: `tofu console` then
# [for a in data.fluxvale_apps.catalog.apps : a.slug] lists what
# var.instances.app may name (README.md).
data "fluxvale_apps" "catalog" {}

# One version list per distinct app slug; the locals below resolve
# each instance's version string against it.
data "fluxvale_app_versions" "all" {
  for_each = toset([for k, v in var.instances : v.app])
  app      = each.value
}

locals {
  # The deploy pin, per instance. Deliberately explicit — the
  # catalog's version rows come back in a version-STRING sort, not a
  # release order (3.4.1-bundled sorts above 3.4.1; 9.x would sort
  # above 10.x), so "first row" means nothing. Bumping the pin is the
  # upgrade path, and a pin change replaces the instance (create-only
  # identity) — bump deliberately.
  versions = { for k, v in var.instances : k => v.version }
}

resource "fluxvale_instance" "apps" {
  for_each = var.instances

  name = each.key
  # The precondition below refuses an unknown version at plan, before
  # anything is created; one() then resolves the pin to its catalog
  # app_version_id (versions are unique per app server-side).
  app_version_id = one([
    for av in data.fluxvale_app_versions.all[each.value.app].app_versions :
    av.id if av.version == local.versions[each.key]
  ])

  cpu_cores  = each.value.cpu_cores
  memory_mb  = each.value.memory_mb
  storage_gb = each.value.storage_gb

  exposure  = each.value.exposure
  always_on = each.value.always_on
  running   = each.value.running

  # Write-only (kept verbatim in state; the API never echoes it).
  env_vars = each.value.env_vars

  lifecycle {
    precondition {
      condition = contains([
        for av in data.fluxvale_app_versions.all[each.value.app].app_versions :
        av.version
      ], local.versions[each.key])
      error_message = "Version ${local.versions[each.key]} of ${each.value.app} is not in the catalog — pick from the catalog listing (README.md)."
    }
  }
}

# Every instance gets a backups row in the bucket above: same
# endpoint (composed from the account id), same keys, its own prefix.
# secret_access_key is write-only — set means applied (or rotated)
# with each save. The org must have paid once for backups (the
# platform's paywall; a 403 here means the wallet, not a bug).
resource "fluxvale_instance_backups" "apps" {
  for_each = var.instances

  instance_id       = fluxvale_instance.apps[each.key].id
  endpoint          = "https://${var.cloudflare_account_id}.r2.cloudflarestorage.com"
  bucket            = cloudflare_r2_bucket.backups.name
  access_key_id     = var.r2_access_key_id
  secret_access_key = var.r2_secret_access_key
  prefix            = "fluxvale/${each.key}"
  retention_keep    = each.value.retention_keep
}

# Custom-domain registrations, for the dogfood pairings: setting an
# instance's `domain` (var.instances) registers the NAME on the
# private-access edge. That is all the platform can express today —
# there is no instance-domain attachment (instance hostnames mint
# only under the platform domain), so a registered row serves
# NOTHING yet; linking names to instances is the dogfood plan's to
# build. The skeleton's defaults leave every domain null — forgejo →
# git.fluxvale.com and umami → analytics.fluxvale.com ride as
# documented placeholders (terraform.tfvars.example), pending that
# plan, which owns the domain/OIDC/money-gate decisions. Registration
# passes the org-wallet gate, and the create waits out DNS-01 (45m) —
# see examples/personal-stack's custom-domain.tf for the full notes.
resource "fluxvale_custom_domain" "apps" {
  for_each = { for k, v in var.instances : k => v if v.domain != null }

  domain = each.value.domain

  wait_for_verification = true

  timeouts {
    create = "45m"
  }
}

# The org's private access: the gateway joins the owner's tailnet and
# attaches every private instance (the attach waits the gateway to
# :active). The gateway is an org singleton that bills monthly, needs
# the org wallet paid AND funded (its create's paywall), and has NO
# API delete — destroy removes it from state only; opt out via the
# panel's Access tab. It is unconditional here: even with zero
# private instances it still creates and bills — remove this resource
# to skip it. If the org already has one, ADOPT it: set
# tailscale_auth_key null in terraform.tfvars first (any auth_key
# value after an import plans a replacement, and replacements are
# rejected at plan time), then `terraform import
# fluxvale_access_gateway.owner <org_id_or_slug>`.
resource "fluxvale_access_gateway" "owner" {
  tailnet_domain = var.tailnet_domain
  auth_key       = var.tailscale_auth_key

  # Uncomment once the gateway exists (org slug or id): an unset
  # organization re-plans unknown against the stored id and shows a
  # spurious replacement every plan — see the README and the
  # provider issue linked there.
  # organization = "your-org-slug"

  instances = [
    for k, i in fluxvale_instance.apps : i.id
    if i.exposure == "private"
  ]
}
