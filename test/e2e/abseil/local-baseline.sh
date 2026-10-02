#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Local baseline for NFR-P2 and NFR-X2: build, then test, //absl/... in a
# prepared Abseil checkout (bazelrun.PrepareAbseil) WITHOUT remote execution,
# from a cold output base, on this machine — a worker-type instance for
# NFR-P2's denominator, the client VM for the reported speed-up, the dev Mac
# for macOS outcomes (X2). Prints one JSON line; logs and the BEP go to OUT.
#
# Usage: local-baseline.sh <abseil-checkout> <out-dir> [bazel flags, e.g. --config=lane-linux]
set -u
WS=$1
OUT=$2
shift 2
mkdir -p "$OUT"
cd "$WS" || exit 2
OB="$WS.local-output-base"
bazel --output_base="$OB" clean --expunge >/dev/null 2>&1 || true
start=$(date +%s)
bazel --output_base="$OB" build "$@" //absl/... >"$OUT/build.log" 2>&1
brc=$?
mid=$(date +%s)
bazel --output_base="$OB" test "$@" --build_event_json_file="$OUT/bep.json" //absl/... >"$OUT/test.log" 2>&1
trc=$?
end=$(date +%s)
printf '{"buildExit":%d,"testExit":%d,"buildSeconds":%d,"testSeconds":%d,"wallSeconds":%d,"cpus":%s}\n' \
  "$brc" "$trc" $((mid - start)) $((end - mid)) $((end - start)) "$(getconf _NPROCESSORS_ONLN)"
