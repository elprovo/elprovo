terraform {
  required_providers {
    hcloud = {
      source  = "hetznercloud/hcloud"
      version = "1.69.0"
    }
  }
}

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

provider "hcloud" {
  endpoint = var.endpoint
}

resource "hcloud_network" "stagelets" {
  name     = var.name
  ip_range = "10.200.0.0/16"
}

resource "hcloud_network_subnet" "stagelets" {
  network_id   = hcloud_network.stagelets.id
  type         = "cloud"
  network_zone = "eu-central"
  ip_range     = "10.200.1.0/24"
}

resource "hcloud_ssh_key" "agent" {
  name       = var.name
  public_key = var.ssh_public_key
}

resource "hcloud_firewall" "stagelets" {
  name = var.name

  rule {
    direction  = "in"
    protocol   = "tcp"
    port       = "22"
    source_ips = [hcloud_network.stagelets.ip_range]
  }

  rule {
    direction  = "in"
    protocol   = "icmp"
    source_ips = ["0.0.0.0/0", "::/0"]
  }

  rule {
    direction       = "out"
    protocol        = "tcp"
    port            = "any"
    destination_ips = ["0.0.0.0/0", "::/0"]
  }

  rule {
    direction       = "out"
    protocol        = "udp"
    port            = "53"
    destination_ips = ["0.0.0.0/0"]
  }

  apply_to {
    label_selector = "stagely-check=${var.name}"
  }
}

resource "hcloud_primary_ip" "front" {
  name        = "${var.name}-front"
  type        = "ipv4"
  location    = "nbg1"
  auto_delete = false
}

resource "hcloud_server" "stagelet" {
  count       = 2
  name        = "${var.name}-${count.index}"
  server_type = "cpx22"
  image       = "ubuntu-24.04"
  location    = "nbg1"
  ssh_keys    = [hcloud_ssh_key.agent.id]
  labels      = { "stagely-check" = var.name }
  user_data   = <<-EOT
    #cloud-config
    write_files:
      - path: /etc/stagely-check
        content: "server ${count.index}\n"
  EOT

  public_net {
    ipv4_enabled = true
    ipv4         = count.index == 0 ? hcloud_primary_ip.front.id : null
    ipv6_enabled = false
  }

}

resource "hcloud_server_network" "stagelet" {
  count      = 2
  server_id  = hcloud_server.stagelet[count.index].id
  network_id = hcloud_network.stagelets.id
  depends_on = [hcloud_network_subnet.stagelets]
}

output "public_addresses" {
  value = hcloud_server.stagelet[*].ipv4_address
}

output "front_ip" {
  value = hcloud_primary_ip.front.ip_address
}

output "private_addresses" {
  value = hcloud_server_network.stagelet[*].ip
}
