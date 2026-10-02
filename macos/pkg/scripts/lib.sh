# SPDX-License-Identifier: FSL-1.1-ALv2
# shellcheck shell=sh disable=SC2034 # constants are used by the scripts that source this file
# Shared helpers for the build-side scripts in macos/pkg/scripts (sourced, never executed).
# POSIX sh; no bashisms (shellcheck -s sh). Host-side scripts (payload/) do not use this file.

# Bazel actions/tests run with a minimal PATH; pkgutil, lsbom and mkbom live in /usr/sbin.
PATH=$PATH:/usr/bin:/bin:/usr/sbin:/sbin
export PATH

# --- identifiers (PROMPT.md §3, R-MAC-10; contract in docs/dev/hostd.md §2) ---------------------
CUCINA_PKG_ID=ai.sloper.cucina.host
CUCINA_UNINSTALL_PKG_ID=ai.sloper.cucina.host.uninstall
CUCINA_LABEL=ai.sloper.cucina.hostd
CUCINA_PKG_TITLE="Cucina host agent"
CUCINA_PKG_VENDOR="Brwse Co."
CUCINA_MIN_MACOS=26.0

cucina_log() { printf '%s\n' "$*" >&2; }
cucina_info() { printf '==> %s\n' "$*" >&2; }
cucina_warn() { printf 'warning: %s\n' "$*" >&2; }
cucina_die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

cucina_need() {
	for cucina_need_cmd in "$@"; do
		command -v "$cucina_need_cmd" >/dev/null 2>&1 || cucina_die "required tool not found: $cucina_need_cmd"
	done
}

# Directory of macos/pkg (parent of scripts/).
cucina_pkg_dir() {
	CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd -P
}

# cucina_load_pins FILE — reads KEY=value lines (no expansion, no command substitution) and exports them.
cucina_load_pins() {
	[ -r "$1" ] || cucina_die "pins file not readable: $1"
	while IFS= read -r cucina_line || [ -n "$cucina_line" ]; do
		case $cucina_line in
		'' | '#'*) continue ;;
		*=*)
			cucina_key=${cucina_line%%=*}
			cucina_val=${cucina_line#*=}
			case $cucina_key in
			*[!A-Z0-9_]* | '') cucina_die "invalid key in $1: $cucina_key" ;;
			esac
			export "$cucina_key=$cucina_val"
			;;
		*) cucina_die "invalid line in $1: $cucina_line" ;;
		esac
	done <"$1"
}

cucina_sha256() {
	shasum -a 256 "$1" | cut -d ' ' -f 1
}

# cucina_verify_sha256 FILE EXPECTED — aborts on mismatch.
cucina_verify_sha256() {
	cucina_got=$(cucina_sha256 "$1")
	[ "$cucina_got" = "$2" ] || cucina_die "SHA-256 mismatch for $1: got $cucina_got, want $2"
}

# Package versions are plain SemVer cores: macOS Installer compares dotted numeric segments, so
# pre-release suffixes would break upgrade ordering (R-OPS-7 single version; ADR 0753).
cucina_validate_version() {
	case $1 in
	'' | *[!0-9.]* | .* | *. | *..*) cucina_die "version must be MAJOR.MINOR.PATCH, got '$1'" ;;
	esac
	cucina_dots=$(printf '%s' "$1" | tr -cd '.' | wc -c | tr -d ' ')
	[ "$cucina_dots" = 2 ] || cucina_die "version must be MAJOR.MINOR.PATCH, got '$1'"
}

# Published file base name: Apple Business asks for package URLs made of lowercase letters, digits and
# hyphens, one URL per version (ADR 0753). 0.1.0 -> cucina-host-0-1-0
cucina_asset_base() {
	printf 'cucina-host-%s' "$(printf '%s' "$1" | tr '.' '-')"
}

# Absolute path without requiring the file to exist (parent must exist).
cucina_abspath() {
	case $1 in
	/*) printf '%s' "$1" ;;
	*) printf '%s/%s' "$(CDPATH='' cd -- "$(dirname -- "$1")" && pwd -P)" "$(basename -- "$1")" ;;
	esac
}
