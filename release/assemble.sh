#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
# Assembles the release from the per-runner dists of ONE stamped build (R-OPS-7, ADR 0151):
# merges them (refusing mixed builds), writes SHA256SUMS and the Homebrew formula, checks every
# artifact and writes the release notes. The release workflow publishes what this produces;
# release/dry-run.sh runs it locally. It never publishes anything.
#
# usage: release/assemble.sh --out DIR [--pkg unsigned|none|signed:DIR] [--oci DIR] [--version V]
#                            [--signer-sha1 HEX] [--expect GROUP]... DIST_DIR...
#
#   --pkg unsigned   keep the Bazel-built unsigned host package (dry runs only)
#   --pkg none       drop the host package (a release without it: docs/operations/releasing.md)
#   --pkg signed:D   replace it with the signed package and manifests in D (CI signing job)
#   --oci DIR        OCI layouts (DIR/<image>/) to check against the chart and meta/images.json
#   --version V      the version the release must have (the tag without "v")
#   --expect GROUP   asset groups that must be present (default: macos linux, + pkg unless none)
#
# Output: DIR/assets/ (the GitHub Release assets incl. SHA256SUMS) and DIR/meta/
# (buildinfo.json, images.json, cucinactl.rb, release-notes.md).
set -euo pipefail
# shellcheck source=release/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

out='' pkg=unsigned oci='' version='' signer='' expects=() dists=()
while [[ $# -gt 0 ]]; do
	case $1 in
	--out) out=$2 && shift 2 ;;
	--pkg) pkg=$2 && shift 2 ;;
	--oci) oci=$2 && shift 2 ;;
	--version) version=$2 && shift 2 ;;
	--signer-sha1) signer=$2 && shift 2 ;;
	--expect) expects+=("$2") && shift 2 ;;
	-*) die "unknown option $1" ;;
	*) dists+=("$1") && shift ;;
	esac
done
[[ -n $out && ${#dists[@]} -gt 0 ]] || die "usage: assemble.sh --out DIR [options] DIST_DIR..."
[[ ${#expects[@]} -gt 0 ]] || expects=(macos linux)
abs() { (cd "$1" && pwd); }

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
staged=()
for i in "${!dists[@]}"; do
	d="$work/dist$i"
	cp -R "$(abs "${dists[$i]}")" "$d"
	chmod -R u+w "$d"
	staged+=("$d")
done

# The host package: never publish an unsigned one (R-MAC-9); dry runs keep it for the checks.
case $pkg in
unsigned) ;;
none | signed:*)
	for d in "${staged[@]}"; do rm -f "$d"/assets/cucina-host-*; done
	if [[ $pkg == signed:* ]]; then
		src="${pkg#signed:}"
		compgen -G "$src/cucina-host-*.pkg" >/dev/null || die "--pkg $pkg: no cucina-host-*.pkg in $src"
		mkdir -p "$work/signed/assets" "$work/signed/meta"
		cp "$src"/cucina-host-* "$work/signed/assets/"
		cp "${staged[0]}/meta/buildinfo.json" "$work/signed/meta/"
		staged+=("$work/signed")
	fi
	;;
*) die "--pkg must be unsigned, none or signed:DIR" ;;
esac
if [[ $pkg != none ]]; then expects+=(pkg); fi

rm -rf "$out"
mkdir -p "$out"
out="$(abs "$out")"
tool finalize --out "$out" --formula-template "$release_root/release/homebrew/cucinactl.rb.tmpl" "${staged[@]}"

args=(verify --dir "$out" --final)
for g in "${expects[@]}"; do args+=(--expect "$g"); done
[[ -z $version ]] || args+=(--version "$version")
if [[ $pkg == signed:* ]]; then args+=(--require-signed); fi
[[ -z $signer ]] || args+=(--signer-sha1 "$signer")
if [[ -n $oci ]]; then
	for layout in "$(abs "$oci")"/*/; do
		args+=(--oci "$(basename "$layout")=${layout%/}")
	done
fi
tool "${args[@]}"

commit="$(sed -n 's/^ *"commit": "\(.*\)",$/\1/p' "$out/meta/buildinfo.json")"
"$release_root/release/notes.sh" --version "$(version_of "$out")" --commit "$commit" >"$out/meta/release-notes.md"
if grep -q '"dirty": true' "$out/meta/buildinfo.json"; then
	printf '\nValidation build from dirty sources; not publishable. Commit identifies its baseline only.\n' >>"$out/meta/release-notes.md"
fi
info "assembled $(version_of "$out"): $out/assets ($(find "$out/assets" -type f | wc -l | tr -d ' ') files)"
