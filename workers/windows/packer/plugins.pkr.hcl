# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Windows worker images (R-POOL-5): two stages built from this directory.
#   base   = Windows Server 2025 + VS 2026 Build Tools + SDK + VC++ redist + WinFSP + shawl + Git (no sysprep;
#            also the Packer source of `worker` and the `windows-client` AMI)
#   worker = base + Buildbarn services + bootstrap hook + dead-man switch, generalised with EC2Launch v2 sysprep
# Entry point: `make -C workers/windows image-windows STAGE=base|worker` (see docs/operations/images.md).

packer {
  required_version = ">= 1.16.0"
  required_plugins {
    amazon = {
      source  = "github.com/hashicorp/amazon"
      version = "= 1.8.2"
    }
  }
}
