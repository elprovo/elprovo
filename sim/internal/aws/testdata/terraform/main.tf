# Applies the AWS bootstrap module, which sim/scripts/check-aws.sh copies
# into ./module, against the simulation.

variable "endpoint" {
  type = string
}

variable "ssh_public_key" {
  type = string
}

variable "name" {
  type    = string
  default = "stagely-sim-check"
}

module "stagely" {
  source = "./module"

  endpoint       = var.endpoint
  name           = var.name
  region         = "us-east-1"
  ssh_public_key = var.ssh_public_key
  operator_cidrs = ["0.0.0.0/0"]

  agent_user_data   = <<-EOT
    #cloud-config
    write_files:
      - path: /etc/stagely-check
        content: "agent\n"
  EOT
  gateway_user_data = <<-EOT
    #cloud-config
    write_files:
      - path: /etc/stagely-check
        content: "gateway\n"
  EOT
}

output "agent_address" {
  value = module.stagely.agent_address
}

output "gateway_ip" {
  value = module.stagely.gateway_ip
}

output "registry" {
  value = module.stagely.registry
}

output "image_id" {
  value = module.stagely.image_id
}
