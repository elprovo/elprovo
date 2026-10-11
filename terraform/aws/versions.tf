terraform {
  required_version = ">= 1.6.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "6.66.0"
    }
  }
}

provider "aws" {
  region = var.region

  dynamic "endpoints" {
    for_each = var.endpoint == "" ? [] : [var.endpoint]
    content {
      ec2 = endpoints.value
      ecr = endpoints.value
      sts = endpoints.value
    }
  }
}
