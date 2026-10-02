#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
# Shared helpers of the release scripts (sourced, not executed).
#
# Environment:
#   BAZEL               bazel launcher (default: bazelisk if on PATH, else bazel)
#   BAZEL_STARTUP_ARGS  startup options, e.g. "--output_base=/path" (word-split)
#   BAZEL_ARGS          extra build options, e.g. "--jobs=4 --config=ci" (word-split)
#   CUCINA_VERSION      version to stamp instead of VERSION (dry runs)

release_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

die() {
	echo "release: $*" >&2
	exit 1
}
info() { echo "release: $*" >&2; }

if [[ -z ${BAZEL:-} ]]; then
	if command -v bazelisk >/dev/null 2>&1; then BAZEL=bazelisk; else BAZEL=bazel; fi
fi

# bzl <command> [args...]: Bazel with the release flags: stamping (ADR 0150), optimised and
# stripped binaries (ADR 0151). BAZEL_ARGS come last, so they can override these. Convenience
# symlinks are left alone: several output bases may share this workspace.
bzl() {
	local cmd=$1
	shift
	# shellcheck disable=SC2086 # BAZEL_STARTUP_ARGS / BAZEL_ARGS are word lists
	(cd "$release_root" && "$BAZEL" ${BAZEL_STARTUP_ARGS:-} "$cmd" \
		--stamp --workspace_status_command=release/workspace-status.sh \
		--compilation_mode=opt --strip=always \
		--experimental_convenience_symlinks=ignore ${BAZEL_ARGS:-} "$@")
}

# outputs <target>: the files of a target, as absolute paths.
outputs() {
	local execroot
	execroot="$(bzl info execution_root 2>/dev/null)"
	bzl cquery --output=files "$1" 2>/dev/null | while read -r f; do printf '%s/%s\n' "$execroot" "$f"; done
}

# tool <args...>: the release tool (//bazel/release/tool) built for this host.
tool() {
	local bin
	bzl build //bazel/release/tool >&2 || die "cannot build //bazel/release/tool"
	bin="$(outputs //bazel/release/tool | head -n 1)"
	"$bin" "$@"
}

# version_of <dist-or-release dir>: the version a build was stamped with.
version_of() {
	sed -n 's/^ *"version": "\(.*\)",$/\1/p' "$1/meta/buildinfo.json"
}

host_os() {
	case "$(uname -s)" in
	Darwin) echo macos ;;
	Linux) echo linux ;;
	*) echo other ;;
	esac
}
