variable "api_url" {
  description = "FluxVale API origin. Use https://staging.fluxvale.com for staging; the default is production."
  type        = string
  default     = "https://fluxvale.com"
}

variable "zone_name" {
  description = "The Cloudflare zone that holds the domain (e.g. example.com)."
  type        = string
}

variable "custom_domain" {
  description = "The subzone to register (e.g. apps.example.com). Names are unique platform-wide."
  type        = string
}

variable "tailnet_domain" {
  description = "Your tailnet's MagicDNS suffix (e.g. example.ts.net)."
  type        = string
}

variable "tailscale_auth_key" {
  description = "Tailscale auth key used to enable the gateway (only when creating one; omit on an adopted gateway)."
  type        = string
  sensitive   = true
  default     = null
}

variable "tailscale_api_token" {
  description = "Tailscale API access token with dns, devices:routes, and devices:core:read scopes. Pass as TF_VAR_tailscale_api_token from an ephemeral variable; it is write-only."
  type        = string
  sensitive   = true
  default     = null
}

variable "tailscale_api_token_version" {
  description = "Rotation signal for tailscale_api_token — bump it whenever the token changes."
  type        = string
  default     = "1"
}
