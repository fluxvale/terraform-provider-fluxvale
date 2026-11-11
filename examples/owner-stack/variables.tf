# Every value the owner supplies, in one file. Required variables
# carry no default on purpose: `tofu plan` with nothing filled in
# errors on exactly them — that error list IS the pairing checklist
# (README.md has the where-to-mint column). Choices with a sensible
# default (the instances, the bucket name) are overridable from
# terraform.tfvars.

variable "api_url" {
  description = "FluxVale API base URL. Production by default; point at staging to rehearse."
  type        = string
  default     = "https://fluxvale.com"
}

variable "fluxvale_pat" {
  description = "FluxVale PAT (fvpat_…). Leave null and export FLUXVALE_PAT — the token never belongs in a file."
  type        = string
  sensitive   = true
  default     = null
}

# ── Cloudflare / R2 ──────────────────────────────────────────────────────

variable "cloudflare_api_token" {
  description = "Cloudflare API token with R2 edit on the account (creates the backups bucket)."
  type        = string
  sensitive   = true
}

variable "cloudflare_account_id" {
  description = "Cloudflare account id — also composes the R2 S3 endpoint in main.tf."
  type        = string
}

variable "r2_bucket_name" {
  description = "The R2 bucket every instance's backups land in (per-instance prefixes keep them apart)."
  type        = string
  default     = "fluxvale-backups"
}

variable "r2_access_key_id" {
  description = "R2 access key id — an account-level R2 token's credential, minted in the Cloudflare dashboard (no TF resource manages it)."
  type        = string
}

variable "r2_secret_access_key" {
  description = "R2 secret access key — write-only to the platform API (set means rotated with each save)."
  type        = string
  sensitive   = true
}

# ── Tailscale ────────────────────────────────────────────────────────────

variable "tailnet_domain" {
  description = "The tailnet's MagicDNS suffix (<tailnet>.ts.net) — the Tailscale admin console shows it on every device name."
  type        = string
}

variable "tailscale_auth_key" {
  description = "Tailscale auth key the gateway joins the tailnet with (write-only; required to ENABLE the gateway). Null when ADOPTING an existing gateway by import — any value here after an import plans a replacement, and gateway replacements are rejected."
  type        = string
  sensitive   = true
  default     = null
}

# ── The stack itself ─────────────────────────────────────────────────────

variable "instances" {
  description = <<-EOT
    The instances, keyed by instance name (the key is the name and the backups prefix). Defaults are three representative shapes: a public always-on forgejo, memos and kavita private behind the gateway. Override the whole map in terraform.tfvars to differ.
    version: the catalog version to deploy, pinned explicitly — the catalog's row order is a version-string sort, not a release order, so there is no "newest row" to track. Bumping the pin replaces the instance (create-only identity) and the replacement's teardown deletes the app-data volume — restore from the R2 backups or lose the data.
    retention_keep: days of backups per instance; null is the server's default.
    env_vars: write-only environment for the instance, kept verbatim in state (the API never echoes it). Changing it REPLACES the instance — the API accepts env vars only at create — and the replacement's teardown deletes the app-data volume, same as a pin bump. The catalog marks some app secrets required — umami's 3.4.1-bundled needs APP_SECRET — without which the app runs but stays broken.
    domain: registers a fluxvale_custom_domain for the instance when set — the dogfood pairings (forgejo → git.fluxvale.com, umami → analytics.fluxvale.com) are pending the dogfood plan; the null default registers nothing.
  EOT
  type = map(object({
    app            = string
    version        = string
    cpu_cores      = number
    memory_mb      = number
    storage_gb     = number
    exposure       = string
    always_on      = bool
    running        = bool
    retention_keep = optional(number)
    env_vars       = optional(map(string))
    domain         = optional(string)
  }))
  # The skeleton's three shapes, pinned to catalog versions at the
  # time of writing — every domain null on purpose: an apply
  # registers no custom domain until the dogfood plan's pairings are
  # filled in (see terraform.tfvars.example).
  default = {
    forgejo = { app = "forgejo", version = "16.0.5", cpu_cores = 1.0, memory_mb = 2048, storage_gb = 10, exposure = "public", always_on = true, running = true, retention_keep = 14, env_vars = null, domain = null }
    memos   = { app = "memos", version = "26.11.0", cpu_cores = 0.25, memory_mb = 512, storage_gb = 5, exposure = "private", always_on = false, running = true, retention_keep = null, env_vars = null, domain = null }
    kavita  = { app = "kavita", version = "0.9.1", cpu_cores = 0.5, memory_mb = 512, storage_gb = 10, exposure = "private", always_on = false, running = true, retention_keep = null, env_vars = null, domain = null }
  }
  validation {
    condition     = alltrue([for k, v in var.instances : contains(["public", "private"], v.exposure)])
    error_message = "Each instance's exposure must be public or private."
  }
}
