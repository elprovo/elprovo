terraform {
  required_providers {
    digitalocean = {
      source  = "digitalocean/digitalocean"
      version = "2.102.0"
    }
  }
}

variable "api_endpoint" {
  type = string
}

variable "token" {
  type      = string
  sensitive = true
}

variable "ssh_public_key" {
  type = string
}

variable "name" {
  type    = string
  default = "stagely-sim-check"
}

provider "digitalocean" {
  token        = var.token
  api_endpoint = var.api_endpoint
}

resource "digitalocean_vpc" "stagelets" {
  name     = var.name
  region   = "nyc3"
  ip_range = "10.200.0.0/24"
}

resource "digitalocean_ssh_key" "agent" {
  name       = var.name
  public_key = var.ssh_public_key
}

resource "digitalocean_tag" "stagelet" {
  name = var.name
}

resource "digitalocean_droplet" "stagelet" {
  count     = 2
  name      = "${var.name}-${count.index}"
  region    = "nyc3"
  size      = "s-1vcpu-1gb"
  image     = "ubuntu-24-04-x64"
  vpc_uuid  = digitalocean_vpc.stagelets.id
  ssh_keys  = [digitalocean_ssh_key.agent.fingerprint]
  tags      = [digitalocean_tag.stagelet.id]
  user_data = <<-EOT
    #cloud-config
    write_files:
      - path: /etc/stagely-check
        content: "droplet ${count.index}\n"
  EOT
}

resource "digitalocean_firewall" "stagelets" {
  name = var.name
  tags = [digitalocean_tag.stagelet.id]

  inbound_rule {
    protocol         = "tcp"
    port_range       = "22"
    source_addresses = [digitalocean_vpc.stagelets.ip_range]
  }

  inbound_rule {
    protocol    = "icmp"
    source_tags = [digitalocean_tag.stagelet.id]
  }

  outbound_rule {
    protocol              = "tcp"
    port_range            = "1-65535"
    destination_addresses = ["0.0.0.0/0", "::/0"]
  }

  outbound_rule {
    protocol              = "udp"
    port_range            = "all"
    destination_addresses = ["0.0.0.0/0", "::/0"]
  }
}

resource "digitalocean_reserved_ip" "front" {
  region = "nyc3"
}

resource "digitalocean_reserved_ip_assignment" "front" {
  ip_address = digitalocean_reserved_ip.front.ip_address
  droplet_id = digitalocean_droplet.stagelet[0].id
}

resource "digitalocean_container_registry" "images" {
  name                   = var.name
  subscription_tier_slug = "basic"
  region                 = "fra1"
}

resource "digitalocean_container_registry_docker_credentials" "images" {
  registry_name = digitalocean_container_registry.images.name
  write         = true
}

output "public_addresses" {
  value = digitalocean_droplet.stagelet[*].ipv4_address
}

output "reserved_ip" {
  value = digitalocean_reserved_ip.front.ip_address
}

output "private_addresses" {
  value = digitalocean_droplet.stagelet[*].ipv4_address_private
}
