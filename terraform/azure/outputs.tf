output "agent_address" {
  description = "Public address of the agent host, for the install over SSH."
  value       = azurerm_public_ip.agent.ip_address
}

output "gateway_address" {
  description = "Public address of the gateway host, for the install over SSH."
  value       = azurerm_public_ip.gateway.ip_address
}

output "gateway_ip" {
  description = "Stable address that stagelet host names point at."
  value       = azurerm_public_ip.gateway.ip_address
}

output "resource_group" {
  description = "Resource group that the agent creates stagelet machines in."
  value       = azurerm_resource_group.environment.name
}

output "location" {
  description = "Location of the environment."
  value       = azurerm_resource_group.environment.location
}

output "stagelet_subnet_id" {
  description = "Subnet that the agent puts stagelet machines in. Its network security group admits only the agent and the gateway."
  value       = azurerm_subnet.stagelets.id
}

output "stagelet_tag" {
  description = "Value of the stagely tag that the agent must put on every machine it creates."
  value       = local.stagelet_tag
}

output "registry" {
  description = "Image name prefix that build workers push to."
  value       = azurerm_container_registry.stagely.login_server
}

output "registry_id" {
  description = "Container registry that the agent makes short-lived token credentials for."
  value       = azurerm_container_registry.stagely.id
}
