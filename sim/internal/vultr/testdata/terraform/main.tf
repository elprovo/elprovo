terraform {
  required_providers {
    vultr = {
      source  = "vultr/vultr"
      version = "2.32.0"
    }
  }
}

# The provider reads the API key from VULTR_API_KEY and always calls
# https://api.vultr.com, which the check resolves to the simulation.
provider "vultr" {}

variable "ssh_public_key" {
  type = string
}

variable "name" {
  type    = string
  default = "stagelysimcheck"
}

data "vultr_os" "ubuntu" {
  filter {
    name   = "name"
    values = ["Ubuntu 24.04 LTS x64"]
  }
}

resource "vultr_ssh_key" "agent" {
  name    = var.name
  ssh_key = var.ssh_public_key
}

resource "vultr_firewall_group" "stagelets" {
  description = var.name
}

resource "vultr_firewall_rule" "ssh" {
  firewall_group_id = vultr_firewall_group.stagelets.id
  protocol          = "tcp"
  ip_type           = "v4"
  subnet            = "203.0.113.0"
  subnet_size       = 24
  port              = "22"
  notes             = "operator"
}

resource "vultr_firewall_rule" "icmp" {
  firewall_group_id = vultr_firewall_group.stagelets.id
  protocol          = "icmp"
  ip_type           = "v4"
  subnet            = "0.0.0.0"
  subnet_size       = 0
}

resource "vultr_firewall_rule" "https_v6" {
  firewall_group_id = vultr_firewall_group.stagelets.id
  protocol          = "tcp"
  ip_type           = "v6"
  subnet            = "::"
  subnet_size       = 0
  port              = "443"
}

resource "vultr_instance" "stagelet" {
  count             = 2
  label             = "${var.name}-${count.index}"
  hostname          = "${var.name}-${count.index}"
  region            = "ewr"
  plan              = "vc2-1c-1gb"
  os_id             = data.vultr_os.ubuntu.id
  ssh_key_ids       = [vultr_ssh_key.agent.id]
  firewall_group_id = vultr_firewall_group.stagelets.id
  tags              = [var.name]
  user_data         = <<-EOT
    #cloud-config
    write_files:
      - path: /etc/stagely-check
        content: "instance ${count.index}\n"
  EOT
}

resource "vultr_reserved_ip" "front" {
  region      = "ewr"
  ip_type     = "v4"
  label       = var.name
  instance_id = vultr_instance.stagelet[0].id
}

resource "vultr_container_registry" "images" {
  name   = var.name
  region = "ewr"
  plan   = "start_up"
  public = false
}

output "main_ips" {
  value = vultr_instance.stagelet[*].main_ip
}

output "reserved_ip" {
  value = vultr_reserved_ip.front.subnet
}

output "registry" {
  value = vultr_container_registry.images.urn
}
