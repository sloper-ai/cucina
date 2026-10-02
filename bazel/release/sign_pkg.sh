#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
# `bazel run //release:pkg_sign`: the local-only signing step of the macOS host package
# (R-MAC-9, R-BUILD-1 "macOS pkg"). It needs keychain access, so it never runs as a build
# action. It signs the same Bazel-built inputs as //release:host_pkg with
# macos/pkg/scripts/sign.sh and writes the package plus its MDM manifests for the GitHub
# Release of this version (ADR 0753):
#
#   bazel run --stamp --workspace_status_command=release/workspace-status.sh //release:pkg_sign -- \
#       --identity "Cucina Host Package Signing" [--keychain PATH] [--no-timestamp] --out DIR
#
# Every option except --out is passed to sign.sh (see its --help: --developer-id, ...).
set -euo pipefail

# bash 3.2 (macOS /bin/bash): no associative arrays.
buildinfo_env='' hostd='' bb_storage='' tart='' license='' notices='' sign_script='' make_manifest=''
signing=()
out=''
while [[ $# -gt 0 ]]; do
	case $1 in
	--buildinfo-env) buildinfo_env=$2 && shift 2 ;;
	--hostd) hostd=$2 && shift 2 ;;
	--bb-storage) bb_storage=$2 && shift 2 ;;
	--tart-tarball) tart=$2 && shift 2 ;;
	--license) license=$2 && shift 2 ;;
	--notices) notices=$2 && shift 2 ;;
	--sign-script) sign_script=$2 && shift 2 ;;
	--make-manifest) make_manifest=$2 && shift 2 ;;
	--out)
		out=$2
		shift 2
		;;
	*)
		signing+=("$1")
		shift
		;;
	esac
done
[[ -n $sign_script && -n $make_manifest && -n $buildinfo_env ]] ||
	{ echo "pkg_sign: run me with bazel run //release:pkg_sign" >&2; exit 2; }
[[ -n $out ]] || { echo "pkg_sign: --out DIR is required" >&2; exit 2; }
[[ $out == /* ]] || out="${BUILD_WORKING_DIRECTORY:-$PWD}/$out"
abs() { if [[ $1 == /* ]]; then printf '%s' "$1"; else printf '%s/%s' "$PWD" "$1"; fi; }

# VERSION, CORE, RELEASE_URL, ... (validated values written by cucina-release buildinfo).
# shellcheck disable=SC1090
. "$(abs "$buildinfo_env")"
base=cucina-host-${CORE//./-}
mkdir -p "$out"
/bin/sh "$(abs "$sign_script")" ${signing[@]+"${signing[@]}"} -- \
	--hostd "$(abs "$hostd")" --bb-storage "$(abs "$bb_storage")" --tart-tarball "$(abs "$tart")" \
	--license "$(abs "$license")" --notices "$(abs "$notices")" --version "$CORE" --out "$out/$base.pkg"
/bin/sh "$(abs "$make_manifest")" --pkg "$out/$base.pkg" --version "$CORE" \
	--url "$RELEASE_URL/$base.pkg" --manifest-url "$RELEASE_URL/$base.plist" --out-dir "$out" >/dev/null
plutil -convert json -o "$out/$base.manifest.json" "$out/$base.plist"
echo "signed $VERSION (package version $CORE): $out/$base.pkg (+ manifests)"
