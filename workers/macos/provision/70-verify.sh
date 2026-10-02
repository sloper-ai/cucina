#!/bin/bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Final gate of the Packer build: the installed smoke test must pass inside the freshly rebooted image
# (waits up to 120 s for the build user's automatic GUI login). Runs as root.
set -euo pipefail
/usr/local/cucina/libexec/cucina-smoke --wait-session 120
