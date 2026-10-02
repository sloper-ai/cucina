# SPDX-License-Identifier: FSL-1.1-ALv2

terraform {
  required_version = ">= 1.13.1, < 1.14.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "= 6.67.0"
    }
  }

  # Local state, deliberately OUTSIDE the repository (public-repo hygiene, §12).
  # OpenTofu evaluates `var.state_dir` early, so `tofu init` fails loudly unless the
  # caller supplies it (scripts/up.sh does; TF_VAR_state_dir also works). That makes it
  # impossible to create a repo-local terraform.tfstate by accident.
  backend "local" {
    path = "${var.state_dir}/base/terraform.tfstate"
  }
}

provider "aws" {
  region = var.region

  # The CLI profile comes from AWS_PROFILE (.work/env.sh sets `default`); it is never
  # hard-coded here. Account IDs are never written into the repository either.
  default_tags {
    tags = local.tags
  }
}
