packer {
  required_plugins {
    amazon = {
      version = "= 1.8.2"
      source  = "github.com/hashicorp/amazon"
    }
  }
}

variable "region" {
  type        = string
  description = "Region where the image is built and registered, such as eu-central-1. Credentials come from AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY, and AWS_ENDPOINT_URL points every AWS API at a simulation for tests."
}

variable "subnet_id" {
  type        = string
  default     = ""
  description = "Subnet of the build instance. Empty means a default subnet of the default VPC."
}

variable "instance_type" {
  type    = string
  default = "t3.medium"
}

variable "ami_name" {
  type    = string
  default = "elprovo-{{timestamp}}"
}

source "amazon-ebs" "elprovo" {
  region        = var.region
  subnet_id     = var.subnet_id
  instance_type = var.instance_type
  ami_name      = var.ami_name

  source_ami_filter {
    owners      = ["099720109477"]
    most_recent = true
    filters = {
      name                = "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"
      architecture        = "x86_64"
      root-device-type    = "ebs"
      virtualization-type = "hvm"
    }
  }

  # Ubuntu AMIs let only the ubuntu user sign in; the build signs in as
  # root, as on the other clouds.
  user_data    = "#cloud-config\ndisable_root: false\n"
  ssh_username = "root"

  run_tags = {
    Name = "elprovo-image-build"
  }
  tags = {
    Name = var.ami_name
  }
}

build {
  sources = ["source.amazon-ebs.elprovo"]

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
      # Cloud-init gave Packer's temporary key to the default user too;
      # Packer removes it only from root.
      "rm -f /home/*/.ssh/authorized_keys",
      # Each instance made from the image runs cloud-init again, which
      # installs the host key that the agent pins.
      "cloud-init clean --logs --machine-id",
    ]
  }
}
