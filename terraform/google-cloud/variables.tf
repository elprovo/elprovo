variable "compute_endpoint" {
  description = "Compute Engine API base URL, ending in /compute/v1/. Empty means Google's. Point it at a simulation for tests. The credentials come from GOOGLE_CREDENTIALS."
  type        = string
  default     = ""
}

variable "artifact_registry_endpoint" {
  description = "Artifact Registry API base URL, ending in /v1/. Empty means Google's."
  type        = string
  default     = ""
}

variable "iam_endpoint" {
  description = "IAM API base URL, ending in /v1/. Empty means Google's."
  type        = string
  default     = ""
}

variable "name" {
  description = "Environment name, used as a prefix for every resource. Lowercase letters, digits and dashes."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{0,30}$", var.name))
    error_message = "name must be 1 to 31 lowercase letters, digits or dashes, starting with a letter."
  }
}

variable "project" {
  description = "Google Cloud project ID."
  type        = string
}

variable "region" {
  description = "Region of the network, the gateway's address and the image repository, such as us-central1."
  type        = string
}

variable "zone" {
  description = "Zone of the agent and gateway hosts, such as us-central1-a. It must be in region."
  type        = string
}

variable "image" {
  description = "Image of the agent and gateway hosts."
  type        = string
  default     = "ubuntu-os-cloud/ubuntu-2404-lts-amd64"
}

variable "agent_machine_type" {
  description = "Machine type of the agent host."
  type        = string
  default     = "e2-small"
}

variable "gateway_machine_type" {
  description = "Machine type of the gateway host."
  type        = string
  default     = "e2-small"
}

variable "subnet_cidr" {
  description = "Internal IP range of the environment's subnetwork."
  type        = string
  default     = "10.42.0.0/20"
}

variable "agent_user_data" {
  description = "Cloud-config user data of the agent host. `stagely setup` sets an SSH host key it pins. It must not set write_files, which the module adds."
  type        = string
  sensitive   = true
  default     = ""
}

variable "gateway_user_data" {
  description = "Cloud-config user data of the gateway host. `stagely setup` sets an SSH host key it pins. It must not set write_files, which the module adds."
  type        = string
  sensitive   = true
  default     = ""
}

variable "ssh_public_key" {
  description = "Public key that `stagely setup` uses to install the agent and the gateway, as root."
  type        = string
}

variable "operator_cidrs" {
  description = "Addresses that may reach the agent and gateway hosts over SSH, for installs and upgrades."
  type        = list(string)
}

variable "stagelet_tag" {
  description = "Network tag the agent puts on every machine it creates. The stagelet firewall rule targets it. Empty means <name>-stagelet, so environments in one project keep apart."
  type        = string
  default     = ""
}

variable "registry_name" {
  description = "ID of the Artifact Registry Docker repository that build workers push to."
  type        = string
}

variable "agent_service_account" {
  description = "Email of the service account the agent signs in with. It may impersonate the registry accounts."
  type        = string
}
