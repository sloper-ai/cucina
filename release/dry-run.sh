#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
# Release dry run (R-OPS-7, ADR 0151): builds every release artifact of the current commit
# with a fake version and runs every release check. It NEVER calls the publishing entrypoint:
# no tag, release, registry package or tap operation is reachable from this script.
#
# usage: release/dry-run.sh [--version V] [--out DIR] [--lint] [--no-all]
#
#   --version V  fake version to stamp (default 0.0.0-dryrun)
#   --out DIR    where to leave the artifacts (default: a new temporary directory)
#   --lint       also run actionlint and shellcheck over the workflows and release scripts
#   --no-all     skip the in-graph //release:all + //release:verify_test comparison (macOS)
#
# It runs the release workflow's build and assembly steps, all on a macOS host (Linux/Windows
# artifacts cross-build). On Linux use `release/build.sh linux --out DIR` for that lane. See
# release/lib.sh (BAZEL, BAZEL_STARTUP_ARGS, BAZEL_ARGS).
set -euo pipefail
# shellcheck source=release/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

version=0.0.0-dryrun out='' lint=0 all=1
while [[ $# -gt 0 ]]; do
	case $1 in
	--version) version=$2 && shift 2 ;;
	--out) out=$2 && shift 2 ;;
	--lint) lint=1 && shift ;;
	--no-all) all=0 && shift ;;
	*) die "unknown argument $1 (see the header of $0)" ;;
	esac
done
[[ $(host_os) == macos ]] || die "a complete release dry run needs macOS; for the Linux-only lane use release/build.sh linux --out DIR"
[[ -n $out ]] || out="$(mktemp -d "${TMPDIR:-/tmp}/cucina-release-dry-run.XXXXXX")"
mkdir -p "$out"
out="$(cd "$out" && pwd)"
export CUCINA_VERSION="$version"
started=$SECONDS

if [[ $lint == 1 ]]; then
	info "lint: actionlint + shellcheck"
	(cd "$release_root" && actionlint && shellcheck -x release/*.sh bazel/release/*.sh)
fi

"$release_root/release/build.sh" macos --out "$out/macos"
"$release_root/release/build.sh" linux --out "$out/linux"
"$release_root/release/assemble.sh" --out "$out/release" --pkg unsigned --oci "$out/linux/oci" \
	--version "$version" --expect macos --expect linux "$out/macos/dist-macos" "$out/linux/dist-linux"

if [[ $all == 1 ]]; then
	# The single-graph build (bazel build //release:all) must produce the same assets.
	"$release_root/release/build.sh" all --out "$out/all"
	if ! diff -u "$out/all/release/assets/SHA256SUMS" "$out/release/assets/SHA256SUMS"; then
		die "//release:all and the per-runner assembly disagree (see the diff above)"
	fi
	info "//release:all matches the per-runner assembly"
fi

info "dry run of $version passed in $((SECONDS - started)) s; nothing was published. Artifacts: $out/release"
