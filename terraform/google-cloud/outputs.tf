output "agent_address" {
  description = "External address of the agent host, for the install over SSH."
  value       = google_compute_instance.agent.network_interface[0].access_config[0].nat_ip
}

output "gateway_address" {
  description = "External address of the gateway host, for the install over SSH."
  value       = google_compute_instance.gateway.network_interface[0].access_config[0].nat_ip
}

output "gateway_ip" {
  description = "Stable address that stagelet host names point at."
  value       = google_compute_address.gateway.address
}

output "subnetwork" {
  description = "Subnetwork that the agent puts stagelet machines in."
  value       = google_compute_subnetwork.environment.id
}

output "machine_prefix" {
  description = "Prefix of the names of the machines the agent creates. Compute Engine instance names are unique in a zone, so each environment gets its own."
  value       = "stagely-${var.name}-"
}

output "stagelet_tag" {
  description = "Network tag the agent must put on every machine it creates."
  value       = local.stagelet_tag
}

output "registry" {
  description = "Image name prefix that build workers push to."
  value       = "${google_artifact_registry_repository.images.location}-docker.pkg.dev/${var.project}/${google_artifact_registry_repository.images.repository_id}"
}

output "registry_push_account" {
  description = "Service account whose tokens build workers push with."
  value       = google_service_account.registry_push.email
}

output "registry_pull_account" {
  description = "Service account whose tokens stagelet machines pull with."
  value       = google_service_account.registry_pull.email
}
