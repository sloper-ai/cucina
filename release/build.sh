#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
# Builds one part of the release with Bazel (stamped), runs its release-lane check and copies
# the result out of bazel-out (R-BUILD-3, ADR 0151). The release workflow runs it per runner;
# release/dry-run.sh runs the same steps locally. It never publishes anything.
#
# usage: release/build.sh <macos|linux|all> --out DIR [--version V] [--no-test]
#
#   macos  //release:macos on a macOS host: DIR/dist-macos/ and DIR/pkg-inputs/ (CI signing inputs)
#   linux  //release:linux (any host): DIR/dist-linux/ and DIR/oci/<image>/ (OCI layouts)
#   all    //release:all on a macOS host: DIR/release/ (finalized) and DIR/oci/<image>/
#
# --version V stamps V instead of the VERSION file (dry runs); see release/lib.sh for BAZEL,
# BAZEL_STARTUP_ARGS and BAZEL_ARGS.
set -euo pipefail
# shellcheck source=release/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

part="${1:-}"
[[ $part == macos || $part == linux || $part == all ]] || die "usage: build.sh <macos|linux|all> --out DIR [--version V] [--no-test]"
shift
out='' test=1
while [[ $# -gt 0 ]]; do
	case $1 in
	--out) out=$2 && shift 2 ;;
	--version) export CUCINA_VERSION=$2 && shift 2 ;;
	--no-test) test=0 && shift ;;
	*) die "unknown argument $1" ;;
	esac
done
[[ -n $out ]] || die "--out DIR is required"
mkdir -p "$out"
out="$(cd "$out" && pwd)"
if [[ $part != linux && $(host_os) != macos ]]; then
	die "//release:$part builds macOS targets on macOS exec only (R-BUILD-1); use 'linux' here"
fi

info "building //release:$part (version ${CUCINA_VERSION:-$(tr -d '[:space:]' <"$release_root/VERSION")})"
bzl build "//release:$part"
if [[ $test == 1 ]]; then
	check="verify_${part}_test"
	[[ $part == all ]] && check=verify_test
	bzl test --test_output=errors "//release:$check"
fi

# copy_tree <target> <dest>: a single-directory output (dist, OCI layout) to dest.
copy_tree() {
	local src
	src="$(outputs "$1")"
	[[ -d $src ]] || die "$1: expected one directory output, got '$src'"
	rm -rf "$2"
	mkdir -p "$(dirname "$2")"
	cp -RL "$src" "$2"
	chmod -R u+w "$2"
}

case $part in
macos)
	copy_tree //release:dist_macos "$out/dist-macos"
	rm -rf "$out/pkg-inputs" && mkdir -p "$out/pkg-inputs"
	outputs //release:pkg_inputs | while read -r f; do cp -L "$f" "$out/pkg-inputs/$(basename "$f")"; done
	chmod -R u+w "$out/pkg-inputs"
	;;
linux)
	copy_tree //release:dist_linux "$out/dist-linux"
	;;
all)
	copy_tree //release:release "$out/release"
	;;
esac
if [[ $part != macos ]]; then
	copy_tree //release:controller_image "$out/oci/cucina-controller"
	copy_tree //release:sts_image "$out/oci/cucina-sts"
fi
info "done: $out"
