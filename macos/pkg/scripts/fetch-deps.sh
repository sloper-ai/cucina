#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
# Downloads the pinned upstream artifacts for the host package (Tart release tarball, bb_storage darwin/arm64)
# into a cache directory and verifies their SHA-256 (R-MAC-8, §0.5). Network is used here, at BUILD time only;
# the package itself never downloads anything at install time.
#
# usage: fetch-deps.sh [--cache DIR] [--print]
#   --cache DIR  cache directory (default: $CUCINA_PKG_CACHE, else $CUCINA_DEV_STORAGE/downloads/pkg,
#                else ~/Library/Caches/cucina-pkg)
#   --print      print "TART_TARBALL=<path>" and "BB_STORAGE_BIN=<path>" lines on stdout
set -eu

. "$(dirname -- "$0")/lib.sh"
PKG_DIR=$(cucina_pkg_dir)
cucina_load_pins "$PKG_DIR/pins.env"

cache=${CUCINA_PKG_CACHE:-}
if [ -z "$cache" ]; then
	if [ -n "${CUCINA_DEV_STORAGE:-}" ]; then cache=$CUCINA_DEV_STORAGE/downloads/pkg; else cache=$HOME/Library/Caches/cucina-pkg; fi
fi
print=0
while [ $# -gt 0 ]; do
	case $1 in
	--cache)
		cache=$2
		shift 2
		;;
	--print)
		print=1
		shift
		;;
	-h | --help)
		sed -n '2,/^set -eu/p' "$0" | sed '$d'
		exit 0
		;;
	*) cucina_die "unknown argument: $1" ;;
	esac
done
cucina_need curl shasum
mkdir -p "$cache"

# fetch URL SHA256 DEST — downloads to a temporary name, verifies, then renames (never leaves a bad file).
fetch() {
	if [ -f "$3" ] && [ "$(cucina_sha256 "$3")" = "$2" ]; then
		return 0
	fi
	cucina_info "downloading $1"
	curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 --retry 3 -o "$3.part" "$1"
	cucina_verify_sha256 "$3.part" "$2"
	mv -f "$3.part" "$3"
}

tart_tarball=$cache/tart-$TART_VERSION.tar.gz
bb_storage=$cache/bb_storage-$BB_STORAGE_RELEASE.darwin_arm64
fetch "$TART_URL" "$TART_SHA256" "$tart_tarball"
fetch "$BB_STORAGE_URL" "$BB_STORAGE_SHA256" "$bb_storage"
chmod 0755 "$bb_storage"

if [ "$print" = 1 ]; then
	printf 'TART_TARBALL=%s\nBB_STORAGE_BIN=%s\n' "$tart_tarball" "$bb_storage"
fi
