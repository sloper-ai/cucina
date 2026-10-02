#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Static tier (§12 public-repo hygiene): gitleaks finds no secret in any file git
# would commit (tracked + untracked-but-not-ignored). Runs unsandboxed (`local`)
# because it scans the live workspace; `external` keeps Bazel from caching it.
# Usage: gitleaks_test.sh <gitleaks binary> <.gitleaks.toml>
set -euo pipefail

gitleaks="$(cd "$(dirname "$1")" && pwd -P)/$(basename "$1")"
config="$(realpath "$2")"
ws="$(dirname "${config}")"
cd "${ws}"
git rev-parse --is-inside-work-tree >/dev/null

tree="${TEST_TMPDIR:-$(mktemp -d)}/tree"
mkdir -p "${tree}"
git ls-files -z --cached --others --exclude-standard |
  while IFS= read -r -d '' f; do [[ -f "${f}" && ! -L "${f}" ]] && printf '%s\0' "${f}"; done |
  tar --null -T - -cf - | tar -xf - -C "${tree}"

"${gitleaks}" dir --no-banner --redact --log-level=warn --exit-code=1 --config="${config}" "${tree}"
echo "gitleaks: no leaks in $(find "${tree}" -type f | wc -l | tr -d ' ') files"
