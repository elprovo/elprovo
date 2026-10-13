output "agent_address" {
  description = "Public address of the agent host, for the install over SSH."
  value       = azurerm_public_ip.agent.ip_address
}

output "gateway_address" {
  description = "Public address of the gateway host, for the install over SSH."
  value       = azurerm_public_ip.gateway.ip_address
}

output "gateway_ip" {
  description = "Stable address that preview host names point at."
  value       = azurerm_public_ip.gateway.ip_address
}

output "resource_group" {
  description = "Resource group that the agent creates preview machines in."
  value       = azurerm_resource_group.environment.name
}

output "location" {
  description = "Location of the environment."
  value       = azurerm_resource_group.environment.location
}

output "preview_subnet_id" {
  description = "Subnet that the agent puts preview machines in. Its network security group admits only the agent and the gateway."
  value       = azurerm_subnet.previews.id
}

output "preview_tag" {
  description = "Value of the elprovo tag that the agent must put on every machine it creates."
  value       = local.preview_tag
}

output "registry" {
  description = "Image name prefix that build workers push to."
  value       = azurerm_container_registry.elprovo.login_server
}

output "registry_id" {
  description = "Container registry that the agent makes short-lived token credentials for."
  value       = azurerm_container_registry.elprovo.id
}
