#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Prefetches every external repository the build needs (host and cross
# configurations) into the shared repository cache configured in user.bazelrc,
# so later builds in any output base start warm. Safe to run in the background:
#   nohup tools/bazel-warmup.sh > $CUCINA_DEV_STORAGE/logs/bazel-warmup.log 2>&1 &
# Env: BAZEL (default: bazelisk, else bazel), WARMUP_OUTPUT_BASE (optional
# dedicated output base so the warm-up never blocks your interactive server).
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
bazel_bin="${BAZEL:-$(command -v bazelisk || command -v bazel)}"
startup=()
if [[ -n "${WARMUP_OUTPUT_BASE:-}" ]]; then
  startup+=("--output_base=${WARMUP_OUTPUT_BASE}")
fi
run() { echo "+ bazel $*" >&2; "${bazel_bin}" "${startup[@]+"${startup[@]}"}" "$@"; }

[[ -f user.bazelrc ]] || tools/setup-user-bazelrc.sh

start=$(date +%s)
# 1. Everything reachable from the host configuration (packages that are still
#    broken in the working tree don't stop the warm-up).
run fetch --noshow_progress --keep_going //... || true
# 2. Toolchains and crates for the cross targets (analysis fetches what the
#    resolved toolchains need; nothing is built).
for platform in \
  @rules_rs//rs/platforms:x86_64-unknown-linux-musl \
  @rules_rs//rs/platforms:aarch64-unknown-linux-musl \
  @llvm//platforms:linux_x86_64_gnu.2.28 \
  @llvm//platforms:linux_aarch64_gnu.2.28; do
  run build --noshow_progress --nobuild --keep_going --platforms="${platform}" //cli/... //tools/hello/... || true
done
# Windows Rust release target (ADR 0103).
run build --noshow_progress --nobuild --keep_going \
  --platforms=@rules_rs//rs/platforms:x86_64-pc-windows-gnullvm //cli/... //tools/hello/rust/... || true
echo "warm-up finished in $(( $(date +%s) - start ))s"
