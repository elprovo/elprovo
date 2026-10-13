variable "api_endpoint" {
  description = "DigitalOcean API base URL. Point it at a simulation for tests. The token comes from DIGITALOCEAN_TOKEN."
  type        = string
  default     = "https://api.digitalocean.com"
}

variable "name" {
  description = "Environment name, used as a prefix for every resource. Lowercase letters, digits and dashes."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{0,30}$", var.name))
    error_message = "name must be 1 to 31 lowercase letters, digits or dashes, starting with a letter."
  }
}

variable "region" {
  description = "DigitalOcean region for the hosts and the previews, such as fra1."
  type        = string
}

variable "image" {
  description = "Image of the agent and gateway hosts."
  type        = string
  default     = "ubuntu-24-04-x64"
}

variable "agent_size" {
  description = "Droplet size of the agent host."
  type        = string
  default     = "s-1vcpu-1gb"
}

variable "gateway_size" {
  description = "Droplet size of the gateway host."
  type        = string
  default     = "s-1vcpu-1gb"
}

variable "vpc_ip_range" {
  description = "Private IP range of the environment's VPC."
  type        = string
  default     = "10.42.0.0/20"
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
  description = "Addresses that may reach the agent and gateway hosts over SSH, for installs and upgrades."
  type        = list(string)
}

variable "preview_tag" {
  description = "Tag the agent puts on every machine it creates. The preview firewall applies to it. Empty means <name>-preview, so environments in one account keep apart."
  type        = string
  default     = ""
}

variable "create_registry" {
  description = "Create the account's container registry. DigitalOcean allows one per account; set false to use an existing one."
  type        = bool
  default     = true
}

variable "registry_name" {
  description = "Name of the container registry that build workers push to."
  type        = string
}

variable "registry_tier" {
  description = "Subscription tier of a new container registry: starter, basic or professional."
  type        = string
  default     = "basic"
}

variable "dns_domain" {
  description = "A domain managed in DigitalOcean DNS. When set, a wildcard record for preview_subdomain points at the gateway."
  type        = string
  default     = ""
}

variable "preview_subdomain" {
  description = "Subdomain of dns_domain that preview host names live under, such as previews."
  type        = string
  default     = "previews"
}
