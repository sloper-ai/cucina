#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Static tier (R-BUILD-1 "Protobuf"): `buf lint` over the API module declared in
# the root buf.yaml (api/proto). Runs in the live workspace (`local`, never
# cached) because buf.yaml addresses the module by its repository path.
# Usage: buf_lint_test.sh <buf binary> <buf.yaml>
set -euo pipefail

buf="$(cd "$(dirname "$1")" && pwd -P)/$(basename "$1")"
ws="$(dirname "$(realpath "$2")")"
export HOME="${TEST_TMPDIR:-$(mktemp -d)}" XDG_CACHE_HOME="${TEST_TMPDIR:-/tmp}/cache"
cd "${ws}"
"${buf}" lint
echo "buf lint: ok"
