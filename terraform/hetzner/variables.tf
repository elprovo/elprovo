variable "endpoint" {
  description = "Hetzner Cloud API base URL, with the /v1 path. Point it at a simulation for tests. The token comes from HCLOUD_TOKEN."
  type        = string
  default     = "https://api.hetzner.cloud/v1"
}

variable "name" {
  description = "Environment name, used as a prefix for every resource. Lowercase letters, digits and dashes."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{0,30}$", var.name))
    error_message = "name must be 1 to 31 lowercase letters, digits or dashes, starting with a letter."
  }
}

variable "network_range" {
  description = "Address range of the private network that the hosts, stagelet machines and build workers share, such as 10.42.0.0/20. `stagely setup` sets it, and the hosts' user data rejects new connections from it."
  type        = string

  validation {
    condition     = can(cidrhost(var.network_range, 0)) && can(regex("^[0-9]{1,3}(\\.[0-9]{1,3}){3}/([89]|1[0-9]|20)$", var.network_range))
    error_message = "network_range must be an IPv4 CIDR block from /8 to /20, such as 10.42.0.0/20."
  }
}

variable "location" {
  description = "Hetzner location for the hosts and the stagelets, such as fsn1."
  type        = string
}

variable "image" {
  description = "Image of the agent and gateway hosts."
  type        = string
  default     = "ubuntu-24.04"
}

variable "agent_server_type" {
  description = "Server type of the agent host."
  type        = string
  default     = "cx23"
}

variable "gateway_server_type" {
  description = "Server type of the gateway host."
  type        = string
  default     = "cx23"
}

variable "agent_user_data" {
  description = "Cloud-init user data of the agent host. `stagely setup` sets an SSH host key it pins."
  type        = string
  sensitive   = true
  default     = ""
}

variable "gateway_user_data" {
  description = "Cloud-init user data of the gateway host. `stagely setup` sets an SSH host key it pins."
  type        = string
  sensitive   = true
  default     = ""
}

variable "ssh_public_key" {
  description = "Public key that `stagely setup` uses to install the agent and the gateway."
  type        = string
}

variable "operator_cidrs" {
  description = "Addresses that may reach the agent and gateway hosts over SSH, for installs and upgrades."
  type        = list(string)
}

variable "stagelet_label" {
  description = "Label, as key=value, that the agent puts on every server it creates. The stagelet firewall applies to it. Empty means stagely=<name>-stagelet, so environments in one project keep apart."
  type        = string
  default     = ""

  validation {
    condition     = var.stagelet_label == "" || can(regex("^[a-zA-Z0-9][-_.a-zA-Z0-9]{0,61}[a-zA-Z0-9]?=[a-zA-Z0-9]([-_.a-zA-Z0-9]{0,61}[a-zA-Z0-9])?$", var.stagelet_label))
    error_message = "stagelet_label must be key=value with a valid Hetzner label key and value."
  }
}
