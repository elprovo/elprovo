terraform {
  required_version = ">= 1.6.0"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "8.4.0"
    }
  }
}

provider "google" {
  project                           = var.project
  region                            = var.region
  zone                              = var.zone
  compute_custom_endpoint           = var.compute_endpoint == "" ? null : var.compute_endpoint
  artifact_registry_custom_endpoint = var.artifact_registry_endpoint == "" ? null : var.artifact_registry_endpoint
  iam_beta_custom_endpoint          = var.iam_endpoint == "" ? null : var.iam_endpoint
}
