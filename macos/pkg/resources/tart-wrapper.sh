#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
# /usr/local/cucina/bin/tart — convenience wrapper for humans (break-glass, troubleshooting):
#   sudo -u cucina -H /usr/local/cucina/bin/tart list
# Same pattern as Homebrew's formula: exec the binary inside the official, untouched tart.app bundle so the bundle's
# embedded provisioning profile (restricted com.apple.vm.networking entitlement) applies. hostd itself runs
# /usr/local/cucina/tart.app/Contents/MacOS/tart directly (docs/dev/hostd.md §2).
exec /usr/local/cucina/tart.app/Contents/MacOS/tart "$@"
