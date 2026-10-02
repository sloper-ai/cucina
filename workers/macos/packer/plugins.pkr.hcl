# SPDX-License-Identifier: FSL-1.1-ALv2
#
# macOS Tart worker image (R-MAC-7): Cirrus Labs' macOS + Xcode image -> Cucina worker image, built locally into
# TART_HOME as `cucina-worker-macos:<xcode>-<cucina_version>`. Entry point: `make -C workers/macos image-macos
# XCODE=27.0` (build -> smoke test -> report). Pushing to the private GHCR package is a separate step (push.sh).

packer {
  required_version = ">= 1.16.0"
  required_plugins {
    tart = {
      source  = "github.com/cirruslabs/tart"
      version = "= 1.21.0"
    }
  }
}
