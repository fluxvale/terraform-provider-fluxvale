# The private instance's own address under the verified domain: a
# label minted as one platform row. Serving is the platform's rule —
# domain verified, gateway claimed, instance private and attached
# (see the custom-domain notes; this example's gateway attaches
# fluxvale_instance.private). The address is unique platform-wide.

resource "fluxvale_instance_hostname" "private" {
  instance_id      = fluxvale_instance.private.id
  access_domain_id = fluxvale_custom_domain.personal.id
  label            = var.custom_address_label
}

variable "custom_address_label" {
  description = "The label below the custom domain (e.g. \"memos\" → memos.<custom_domain>). Lowercase alnum + inner hyphens, 1-63 chars — the platform's grammar."
  type        = string
  default     = "memos"
}

output "custom_address" {
  description = "The composed address — the resource carries no FQDN attribute by design (the row is only its identity and where it sits)."
  value       = "${fluxvale_instance_hostname.private.label}.${fluxvale_custom_domain.personal.domain}"
}
