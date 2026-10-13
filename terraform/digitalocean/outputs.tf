output "agent_address" {
  description = "Public address of the agent host, for the install over SSH."
  value       = digitalocean_droplet.agent.ipv4_address
}

output "gateway_address" {
  description = "Public address of the gateway host, for the install over SSH."
  value       = digitalocean_droplet.gateway.ipv4_address
}

output "gateway_ip" {
  description = "Stable address that preview host names point at."
  value       = digitalocean_reserved_ip.gateway.ip_address
}

output "vpc_id" {
  description = "VPC that the agent puts preview machines in."
  value       = digitalocean_vpc.environment.id
}

output "registry" {
  description = "Image name prefix that build workers push to."
  value       = "registry.digitalocean.com/${var.registry_name}"
}

output "preview_tag" {
  description = "Tag the agent must put on every machine it creates."
  value       = digitalocean_tag.preview.id
}

output "preview_domain" {
  description = "Domain that preview host names live under, when DNS is managed here."
  value       = var.dns_domain == "" ? "" : "${var.preview_subdomain}.${var.dns_domain}"
}
