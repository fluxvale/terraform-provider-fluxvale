output "org_id" {
  description = "The created organization's id."
  value       = fluxvale_organization.team.id
}

output "org_slug" {
  description = "The organization's address."
  value       = fluxvale_organization.team.slug
}

output "instance_id" {
  description = "The instance's id."
  value       = fluxvale_instance.app.id
}

output "instance_url" {
  description = "The instance's public URL once deployed."
  value       = fluxvale_instance.app.url
}
