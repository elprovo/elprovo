packer {
  required_plugins {
    azure = {
      version = "= 2.6.4"
      source  = "github.com/hashicorp/azure"
    }
  }
}

variable "metadata_host" {
  type        = string
  default     = ""
  description = "URL of the Resource Manager endpoint of a custom cloud, such as a simulation. Empty means Azure."
}

variable "client_id" {
  type        = string
  default     = env("ARM_CLIENT_ID")
  description = "Client id of the service principal. Defaults to ARM_CLIENT_ID."
}

variable "client_secret" {
  type        = string
  default     = env("ARM_CLIENT_SECRET")
  sensitive   = true
  description = "Client secret of the service principal. Defaults to ARM_CLIENT_SECRET."
}

variable "tenant_id" {
  type        = string
  default     = env("ARM_TENANT_ID")
  description = "Microsoft Entra tenant of the service principal. Defaults to ARM_TENANT_ID."
}

variable "subscription_id" {
  type        = string
  default     = env("ARM_SUBSCRIPTION_ID")
  description = "Subscription the image is built in. Defaults to ARM_SUBSCRIPTION_ID."
}

variable "location" {
  type        = string
  description = "Location where the image is built and stored, such as westeurope."
}

variable "image_resource_group" {
  type        = string
  description = "Existing resource group that keeps the managed image."
}

variable "image_name" {
  type        = string
  default     = ""
  description = "Name of the managed image. Empty means elprovo-<UTC time>."
}

variable "vm_size" {
  type    = string
  default = "Standard_B2s"
}

locals {
  image_name = var.image_name != "" ? var.image_name : "elprovo-${formatdate("YYYYMMDDhhmmss", timestamp())}"
  build_user = "packer"
}

source "azure-arm" "elprovo" {
  metadata_host                     = var.metadata_host
  client_id                         = var.client_id
  client_secret                     = var.client_secret
  tenant_id                         = var.tenant_id
  subscription_id                   = var.subscription_id
  location                          = var.location
  vm_size                           = var.vm_size
  os_type                           = "Linux"
  image_publisher                   = "Canonical"
  image_offer                       = "ubuntu-24_04-lts"
  image_sku                         = "server"
  managed_image_name                = local.image_name
  managed_image_resource_group_name = var.image_resource_group
  ssh_username                      = local.build_user
  azure_tags = {
    elprovo = "image-build"
  }
}

build {
  sources = ["source.azure-arm.elprovo"]

  provisioner "shell" {
    execute_command  = "chmod +x {{ .Path }}; {{ .Vars }} sudo -E sh -eu '{{ .Path }}'"
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
      # Each machine made from the image runs cloud-init again, which creates
      # its admin user and installs the host key that the agent pins. The
      # build user and its keys stay out of the image.
      "cloud-init clean --logs --machine-id",
      "rm -f /root/.ssh/authorized_keys /etc/sudoers.d/90-cloud-init-users",
      "userdel --force --remove ${local.build_user}",
    ]
  }
}
