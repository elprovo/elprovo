variable "metadata_host" {
  description = "Host name of the Resource Manager endpoint whose metadata document describes the cloud, such as management.azure.com. Point it at a simulation for tests."
  type        = string
  default     = "management.azure.com"
}

variable "name" {
  description = "Environment name, used as a prefix for every resource. Lowercase letters, digits and dashes."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{0,30}$", var.name))
    error_message = "name must be 1 to 31 lowercase letters, digits or dashes, starting with a letter."
  }
}

variable "location" {
  description = "Azure location for the hosts and the previews, such as westeurope."
  type        = string
}

variable "image" {
  description = "Platform image of the agent and gateway hosts."
  type = object({
    publisher = string
    offer     = string
    sku       = string
    version   = string
  })
  default = {
    publisher = "Canonical"
    offer     = "ubuntu-24_04-lts"
    sku       = "server"
    version   = "latest"
  }
}

variable "agent_size" {
  description = "Virtual machine size of the agent host."
  type        = string
  default     = "Standard_B1s"
}

variable "gateway_size" {
  description = "Virtual machine size of the gateway host."
  type        = string
  default     = "Standard_B1s"
}

variable "admin_username" {
  description = "Admin user of the agent and gateway hosts. Azure requires one; setup logs in as root with the same key."
  type        = string
  default     = "elprovo"
}

variable "address_space" {
  description = "Private address range of the environment's virtual network."
  type        = string
  default     = "10.42.0.0/16"
}

variable "hosts_subnet" {
  description = "Subnet of the agent and gateway hosts, inside address_space."
  type        = string
  default     = "10.42.0.0/24"
}

variable "previews_subnet" {
  description = "Subnet of preview machines and build workers, inside address_space."
  type        = string
  default     = "10.42.16.0/20"
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
  description = "Value of the elprovo tag that the agent puts on every machine it creates. Empty means <name>-preview, so environments in one subscription keep apart."
  type        = string
  default     = ""
}

variable "registry_name" {
  description = "Name of the container registry that build workers push to: 5 to 50 letters and digits, unique across Azure."
  type        = string
}

variable "registry_sku" {
  description = "Tier of the container registry: Basic, Standard or Premium."
  type        = string
  default     = "Basic"
}
