packer {
  required_plugins {
    hcloud = {
      version = "= 1.8.1"
      source  = "github.com/hetznercloud/hcloud"
    }
  }
}

variable "endpoint" {
  type        = string
  default     = "https://api.hetzner.cloud/v1"
  description = "Hetzner Cloud API base URL, with the /v1 path. Point it at a simulation for tests. The token comes from HCLOUD_TOKEN."
}

variable "token" {
  type        = string
  default     = env("HCLOUD_TOKEN")
  sensitive   = true
  description = "Hetzner Cloud API token. Defaults to HCLOUD_TOKEN."
}

variable "location" {
  type        = string
  description = "Location where the image is built, such as fsn1. A snapshot can boot servers in every location."
}

variable "server_type" {
  type    = string
  default = "cx23"
}

variable "base_image" {
  type    = string
  default = "ubuntu-24.04"
}

variable "snapshot_name" {
  type    = string
  default = "stagely-{{timestamp}}"
}

source "hcloud" "stagely" {
  endpoint             = var.endpoint
  token                = var.token
  location             = var.location
  server_type          = var.server_type
  image                = var.base_image
  ssh_username         = "root"
  snapshot_name        = var.snapshot_name
  snapshot_labels      = { stagely = "golden-image" }
  server_labels        = { stagely = "image-build" }
  public_ipv6_disabled = true
}

build {
  sources = ["source.hcloud.stagely"]

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
      # Each server made from the image runs cloud-init again, which
      # installs the host key that the agent pins.
      "cloud-init clean --logs --machine-id",
    ]
  }
}
