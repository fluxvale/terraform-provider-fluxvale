# The owner's personal stack (#482's acceptance): a public app, a
# private app behind the access gateway, one R2 bucket feeding both
# instances' backups, and the org's custom domain. PLACEHOLDERS ONLY —
# the PA collects the real values (terraform.tfvars.example →
# terraform.tfvars) and runs the acceptance plan/apply/destroy against
# the real org; nothing real belongs in git.

terraform {
  required_providers {
    fluxvale = {
      source = "registry.terraform.io/fluxvale/fluxvale"
    }
    cloudflare = {
      source  = "cloudflare/cloudflare"
      version = "~> 5.0" # current major at the time of writing
    }
  }
}

provider "fluxvale" {
  api_url = var.api_url
  # Precedence, per the provider's Configure: THIS ATTRIBUTE WINS, and
  # the FLUXVALE_PAT environment variable is the fallback when it's
  # unset/null — so the preferred shape is leaving it null and
  # exporting FLUXVALE_PAT (no secrets in files). Never hardcode.
  token = var.fluxvale_pat
  # No "organization" here: the token belongs to the owner's sole
  # organization, so every resource resolves it — naming it is only
  # needed with several memberships (the provider errors, never picks).
}

provider "cloudflare" {
  api_token = var.cloudflare_api_token
}

# R2: the backups' destination. The bucket is created here; the access
# keys are account-level R2 tokens (no TF resource for those) — they
# arrive as vars, and the backups resources below consume the bucket's
# real name via interpolation. That wiring — TF-created R2 feeding
# fluxvale_instance_backups — is the composition this example exists to
# prove.
resource "cloudflare_r2_bucket" "backups" {
  account_id = var.cloudflare_account_id
  name       = var.r2_bucket_name
}

# Catalog selection by slug: the apps must exist (plan refuses here,
# before anything is created), and each version var must match a
# catalog version string. The resolved ids feed the instances.
locals {
  public_app      = one([for a in data.fluxvale_apps.catalog.apps : a if a.slug == var.public_app_slug])
  private_app     = one([for a in data.fluxvale_apps.catalog.apps : a if a.slug == var.private_app_slug])
  public_version  = one([for v in data.fluxvale_app_versions.public.app_versions : v if v.version == var.public_app_version])
  private_version = one([for v in data.fluxvale_app_versions.private.app_versions : v if v.version == var.private_app_version])
}

data "fluxvale_apps" "catalog" {}

data "fluxvale_app_versions" "public" {
  app = var.public_app_slug
}

data "fluxvale_app_versions" "private" {
  app = var.private_app_slug
}

# The public instance: bigger box, always-on (a forge must not sleep
# mid-push), default public exposure.
resource "fluxvale_instance" "public" {
  name           = "forgejo"
  app_version_id = local.public_version.id
  cpu_cores      = 1.0
  memory_mb      = 1024
  storage_gb     = 5
  always_on      = true
  running        = true
}

# The private instance: smaller box, private exposure — reachable only
# through the gateway below (its `instances` set attaches it and waits
# the gateway to :active before the attach call).
resource "fluxvale_instance" "private" {
  name           = "memos"
  app_version_id = local.private_version.id
  cpu_cores      = 0.5
  memory_mb      = 512
  storage_gb     = 2
  exposure       = "private"
}

resource "fluxvale_access_gateway" "personal" {
  # The gateway joins YOUR tailnet: the domain is the tailnet's
  # MagicDNS suffix, auth_key is a Tailscale auth key (write-only —
  # the API never echoes it; it is required to ENABLE the gateway).
  tailnet_domain = var.tailnet_domain
  auth_key       = var.tailscale_auth_key
  instances      = [fluxvale_instance.private.id]
}

# Backups, instance A — real-looking wiring: the endpoint composes from
# the R2 account id, the bucket name rides the TF-created bucket.
resource "fluxvale_instance_backups" "public" {
  instance_id       = fluxvale_instance.public.id
  endpoint          = "https://${var.cloudflare_account_id}.r2.cloudflarestorage.com"
  bucket            = cloudflare_r2_bucket.backups.name
  access_key_id     = var.r2_access_key_id
  secret_access_key = var.r2_secret_access_key
  prefix            = "fluxvale/forgejo"
  retention_keep    = 14
}

# Backups, instance B — same bucket, its own prefix, the server's
# default retention.
resource "fluxvale_instance_backups" "private" {
  instance_id       = fluxvale_instance.private.id
  endpoint          = "https://${var.cloudflare_account_id}.r2.cloudflarestorage.com"
  bucket            = cloudflare_r2_bucket.backups.name
  access_key_id     = var.r2_access_key_id
  secret_access_key = var.r2_secret_access_key
  prefix            = "fluxvale/memos"
}
