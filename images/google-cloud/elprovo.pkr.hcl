packer {
  required_plugins {
    googlecompute = {
      version = "= 1.2.7"
      source  = "github.com/hashicorp/googlecompute"
    }
  }
}

variable "project_id" {
  type        = string
  description = "Google Cloud project where the image is built and stored. The credentials come from GOOGLE_APPLICATION_CREDENTIALS."
}

variable "zone" {
  type        = string
  description = "Zone of the build instance, such as us-central1-a."
}

variable "compute_endpoint" {
  type        = string
  default     = ""
  description = "Compute Engine API base URL, ending in /compute/v1/. Empty means Google's. Point it at a simulation for tests."
}

variable "machine_type" {
  type    = string
  default = "e2-standard-2"
}

variable "source_image_family" {
  type    = string
  default = "ubuntu-2404-lts-amd64"
}

variable "image_name" {
  type    = string
  default = "elprovo-{{timestamp}}"
}

variable "image_family" {
  type    = string
  default = "elprovo"
}

source "googlecompute" "elprovo" {
  project_id              = var.project_id
  zone                    = var.zone
  machine_type            = var.machine_type
  source_image_family     = var.source_image_family
  source_image_project_id = ["ubuntu-os-cloud"]
  image_name              = var.image_name
  image_family            = var.image_family
  ssh_username            = "root"
  use_os_login            = false
  tags                    = ["elprovo-image-build"]
  custom_endpoints        = var.compute_endpoint == "" ? {} : { compute = var.compute_endpoint }
}

build {
  sources = ["source.googlecompute.elprovo"]

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
