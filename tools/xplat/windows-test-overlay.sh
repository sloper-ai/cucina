#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Builds the @bazel_tools overlay that lets a Linux or macOS Bazel client run tests on Windows
# execution platforms (R-XPLAT-4, bazelbuild/bazel#19209, ADR 0904).
#
# Non-Windows Bazel builds embed dummy.sh where Windows builds embed the test wrapper (tw.exe) and
# the test XML writer (xml.exe), so a test whose exec platform is Windows fails with
# "missing input file '@@bazel_tools//tools/test:tw.exe'". The overlay is this client's own
# @bazel_tools (the install base's embedded_tools, unchanged) plus tw.exe and xml.exe taken from
# the official Windows release of the SAME Bazel version, verified by SHA-256. Bazel 9.2 accepts it
# under Bzlmod:
#
#   common --override_repository=bazel_tools=<overlay directory>
#
# Usage: tools/xplat/windows-test-overlay.sh [OUTPUT_DIR]
#   OUTPUT_DIR defaults to ${XDG_CACHE_HOME:-$HOME/.cache}/cucina/bazel_tools_overlay/<version>.
# Env:   BAZEL (default: bazelisk, else bazel) runs `bazel info` in the current workspace.
# Prints the --override_repository line to add to the machine-local user.bazelrc.
set -euo pipefail

# Official Windows releases (https://github.com/bazelbuild/bazel/releases), no-JDK variant: the
# embedded tools are identical. Add a line per pinned Bazel version (.bazelversion).
release_sha256() {
  case "$1" in
    9.2.0) echo "d86a8241cfd5c0ce56ec6a61c08c381ff4178dc74d17dd5349b6636ccc5f1dcb" ;;
    *) return 1 ;;
  esac
}

bazel="${BAZEL:-}"
if [[ -z "${bazel}" ]]; then
  for candidate in bazelisk bazel; do
    if command -v "${candidate}" >/dev/null 2>&1; then
      bazel="${candidate}"
      break
    fi
  done
fi
: "${bazel:?neither bazelisk nor bazel found; set BAZEL=/path/to/bazel}"

version="$("${bazel}" info release 2>/dev/null | awk '{print $2}')"
install_base="$("${bazel}" info install_base 2>/dev/null)"
[[ -n "${version}" && -d "${install_base}/embedded_tools" ]] || {
  echo "windows-test-overlay: cannot find this Bazel's embedded tools (bazel info failed)" >&2
  exit 1
}
sha256="$(release_sha256 "${version}")" || {
  echo "windows-test-overlay: no pinned Windows release for Bazel ${version}; add its SHA-256 to $0" >&2
  exit 1
}
case "$(uname -s)" in
  MINGW* | MSYS* | CYGWIN*)
    echo "windows-test-overlay: Windows Bazel already embeds tw.exe/xml.exe; no overlay is needed" >&2
    exit 1
    ;;
esac

out="${1:-${XDG_CACHE_HOME:-${HOME}/.cache}/cucina/bazel_tools_overlay/${version}}"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

asset="bazel_nojdk-${version}-windows-x86_64.exe"
curl -fsSL --retry 3 -o "${work}/${asset}" \
  "https://github.com/bazelbuild/bazel/releases/download/${version}/${asset}"
actual="$(shasum -a 256 "${work}/${asset}" 2>/dev/null || sha256sum "${work}/${asset}")"
if [[ "${actual%% *}" != "${sha256}" ]]; then
  echo "windows-test-overlay: SHA-256 mismatch for ${asset}: got ${actual%% *}, want ${sha256}" >&2
  exit 1
fi
# The Windows launcher is a native stub with the install base appended as a zip archive.
unzip -q -o "${work}/${asset}" "embedded_tools/tools/test/tw.exe" "embedded_tools/tools/test/xml.exe" -d "${work}/win"

rm -rf "${out}.tmp"
mkdir -p "${out}.tmp"
# Everything of this client's @bazel_tools, unchanged, except the embedded JDK (linked, not copied).
for entry in "${install_base}/embedded_tools"/* "${install_base}/embedded_tools"/.[!.]*; do
  [[ -e "${entry}" ]] || continue
  if [[ "$(basename "${entry}")" == "jdk" ]]; then
    ln -s "${entry}" "${out}.tmp/jdk"
  else
    cp -R "${entry}" "${out}.tmp/"
  fi
done
install -m 0755 "${work}/win/embedded_tools/tools/test/tw.exe" "${work}/win/embedded_tools/tools/test/xml.exe" "${out}.tmp/tools/test/"
cat >"${out}.tmp/CUCINA_OVERLAY.txt" <<EOF
@bazel_tools overlay for Bazel ${version} on $(uname -s)/$(uname -m) (Cucina, ADR 0904).
Base: ${install_base}/embedded_tools
Added: tools/test/tw.exe, tools/test/xml.exe from ${asset} (sha256 ${sha256}).
EOF
rm -rf "${out}"
mv "${out}.tmp" "${out}"

echo "# Windows tests from this $(uname -s) client (R-XPLAT-4); add to user.bazelrc:"
echo "common --override_repository=bazel_tools=${out}"
