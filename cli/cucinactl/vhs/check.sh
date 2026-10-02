#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Runs the VHS tapes of `cucinactl tui` (2-3 key flows, R-TEST-6) and compares the
# text frames they capture with the goldens in golden/. The tapes use the built-in
# deterministic demo data (CUCINA_TUI_DEMO=frozen|scripted), so no cluster is needed.
#
# Needs vhs v0.12.1, ttyd and ffmpeg on PATH, plus a Chrome/Chromium for vhs's
# renderer. These are not hermetic, so this is a manual-tier check (docs/tui.md),
# not part of `bazel test //...`.
#
#   cli/cucinactl/vhs/check.sh             # compare with the goldens
#   cli/cucinactl/vhs/check.sh --update    # rewrite the goldens (review the diff)
#   CUCINACTL_BIN=/path/to/cucinactl cli/cucinactl/vhs/check.sh   # another binary
#   VHS_KEEP=/some/dir cli/cucinactl/vhs/check.sh                 # keep the GIFs
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/../../.." && pwd)"
update=0
[[ "${1:-}" == "--update" ]] && update=1

bin="${CUCINACTL_BIN:-}"
if [[ -z "$bin" ]]; then
  (cd "$repo" && cargo build -q -p cucinactl --bin cucinactl)
  target="${CARGO_TARGET_DIR:-$repo/target}"
  bin="$target/debug/cucinactl"
fi
[[ -x "$bin" ]] || { echo "no cucinactl binary at $bin" >&2; exit 2; }

if [[ -n "${VHS:-}" ]]; then
  vhs=("$VHS")
elif command -v vhs >/dev/null; then
  vhs=(vhs)
else
  vhs=(mise exec vhs@0.12.1 -- vhs)
fi
for tool in ttyd ffmpeg; do
  command -v "$tool" >/dev/null || { echo "missing $tool (e.g. brew install $tool)" >&2; exit 2; }
done

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin"
ln -s "$bin" "$work/bin/cucinactl"

# VHS writes the whole terminal after every visible command, frames separated by a
# line of 80 '─'. Keep the TUI frames only: trim trailing blanks, drop empty and
# shell-prompt-only frames and consecutive duplicates.
normalize() {
  local sep
  sep="$(printf '─%.0s' $(seq 80))"
  awk -v sep="$sep" '
    function flush(  t) {
      t = frame; sub(/\n+$/, "", t)
      if (t != "" && t != ">" && t != prev) { if (n++) print sep; print t; prev = t }
      frame = ""
    }
    $0 == sep { flush(); next }
    { line = $0; sub(/[ \t]+$/, "", line); frame = frame line "\n" }
    END { flush() }'
}

status=0
for tape in "$here"/*.tape; do
  name="$(basename "$tape" .tape)"
  if ! (cd "$work" && PATH="$work/bin:$PATH" "${vhs[@]}" "$tape" >"$work/$name.log" 2>&1); then
    echo "FAIL $name: vhs failed" >&2
    tail -20 "$work/$name.log" >&2
    status=1
    continue
  fi
  normalize <"$work/out/$name.txt" >"$work/$name.frames"
  if [[ -n "${VHS_KEEP:-}" ]]; then
    mkdir -p "$VHS_KEEP" && cp "$work/out/$name.gif" "$VHS_KEEP/"
  fi
  if ((update)); then
    cp "$work/$name.frames" "$here/golden/$name.txt"
    echo "updated golden/$name.txt"
  elif diff -u "$here/golden/$name.txt" "$work/$name.frames"; then
    echo "ok   $name"
  else
    echo "FAIL $name: frames differ from golden/$name.txt" >&2
    status=1
  fi
done
exit "$status"
