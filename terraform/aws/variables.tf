variable "endpoint" {
  description = "One URL for the EC2, ECR and STS APIs, such as a simulation for tests. Empty means AWS. Credentials come from AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY."
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

variable "region" {
  description = "AWS region for the hosts and the stagelets, such as eu-central-1."
  type        = string
}

variable "availability_zone" {
  description = "Availability zone of the environment's subnet. Empty lets AWS choose."
  type        = string
  default     = ""
}

variable "image" {
  description = "AMI of the agent and gateway hosts, and of stagelet machines without a golden image. Empty means the newest Ubuntu 24.04 AMI from Canonical."
  type        = string
  default     = ""
}

variable "agent_instance_type" {
  description = "Instance type of the agent host."
  type        = string
  default     = "t3.small"
}

variable "gateway_instance_type" {
  description = "Instance type of the gateway host."
  type        = string
  default     = "t3.micro"
}

variable "vpc_cidr" {
  description = "IP range of the environment's VPC."
  type        = string
  default     = "10.42.0.0/16"
}

variable "subnet_cidr" {
  description = "IP range of the environment's subnet, inside vpc_cidr."
  type        = string
  default     = "10.42.0.0/20"
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
  description = "IPv4 address ranges that may reach the agent and gateway hosts over SSH, for installs and upgrades."
  type        = list(string)

  validation {
    condition     = length(var.operator_cidrs) > 0 && alltrue([for c in var.operator_cidrs : can(cidrnetmask(c))])
    error_message = "operator_cidrs must list IPv4 CIDR blocks, such as 203.0.113.7/32; the VPC has no IPv6."
  }
}

variable "stagelet_group" {
  description = "Value of the stagely-group tag the agent puts on every instance it creates, and finds them by. Empty means <name>-stagelet, so environments in one account keep apart."
  type        = string
  default     = ""
}

variable "registry_prefix" {
  description = "ECR repository name prefix that build workers push under. ECR creates each repository on its first push. Empty means name."
  type        = string
  default     = ""
}
