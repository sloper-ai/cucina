#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
# Release-lane check (R-OPS-7, R-BUILD-3, R-AUTH-8): runs `cucina-release verify` over the
# artifacts of this build. The arguments come from //release:BUILD.bazel.
set -euo pipefail
export TMPDIR="${TEST_TMPDIR:-${TMPDIR:-/tmp}}"
exec "$@"
