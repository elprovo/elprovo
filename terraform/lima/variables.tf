variable "name" {
  description = "Environment name, used as a prefix for every resource. Lowercase letters, digits and dashes."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{0,30}$", var.name))
    error_message = "name must be 1 to 31 lowercase letters, digits or dashes, starting with a letter."
  }
}

variable "host_address" {
  description = "Address at which Lima VMs on the user-v2 network reach the Mac."
  type        = string
  default     = "host.lima.internal"
}

variable "registry_port" {
  description = "Port on the Mac where the image registry listens."
  type        = number
  default     = 5050
}

variable "preview_domain" {
  description = "Domain that preview host names live under."
  type        = string
  default     = "previews.localhost"
}

variable "certificate_dir" {
  description = "Directory for the gateway's certificate and key."
  type        = string
}
