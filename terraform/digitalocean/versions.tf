terraform {
  required_version = ">= 1.6.0"

  required_providers {
    digitalocean = {
      source  = "digitalocean/digitalocean"
      version = "2.102.0"
    }
  }
}

provider "digitalocean" {
  api_endpoint = var.api_endpoint
}
