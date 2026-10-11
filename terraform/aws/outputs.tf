output "agent_address" {
  description = "Public address of the agent host, for the install over SSH."
  value       = aws_instance.agent.public_ip
}

output "gateway_address" {
  description = "Public address of the gateway host, for the install over SSH: its Elastic IP."
  value       = aws_eip.gateway.public_ip
}

output "gateway_ip" {
  description = "Stable address that stagelet host names point at."
  value       = aws_eip.gateway.public_ip
}

output "subnet_id" {
  description = "Subnet that the agent puts stagelet machines in."
  value       = aws_subnet.environment.id
}

output "stagelet_security_group_id" {
  description = "Security group that the agent puts on every machine it creates."
  value       = aws_security_group.stagelets.id
}

output "stagelet_group" {
  description = "Value of the stagely-group tag the agent must put on every machine it creates."
  value       = local.stagelet_group
}

output "image_id" {
  description = "AMI that stagelet machines boot without a golden image."
  value       = local.image
}

output "registry" {
  description = "Image name prefix that build workers push to."
  value       = "${data.aws_caller_identity.current.account_id}.dkr.ecr.${var.region}.amazonaws.com/${local.registry_prefix}"
}
