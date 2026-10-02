#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# `bazel run //:update_goldens` (R-TEST-8e): refreshes every checked-in golden
# and generated file in the repository, i.e. every write_source_files target
# (cucina_proto_go/_rust and any `write_source_files(..., tags = ["tier-static"])`).
# CI never runs this; the tier-static diff tests fail on drift instead.
#
# Usage: bazel run //:update_goldens [-- <query scope, default //...>]
set -euo pipefail

cd "${BUILD_WORKSPACE_DIRECTORY:?run me with: bazel run //:update_goldens}"
scope="${1:-//...}"

bazel_bin="${BAZEL:-}"
if [[ -z "${bazel_bin}" ]]; then
  for candidate in bazelisk bazel; do
    if command -v "${candidate}" >/dev/null 2>&1 && "${candidate}" version >/dev/null 2>&1; then
      bazel_bin="${candidate}"
      break
    fi
  done
fi
: "${bazel_bin:?neither bazelisk nor bazel found; set BAZEL=/path/to/bazel}"

# Top-level update targets only: write_source_files with several files also
# creates per-file `<name>_<n>` targets that the aggregate already runs.
targets=()
while IFS= read -r t; do
  [[ -n "${t}" ]] && targets+=("${t}")
done < <("${bazel_bin}" query --noshow_progress --output=label \
  "kind('_write_source_file', ${scope}) except attr(name, '_[0-9]+\$', ${scope})" 2>/dev/null)
if [[ ${#targets[@]} -eq 0 ]]; then
  echo "update_goldens: no write_source_files targets under ${scope}"
  exit 0
fi

echo "update_goldens: building ${#targets[@]} update targets"
"${bazel_bin}" build --noshow_progress "${targets[@]}"
for t in "${targets[@]}"; do
  "${bazel_bin}" run --noshow_progress --ui_event_filters=-info,-stdout "${t}"
done
echo "update_goldens: done; review with 'git diff'"
