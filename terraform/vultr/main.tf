locals {
  stagelet_tag    = var.stagelet_tag != "" ? var.stagelet_tag : "${var.name}-stagelet"
  registry_region = var.registry_region != "" ? var.registry_region : var.region
  operators = {
    for cidr in var.operator_cidrs : cidr => {
      ip_type     = strcontains(cidr, ":") ? "v6" : "v4"
      subnet      = cidrhost(cidr, 0)
      subnet_size = tonumber(split("/", cidr)[1])
    }
  }
  everyone = {
    v4 = { subnet = "0.0.0.0", subnet_size = 0 }
    v6 = { subnet = "::", subnet_size = 0 }
  }
}

resource "vultr_ssh_key" "operator" {
  name    = "${var.name}-stagely-setup"
  ssh_key = var.ssh_public_key
}

# The hosts, stagelet machines and build workers share this VPC, and the
# agent and the gateway reach stagelets over it. Vultr firewall groups filter
# only inbound traffic on the public interface, so each machine filters the
# VPC with its own iptables rules, which its user data adds on every boot:
# the agent and the gateway reject every new connection from the VPC, and
# stagelet machines and build workers accept new ones only from the agent and
# the gateway.
resource "vultr_vpc" "environment" {
  region         = var.region
  description    = "${var.name}-stagely"
  v4_subnet      = cidrhost(var.network_range, 0)
  v4_subnet_mask = tonumber(split("/", var.network_range)[1])
}

# The agent accepts no inbound connections except SSH from the operator.
resource "vultr_firewall_group" "agent" {
  description = "${var.name}-agent"
}

resource "vultr_firewall_rule" "agent_ssh" {
  for_each          = local.operators
  firewall_group_id = vultr_firewall_group.agent.id
  protocol          = "tcp"
  ip_type           = each.value.ip_type
  subnet            = each.value.subnet
  subnet_size       = each.value.subnet_size
  port              = "22"
  notes             = "operator"
}

# The gateway is the only host that accepts internet traffic.
resource "vultr_firewall_group" "gateway" {
  description = "${var.name}-gateway"
}

resource "vultr_firewall_rule" "gateway_http" {
  for_each          = local.everyone
  firewall_group_id = vultr_firewall_group.gateway.id
  protocol          = "tcp"
  ip_type           = each.key
  subnet            = each.value.subnet
  subnet_size       = each.value.subnet_size
  port              = "80"
}

resource "vultr_firewall_rule" "gateway_https" {
  for_each          = local.everyone
  firewall_group_id = vultr_firewall_group.gateway.id
  protocol          = "tcp"
  ip_type           = each.key
  subnet            = each.value.subnet
  subnet_size       = each.value.subnet_size
  port              = "443"
}

resource "vultr_firewall_rule" "gateway_ssh" {
  for_each          = local.operators
  firewall_group_id = vultr_firewall_group.gateway.id
  protocol          = "tcp"
  ip_type           = each.value.ip_type
  subnet            = each.value.subnet
  subnet_size       = each.value.subnet_size
  port              = "22"
  notes             = "operator"
}

# Stagelet machines and build workers accept no connections on their public
# address, which they keep only to reach the internet: the agent and the
# gateway reach them over the VPC. A group without rules drops all inbound
# traffic. The agent puts every machine in this group.
resource "vultr_firewall_group" "stagelets" {
  description = "${var.name}-stagelets"
}

# The agent's main address stays the same when the host is replaced, so the
# account's API access control can name it.
resource "vultr_reserved_ip" "agent" {
  region  = var.region
  ip_type = "v4"
  label   = "${var.name}-agent"
}

# Stagelet host names point at the gateway's reserved IP.
resource "vultr_reserved_ip" "gateway" {
  region  = var.region
  ip_type = "v4"
  label   = "${var.name}-gateway"
}

resource "vultr_instance" "agent" {
  label             = "${var.name}-agent"
  hostname          = "${var.name}-agent"
  region            = var.region
  plan              = var.agent_plan
  os_id             = var.os_id
  ssh_key_ids       = [vultr_ssh_key.operator.id]
  firewall_group_id = vultr_firewall_group.agent.id
  reserved_ip_id    = vultr_reserved_ip.agent.id
  vpc_ids           = [vultr_vpc.environment.id]
  tags              = ["${var.name}-agent"]
  user_data         = var.agent_user_data
  activation_email  = false

  # The host is reachable over SSH as soon as it exists.
  depends_on = [vultr_firewall_rule.agent_ssh]
}

resource "vultr_instance" "gateway" {
  label             = "${var.name}-gateway"
  hostname          = "${var.name}-gateway"
  region            = var.region
  plan              = var.gateway_plan
  os_id             = var.os_id
  ssh_key_ids       = [vultr_ssh_key.operator.id]
  firewall_group_id = vultr_firewall_group.gateway.id
  reserved_ip_id    = vultr_reserved_ip.gateway.id
  vpc_ids           = [vultr_vpc.environment.id]
  tags              = ["${var.name}-gateway"]
  user_data         = var.gateway_user_data
  activation_email  = false

  depends_on = [vultr_firewall_rule.gateway_http, vultr_firewall_rule.gateway_https, vultr_firewall_rule.gateway_ssh]
}

resource "vultr_container_registry" "stagely" {
  name   = var.registry_name
  region = local.registry_region
  plan   = var.registry_plan
  public = false
}
