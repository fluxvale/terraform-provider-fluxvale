variable "api_url" {
  description = "FluxVale API base URL. Default https://fluxvale.com; staging is https://staging.fluxvale.com."
  type        = string
  default     = "https://fluxvale.com"
}

variable "org_name" {
  description = "The team organization's display name."
  type        = string
  default     = "Acme"
}

variable "org_slug" {
  description = "The organization's address — lowercase letters, digits, and hyphens. Reserved brand slugs are refused."
  type        = string
  default     = "acme-team"
}

variable "app_slug" {
  description = "Catalog app slug to deploy."
  type        = string
  default     = "forgejo"
}

variable "instance_name" {
  description = "The instance's display name."
  type        = string
  default     = "forgejo"
}

variable "cpu_cores" {
  description = "Instance CPU cores."
  type        = number
  default     = 0.5
}

variable "memory_mb" {
  description = "Instance memory in MB."
  type        = number
  default     = 512
}

variable "storage_gb" {
  description = "Instance storage in GB."
  type        = number
  default     = 5
}
