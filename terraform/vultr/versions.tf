terraform {
  required_version = ">= 1.6.0"

  required_providers {
    vultr = {
      source  = "vultr/vultr"
      version = "2.32.0"
    }
  }
}

# The provider reads the API key from VULTR_API_KEY. It always calls
# https://api.vultr.com; tests resolve that name to a simulation.
provider "vultr" {}
