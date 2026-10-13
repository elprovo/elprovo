packer {
  required_plugins {
    vultr = {
      version = "= 2.7.0"
      source  = "github.com/vultr/vultr"
    }
  }
}

# The plugin always calls https://api.vultr.com. To build against a
# simulation, resolve that name to it and trust its certificate.

variable "api_key" {
  type        = string
  default     = env("VULTR_API_KEY")
  sensitive   = true
  description = "Vultr API key. Defaults to VULTR_API_KEY."
}

variable "region" {
  type        = string
  description = "Region where the image is built, such as fra."
}

variable "plan" {
  type    = string
  default = "vc2-2c-4gb"
}

variable "os_id" {
  type        = number
  default     = 2284
  description = "Operating system to build on. 2284 is Ubuntu 24.04 LTS x64."
}

variable "snapshot_description" {
  type    = string
  default = "elprovo-{{timestamp}}"
}

source "vultr" "elprovo" {
  api_key              = var.api_key
  region_id            = var.region
  plan_id              = var.plan
  os_id                = var.os_id
  ssh_username         = "root"
  snapshot_description = var.snapshot_description
  instance_label       = "elprovo-image-build"
  tags                 = ["elprovo-image-build"]
  state_timeout        = "15m"
}

build {
  sources = ["source.vultr.elprovo"]

  provisioner "shell" {
    environment_vars = ["DEBIAN_FRONTEND=noninteractive"]
    inline = [
      "cloud-init status --wait || true",
      "apt-get update",
      "apt-get install -y --no-install-recommends docker.io docker-compose-v2 docker-buildx",
      "systemctl enable docker",
      "docker compose version",
      "docker buildx version",
      "apt-get clean",
      "rm -rf /var/lib/apt/lists/*",
      # Each instance made from the image runs cloud-init again, which
      # installs the host key that the agent pins.
      "cloud-init clean --logs --machine-id",
    ]
  }
}
