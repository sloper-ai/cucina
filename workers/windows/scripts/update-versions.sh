#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Copies the installed-version inventory downloaded from the base build (image-base.json) into the
# `installed` section of workers/windows/versions.json, so the exact MSVC/SDK/VS/WinFSP/Git/redist versions
# that Bazel pins (BAZEL_VC_FULL_VERSION, BAZEL_WINSDK_FULL_VERSION) are reviewable in the repo (R-VER-1).
#
#   update-versions.sh <image-base.json>
set -euo pipefail

inventory=${1:?usage: update-versions.sh <image-base.json>}
versions="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/versions.json"

# The file is written by Windows PowerShell (UTF-8 with BOM); decode it as utf-8-sig before jq sees it.
installed=$(python3 -c 'import json, sys; print(json.dumps(json.load(open(sys.argv[1], encoding="utf-8-sig"))))' "$inventory" | jq -c '{
    image_version,
    os,
    ec2launch,
    ssm_agent,
    vs_display_version: .toolchain.vs_display_version,
    vs_installation_version: .toolchain.vs_installation_version,
    vs_install_path: .toolchain.vs_install_path,
    msvc_version: .toolchain.msvc_version,
    cl_exe_version: .toolchain.cl_exe_version,
    windows_sdk_version: .toolchain.windows_sdk_version,
    vc_redist_x64: .components.vc_redist_x64,
    winfsp: .components.winfsp,
    git: .components.git,
    shawl: .components.shawl,
    bazelisk: .components.bazelisk,
    bazel_repo_env: .toolchain.bazel,
    disk_c_used_gib
  }')
tmp=$(mktemp "${versions}.XXXXXX")
trap 'rm -f "$tmp"' EXIT
jq --indent 2 --argjson i "$installed" '.installed = $i' "$versions" >"$tmp"
mv "$tmp" "$versions"
echo "update-versions: recorded MSVC $(jq -r '.msvc_version' <<<"$installed"), SDK $(jq -r '.windows_sdk_version' <<<"$installed")"
