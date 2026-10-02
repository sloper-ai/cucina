# SPDX-License-Identifier: FSL-1.1-ALv2

terraform {
  required_version = ">= 1.13.1, < 1.14.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "= 6.67.0"
    }
  }

  # Local state outside the repository; see base/versions.tf.
  backend "local" {
    path = "${var.state_dir}/env/terraform.tfstate"
  }
}

provider "aws" {
  region = var.region

  default_tags {
    tags = local.tags
  }
}
