output "agent_address" {
  description = "Public address of the agent host, for the install over SSH."
  value       = hcloud_server.agent.ipv4_address
}

output "gateway_address" {
  description = "Public address of the gateway host, for the install over SSH."
  value       = hcloud_server.gateway.ipv4_address
}

output "gateway_ip" {
  description = "Stable address that stagelet host names point at."
  value       = hcloud_primary_ip.gateway.ip_address
}

output "stagelet_label" {
  description = "Label, as key=value, that the agent must put on every server it creates."
  value       = local.stagelet_label
}

output "machine_prefix" {
  description = "Prefix of the names of the servers the agent creates. Hetzner keeps server names unique in a project, so each environment has its own."
  value       = "stagely-${var.name}-"
}

output "network_id" {
  description = "Private network that the agent puts every server it creates on."
  value       = tostring(hcloud_network.environment.id)
}

output "agent_private_ip" {
  description = "Address of the agent host on the private network, from which it drives servers."
  value       = hcloud_server_network.agent.ip
}

output "gateway_private_ip" {
  description = "Address of the gateway host on the private network, from which it forwards visitors to stagelets."
  value       = hcloud_server_network.gateway.ip
}
