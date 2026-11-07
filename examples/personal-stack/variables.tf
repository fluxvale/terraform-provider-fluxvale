variable "api_url" {
  description = "FluxVale API base URL. Default is production; point at staging while rehearsing."
  type        = string
  default     = "https://fluxvale.com"
}

variable "fluxvale_pat" {
  description = "FluxVale PAT (fvpat_…). Leave null: FLUXVALE_PAT in the environment is preferred."
  type        = string
  sensitive   = true
  default     = null
}

variable "cloudflare_api_token" {
  description = "Cloudflare API token with R2 write on the account (creates the backups bucket)."
  type        = string
  sensitive   = true
}

variable "cloudflare_account_id" {
  description = "The Cloudflare account id — also composes the R2 S3 endpoint."
  type        = string
}

variable "r2_bucket_name" {
  description = "Name of the R2 bucket both instances' backups land in."
  type        = string
}

variable "r2_access_key_id" {
  description = "R2 access key id (an account-level R2 token's credential)."
  type        = string
}

variable "r2_secret_access_key" {
  description = "R2 secret access key — write-only to the API (set ⇒ rotated with each save)."
  type        = string
  sensitive   = true
}

variable "public_app_slug" {
  description = "The public instance's catalog app slug (must exist — plan refuses unknown slugs)."
  type        = string
}

variable "public_app_version" {
  description = "The public instance's version string (selected from the catalog by the app_versions data source)."
  type        = string
}

variable "private_app_slug" {
  description = "The private instance's catalog app slug."
  type        = string
}

variable "private_app_version" {
  description = "The private instance's version string."
  type        = string
}

variable "tailnet_domain" {
  description = "Your tailnet's MagicDNS suffix (e.g. your-tailnet.ts.net — see the Tailscale admin console)."
  type        = string
}

variable "tailscale_auth_key" {
  description = "Tailscale auth key the gateway joins the tailnet with (write-only; required to enable)."
  type        = string
  sensitive   = true
}
