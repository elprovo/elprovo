variable "name" {
  description = "Environment name, used as a prefix for every resource. Lowercase letters, digits and dashes."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{0,30}$", var.name))
    error_message = "name must be 1 to 31 lowercase letters, digits or dashes, starting with a letter."
  }
}

variable "region" {
  description = "Vultr region for the hosts and the previews, such as fra."
  type        = string
}

variable "network_range" {
  description = "Address range of the VPC that the hosts, preview machines and build workers share, such as 10.42.0.0/20. `elprovo setup` sets it, and the hosts' user data rejects new connections from it."
  type        = string

  validation {
    condition     = can(cidrhost(var.network_range, 0)) && can(regex("^[0-9]{1,3}(\\.[0-9]{1,3}){3}/([89]|1[0-9]|2[0-9])$", var.network_range))
    error_message = "network_range must be an IPv4 CIDR block from /8 to /29, such as 10.42.0.0/20."
  }
}

variable "os_id" {
  description = "Operating system of the agent and gateway hosts. 2284 is Ubuntu 24.04 LTS x64."
  type        = number
  default     = 2284
}

variable "agent_plan" {
  description = "Plan of the agent host."
  type        = string
  default     = "vc2-1c-1gb"
}

variable "gateway_plan" {
  description = "Plan of the gateway host."
  type        = string
  default     = "vc2-1c-1gb"
}

variable "agent_user_data" {
  description = "Cloud-init user data of the agent host. `elprovo setup` sets an SSH host key it pins."
  type        = string
  sensitive   = true
  default     = ""
}

variable "gateway_user_data" {
  description = "Cloud-init user data of the gateway host. `elprovo setup` sets an SSH host key it pins."
  type        = string
  sensitive   = true
  default     = ""
}

variable "ssh_public_key" {
  description = "Public key that `elprovo setup` uses to install the agent and the gateway."
  type        = string
}

variable "operator_cidrs" {
  description = "Address ranges, such as 203.0.113.7/32, that may reach the agent and gateway hosts over SSH, for installs and upgrades."
  type        = list(string)

  validation {
    condition     = alltrue([for cidr in var.operator_cidrs : can(cidrhost(cidr, 0))])
    error_message = "operator_cidrs must hold CIDR blocks, such as 203.0.113.7/32."
  }
}

variable "preview_tag" {
  description = "Tag the agent puts on every machine it creates. Empty means <name>-preview, so environments in one account keep apart."
  type        = string
  default     = ""
}

variable "registry_name" {
  description = "Name of the container registry that build workers push to. Vultr registry names are global and hold lowercase letters and digits only."
  type        = string

  validation {
    condition     = can(regex("^[a-z0-9]+$", var.registry_name))
    error_message = "registry_name must hold lowercase letters and digits only."
  }
}

variable "registry_plan" {
  description = "Plan of the container registry: start_up, business, premium or enterprise."
  type        = string
  default     = "start_up"
}

variable "registry_region" {
  description = "Region of the container registry. Empty means region."
  type        = string
  default     = ""
}
