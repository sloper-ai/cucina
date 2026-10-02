#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Marks go.mod requirements as direct (drops "// indirect") when our code
# imports them, WITHOUT the rest of `go mod tidy` (which would also drop
# requirements other agents added but don't import yet). Gazelle's go_deps only
# exposes direct requirements as repositories, so run this before
# `bazel mod tidy` + `bazel run //:gazelle`:
#
#   lockf "$CUCINA_DEV_STORAGE/gomod.lock" tools/go-mod-direct.sh
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
diff="$(go mod tidy -diff 2>/dev/null || true)"
# Requirements whose only change under `go mod tidy` is losing "// indirect".
was_indirect="$(printf '%s\n' "${diff}" | awk '/^-\t/ && /\/\/ indirect/ {print $2 "@" $3}' | sort -u)"
now_direct="$(printf '%s\n' "${diff}" | awk '/^\+\t/ && !/\/\/ indirect/ {print $2 "@" $3}' | sort -u)"
promote="$(comm -12 <(printf '%s\n' "${was_indirect}") <(printf '%s\n' "${now_direct}") | sed '/^$/d')"
if [[ -z "${promote}" ]]; then
  echo "go-mod-direct: nothing to promote"
  exit 0
fi
args=()
while IFS= read -r modver; do
  args+=("-droprequire=${modver%@*}" "-require=${modver}")
  echo "go-mod-direct: ${modver}"
done <<<"${promote}"
go mod edit "${args[@]}"
