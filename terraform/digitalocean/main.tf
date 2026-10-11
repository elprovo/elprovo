locals {
  stagelet_tag = var.stagelet_tag != "" ? var.stagelet_tag : "${var.name}-stagelet"
}

# Firewalls apply to tags, and DigitalOcean accepts a firewall only for tags
# that exist, so the module creates them before any droplet or firewall.
resource "digitalocean_tag" "agent" {
  name = "${var.name}-agent"
}

resource "digitalocean_tag" "gateway" {
  name = "${var.name}-gateway"
}

resource "digitalocean_tag" "stagelet" {
  name = local.stagelet_tag
}

resource "digitalocean_vpc" "environment" {
  name     = "${var.name}-stagely"
  region   = var.region
  ip_range = var.vpc_ip_range
}

resource "digitalocean_ssh_key" "operator" {
  name       = "${var.name}-stagely-setup"
  public_key = var.ssh_public_key
}

resource "digitalocean_droplet" "agent" {
  name      = "${var.name}-agent"
  region    = var.region
  size      = var.agent_size
  image     = var.image
  vpc_uuid  = digitalocean_vpc.environment.id
  ssh_keys  = [digitalocean_ssh_key.operator.fingerprint]
  tags      = [digitalocean_tag.agent.id]
  user_data = var.agent_user_data
}

resource "digitalocean_droplet" "gateway" {
  name      = "${var.name}-gateway"
  region    = var.region
  size      = var.gateway_size
  image     = var.image
  vpc_uuid  = digitalocean_vpc.environment.id
  ssh_keys  = [digitalocean_ssh_key.operator.fingerprint]
  tags      = [digitalocean_tag.gateway.id]
  user_data = var.gateway_user_data
}

resource "digitalocean_reserved_ip" "gateway" {
  region = var.region
}

resource "digitalocean_reserved_ip_assignment" "gateway" {
  ip_address = digitalocean_reserved_ip.gateway.ip_address
  droplet_id = digitalocean_droplet.gateway.id
}

# The agent accepts no inbound connections except SSH from the operator.
resource "digitalocean_firewall" "agent" {
  name = "${var.name}-agent"
  tags = [digitalocean_tag.agent.id]

  inbound_rule {
    protocol         = "tcp"
    port_range       = "22"
    source_addresses = var.operator_cidrs
  }

  outbound_rule {
    protocol              = "tcp"
    port_range            = "1-65535"
    destination_addresses = ["0.0.0.0/0", "::/0"]
  }

  outbound_rule {
    protocol              = "udp"
    port_range            = "1-65535"
    destination_addresses = ["0.0.0.0/0", "::/0"]
  }
}

# The gateway is the only host that accepts internet traffic.
resource "digitalocean_firewall" "gateway" {
  name = "${var.name}-gateway"
  tags = [digitalocean_tag.gateway.id]

  inbound_rule {
    protocol         = "tcp"
    port_range       = "80"
    source_addresses = ["0.0.0.0/0", "::/0"]
  }

  inbound_rule {
    protocol         = "tcp"
    port_range       = "443"
    source_addresses = ["0.0.0.0/0", "::/0"]
  }

  inbound_rule {
    protocol         = "tcp"
    port_range       = "22"
    source_addresses = var.operator_cidrs
  }

  outbound_rule {
    protocol              = "tcp"
    port_range            = "1-65535"
    destination_addresses = ["0.0.0.0/0", "::/0"]
  }

  outbound_rule {
    protocol              = "udp"
    port_range            = "1-65535"
    destination_addresses = ["0.0.0.0/0", "::/0"]
  }
}

# Stagelet machines and build workers accept traffic only from the agent,
# which drives them over SSH and checks health, and from the gateway.
resource "digitalocean_firewall" "stagelets" {
  name = "${var.name}-stagelets"
  tags = [digitalocean_tag.stagelet.id]

  inbound_rule {
    protocol           = "tcp"
    port_range         = "1-65535"
    source_droplet_ids = [digitalocean_droplet.agent.id, digitalocean_droplet.gateway.id]
  }

  outbound_rule {
    protocol              = "tcp"
    port_range            = "1-65535"
    destination_addresses = ["0.0.0.0/0", "::/0"]
  }

  outbound_rule {
    protocol              = "udp"
    port_range            = "1-65535"
    destination_addresses = ["0.0.0.0/0", "::/0"]
  }
}

resource "digitalocean_container_registry" "stagely" {
  count                  = var.create_registry ? 1 : 0
  name                   = var.registry_name
  subscription_tier_slug = var.registry_tier
  region                 = var.region
}

resource "digitalocean_record" "stagelets" {
  count  = var.dns_domain == "" ? 0 : 1
  domain = var.dns_domain
  type   = "A"
  name   = "*.${var.stagelet_subdomain}"
  value  = digitalocean_reserved_ip.gateway.ip_address
  ttl    = 300
}
