output "registry" {
  description = "Registry address as the VMs reach it, host:port."
  value       = "${var.host_address}:${var.registry_port}"
}

output "certificate_file" {
  description = "The gateway's certificate."
  value       = "${var.certificate_dir}/tls.crt"
}

output "key_file" {
  description = "The gateway's private key."
  value       = "${var.certificate_dir}/tls.key"
}
