locals {
  preview_label = var.preview_label != "" ? var.preview_label : "elprovo=${var.name}-preview"
}

resource "hcloud_ssh_key" "operator" {
  name       = "${var.name}-elprovo-setup"
  public_key = var.ssh_public_key
}

# The gateway's stable address. A primary IP is the server's own address,
# which Hetzner configures on the server, and it outlives the server, so a
# replaced gateway keeps the address that preview host names point at.
resource "hcloud_primary_ip" "gateway" {
  name        = "${var.name}-gateway"
  type        = "ipv4"
  location    = var.location
  auto_delete = false
}

# The agent accepts no inbound connections except SSH from the operator.
resource "hcloud_firewall" "agent" {
  name = "${var.name}-agent"

  rule {
    direction  = "in"
    protocol   = "tcp"
    port       = "22"
    source_ips = var.operator_cidrs
  }
}

# The gateway is the only host that accepts internet traffic.
resource "hcloud_firewall" "gateway" {
  name = "${var.name}-gateway"

  rule {
    direction  = "in"
    protocol   = "tcp"
    port       = "80"
    source_ips = ["0.0.0.0/0", "::/0"]
  }

  rule {
    direction  = "in"
    protocol   = "tcp"
    port       = "443"
    source_ips = ["0.0.0.0/0", "::/0"]
  }

  rule {
    direction  = "in"
    protocol   = "tcp"
    port       = "22"
    source_ips = var.operator_cidrs
  }
}

data "hcloud_location" "environment" {
  name = var.location
}

# The hosts, preview machines and build workers share this network, and the
# agent and the gateway reach previews over it. Hetzner's firewalls do not
# filter private networks, so each machine filters it with its own iptables
# rules, which its user data adds on every boot: the agent and the gateway
# reject every new connection from the network, and preview machines and
# build workers accept new ones only from the agent and the gateway.
resource "hcloud_network" "environment" {
  name     = "${var.name}-elprovo"
  ip_range = var.network_range
}

resource "hcloud_network_subnet" "environment" {
  network_id   = hcloud_network.environment.id
  type         = "cloud"
  network_zone = data.hcloud_location.environment.network_zone
  ip_range     = cidrsubnet(var.network_range, 4, 0)
}

resource "hcloud_server" "agent" {
  name         = "${var.name}-agent"
  server_type  = var.agent_server_type
  image        = var.image
  location     = var.location
  ssh_keys     = [hcloud_ssh_key.operator.id]
  user_data    = var.agent_user_data
  firewall_ids = [hcloud_firewall.agent.id]

  public_net {
    ipv4_enabled = true
    ipv6_enabled = false
  }
}

resource "hcloud_server_network" "agent" {
  server_id  = hcloud_server.agent.id
  network_id = hcloud_network.environment.id
  depends_on = [hcloud_network_subnet.environment]
}

resource "hcloud_server" "gateway" {
  name         = "${var.name}-gateway"
  server_type  = var.gateway_server_type
  image        = var.image
  location     = var.location
  ssh_keys     = [hcloud_ssh_key.operator.id]
  user_data    = var.gateway_user_data
  firewall_ids = [hcloud_firewall.gateway.id]

  public_net {
    ipv4_enabled = true
    ipv4         = hcloud_primary_ip.gateway.id
    ipv6_enabled = false
  }
}

resource "hcloud_server_network" "gateway" {
  server_id  = hcloud_server.gateway.id
  network_id = hcloud_network.environment.id
  depends_on = [hcloud_network_subnet.environment]
}

# Preview machines and build workers accept no connections on their public
# address, which they keep only to reach the internet: the agent and the
# gateway reach them over the network. A firewall without rules drops all
# inbound traffic. It finds them by the label the agent puts on them.
resource "hcloud_firewall" "previews" {
  name = "${var.name}-previews"

  apply_to {
    label_selector = local.preview_label
  }
}
