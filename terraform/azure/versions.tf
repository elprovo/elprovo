terraform {
  required_version = ">= 1.6.0"

  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = "5.7.0"
    }
  }
}

# Credentials and the subscription come from ARM_CLIENT_ID, ARM_CLIENT_SECRET,
# ARM_TENANT_ID and ARM_SUBSCRIPTION_ID.
provider "azurerm" {
  features {}
  metadata_host                   = var.metadata_host
  resource_provider_registrations = "none"
}
