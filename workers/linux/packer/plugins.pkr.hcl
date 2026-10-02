# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Linux worker images (R-POOL-4): one template for x86_64 and arm64 (Graviton), Ubuntu 26.04 LTS (default) or
# Amazon Linux 2023. Entry point: `make -C workers/linux image-linux ARCH=x86_64|arm64 VARIANT=ubuntu|al2023`.

packer {
  required_version = ">= 1.16.0"
  required_plugins {
    amazon = {
      source  = "github.com/hashicorp/amazon"
      version = "= 1.8.2"
    }
  }
}
