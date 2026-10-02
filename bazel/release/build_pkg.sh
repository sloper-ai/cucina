#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
# Action of cucina_host_pkg (//bazel/release:defs.bzl): builds the UNSIGNED host package with
# macos/pkg/scripts/build-pkg.sh and writes its MDM manifests with make-manifest.sh, named and
# addressed as GitHub Release assets of this version (ADR 0753, ADR 0151).
#
# usage: build_pkg.sh OUT_DIR BUILDINFO_ENV BUILD_PKG MAKE_MANIFEST HOSTD BB_STORAGE TART_TARBALL LICENSE NOTICES
set -eu

[ $# -eq 9 ] || {
	echo "usage: build_pkg.sh OUT_DIR BUILDINFO_ENV BUILD_PKG MAKE_MANIFEST HOSTD BB_STORAGE TART LICENSE NOTICES" >&2
	exit 2
}
abs() { case $1 in /*) printf '%s' "$1" ;; *) printf '%s/%s' "$PWD" "$1" ;; esac }

out=$(abs "$1")
# VERSION, CORE, RELEASE_URL, ... (validated values written by cucina-release buildinfo).
# shellcheck disable=SC1090
. "$(abs "$2")"
build_pkg=$(abs "$3")
make_manifest=$(abs "$4")
base=cucina-host-$(printf '%s' "$CORE" | tr . -)

work=$(mktemp -d "${TMPDIR:-/tmp}/cucina-pkg.XXXXXX")
trap 'rm -rf "$work"' EXIT INT TERM
mkdir -p "$out"

/bin/sh "$build_pkg" --hostd "$(abs "$5")" --bb-storage "$(abs "$6")" --tart-tarball "$(abs "$7")" \
	--license "$(abs "$8")" --notices "$(abs "$9")" --version "$CORE" --work "$work/stage" \
	--out "$out/$base.pkg" >"$work/build.log" 2>&1 || {
	cat "$work/build.log" >&2
	exit 1
}
/bin/sh "$make_manifest" --pkg "$out/$base.pkg" --version "$CORE" --url "$RELEASE_URL/$base.pkg" \
	--manifest-url "$RELEASE_URL/$base.plist" --out-dir "$out" >/dev/null
