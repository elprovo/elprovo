packer {
  required_plugins {
    digitalocean = {
      version = "= 1.4.1"
      source  = "github.com/digitalocean/digitalocean"
    }
  }
}

variable "api_url" {
  type        = string
  default     = "https://api.digitalocean.com"
  description = "DigitalOcean API base URL. Point it at a simulation for tests. The token comes from DIGITALOCEAN_TOKEN."
}

variable "api_token" {
  type        = string
  default     = env("DIGITALOCEAN_TOKEN")
  sensitive   = true
  description = "DigitalOcean API token. Defaults to DIGITALOCEAN_TOKEN."
}

variable "region" {
  type        = string
  description = "Region where the image is built and stored, such as fra1."
}

variable "size" {
  type    = string
  default = "s-2vcpu-4gb"
}

variable "base_image" {
  type    = string
  default = "ubuntu-24-04-x64"
}

variable "snapshot_name" {
  type    = string
  default = "stagely-{{timestamp}}"
}

source "digitalocean" "stagely" {
  api_url       = var.api_url
  api_token     = var.api_token
  region        = var.region
  size          = var.size
  image         = var.base_image
  ssh_username  = "root"
  snapshot_name = var.snapshot_name
  droplet_agent = false
  tags          = ["stagely-image-build"]
}

build {
  sources = ["source.digitalocean.stagely"]

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
      # Each droplet made from the image runs cloud-init again, which
      # installs the host key that the agent pins.
      "cloud-init clean --logs --machine-id",
    ]
  }
}
