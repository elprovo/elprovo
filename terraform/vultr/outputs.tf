output "agent_address" {
  description = "Public address of the agent host, for the install over SSH."
  value       = vultr_instance.agent.main_ip
}

output "gateway_address" {
  description = "Public address of the gateway host, for the install over SSH."
  value       = vultr_instance.gateway.main_ip
}

output "gateway_ip" {
  description = "Stable address that stagelet host names point at."
  value       = vultr_reserved_ip.gateway.subnet
}

output "stagelet_firewall_group_id" {
  description = "Firewall group the agent puts every machine it creates in."
  value       = vultr_firewall_group.stagelets.id
}

output "stagelet_tag" {
  description = "Tag the agent must put on every machine it creates."
  value       = local.stagelet_tag
}

output "registry" {
  description = "Image name prefix that build workers push to."
  value       = vultr_container_registry.stagely.urn
}

output "registry_id" {
  description = "Registry that the agent gets short-lived Docker credentials for, for each machine."
  value       = vultr_container_registry.stagely.id
}

output "vpc_id" {
  description = "VPC that the agent puts every machine it creates in."
  value       = vultr_vpc.environment.id
}

output "agent_private_ip" {
  description = "Address of the agent host in the VPC, from which it drives machines."
  value       = vultr_instance.agent.internal_ip
}

output "gateway_private_ip" {
  description = "Address of the gateway host in the VPC, from which it forwards visitors to stagelets."
  value       = vultr_instance.gateway.internal_ip
}
