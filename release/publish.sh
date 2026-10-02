#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
# Publishes an assembled release (release/assemble.sh output) — only from the tag-triggered
# release workflow (R-OPS-7, ADR 0151). Nothing is published without CUCINA_PUBLISH=1; with
# --dry-run every step prints what it would do (release/dry-run.sh exercises that).
#
# usage: release/publish.sh <step> --release DIR [step options] [--dry-run]
#
#   images   --oci DIR   push each OCI layout DIR/<image>/ to ghcr.io/<owner>/<image>:<version>
#                        with crane; the pushed digest must be the one in meta/images.json.
#                        Prints "<image-ref>@<digest>" per image (attestation subjects).
#   chart                helm push assets/cucina-<version>.tgz oci://ghcr.io/<owner>/charts;
#                        prints "ghcr.io/<owner>/charts/cucina@<digest>".
#   github               gh release create v<version> with assets/* and meta/release-notes.md
#                        (pre-release versions are marked as such).
#   homebrew --tap DIR   commit meta/cucinactl.rb as Formula/cucinactl.rb in a checkout of the
#                        tap repository and push it (final versions only).
#
# Credentials come from the environment: registry logins (crane, helm) and GH_TOKEN are set up
# by the workflow from GITHUB_TOKEN; the tap checkout carries HOMEBREW_TAP_TOKEN. Nothing here
# prints them.
set -euo pipefail
# shellcheck source=release/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

step="${1:-}"
shift || true
release='' oci='' tap='' dry=0
while [[ $# -gt 0 ]]; do
	case $1 in
	--release) release=$2 && shift 2 ;;
	--oci) oci=$2 && shift 2 ;;
	--tap) tap=$2 && shift 2 ;;
	--dry-run) dry=1 && shift ;;
	*) die "unknown argument $1" ;;
	esac
done
[[ -n $release && -f $release/meta/buildinfo.json ]] || die "usage: publish.sh <images|chart|github|homebrew> --release DIR [--dry-run]"
if [[ $dry == 0 && ${CUCINA_PUBLISH:-} != 1 ]]; then
	die "refusing to publish without CUCINA_PUBLISH=1 (only the tag-triggered release workflow publishes; R-OPS-7)"
fi

field() { sed -n "s/^ *\"$1\": \"\{0,1\}\([^\",]*\)\"\{0,1\},\{0,1\}$/\1/p" "$release/meta/buildinfo.json"; }
version="$(field version)"
repo="$(field repository)"
prerelease="$(field prerelease)"
owner="$(printf '%s' "${repo%%/*}" | tr '[:upper:]' '[:lower:]')"
[[ -n $version && -n $repo ]] || die "cannot read $release/meta/buildinfo.json"
if [[ $dry == 0 ]]; then
	[[ $(field stamped) == true && $(field dirty) == false ]] ||
		die "refusing to publish a dirty, unstamped or unknown-source build"
	[[ ${GITHUB_ACTIONS:-} == true && ${GITHUB_EVENT_NAME:-} == push && ${GITHUB_REF:-} == "refs/tags/v$version" ]] ||
		die "publishing is restricted to the tag-triggered GitHub Actions workflow"
	[[ ${GITHUB_REPOSITORY:-} == "$repo" && -n $(field commit) && ${GITHUB_SHA:-} == "$(field commit)" ]] ||
		die "release source does not match this workflow's repository and commit"
fi

# run <cmd...>: execute, or print in dry-run mode.
run() {
	if [[ $dry == 1 ]]; then
		printf 'dry-run:'
		printf ' %q' "$@"
		printf '\n'
	else
		"$@"
	fi
}

case $step in
images)
	[[ -d $oci ]] || die "images: --oci DIR is required"
	for layout in "$oci"/*/; do
		image="$(basename "$layout")"
		ref="ghcr.io/$owner/$image:$version"
		want="$(sed -n "s/^ *\"$image\": \"\(sha256:[0-9a-f]*\)\",\{0,1\}$/\1/p" "$release/meta/images.json")"
		[[ -n $want ]] || die "images: $image is not in meta/images.json"
		if [[ $dry == 1 ]]; then
			run crane push "${layout%/}" "$ref"
			echo "ghcr.io/$owner/$image@$want"
			continue
		fi
		pushed="$(crane push "${layout%/}" "$ref")"
		[[ $pushed == *"@$want" ]] || die "images: pushed $pushed, expected digest $want"
		echo "ghcr.io/$owner/$image@$want"
		# Anonymous pull = the package is public. New GHCR packages start private (first release).
		if ! DOCKER_CONFIG="$(mktemp -d)" crane manifest "$ref" >/dev/null 2>&1; then
			echo "::warning::ghcr.io/$owner/$image is not publicly readable: set the package visibility to public (docs/operations/releasing.md)" >&2
		fi
	done
	;;
chart)
	chart="$release/assets/cucina-$version.tgz"
	[[ -f $chart ]] || die "chart: $chart not found"
	if [[ $dry == 1 ]]; then
		run helm push "$chart" "oci://ghcr.io/$owner/charts"
		exit 0
	fi
	out="$(helm push "$chart" "oci://ghcr.io/$owner/charts" 2>&1)" || die "helm push failed: $out"
	digest="$(printf '%s\n' "$out" | sed -n 's/^Digest: *\(sha256:[0-9a-f]*\).*/\1/p' | head -n 1)"
	[[ -n $digest ]] || die "helm push printed no digest: $out"
	echo "ghcr.io/$owner/charts/cucina@$digest"
	if ! DOCKER_CONFIG="$(mktemp -d)" crane manifest "ghcr.io/$owner/charts/cucina:$version" >/dev/null 2>&1; then
		echo "::warning::ghcr.io/$owner/charts/cucina is not publicly readable: set the package visibility to public (docs/operations/releasing.md)" >&2
	fi
	;;
github)
	notes="$release/meta/release-notes.md"
	[[ -f $notes ]] || die "github: $notes not found"
	flags=(--repo "$repo" --verify-tag --title "Cucina $version" --notes-file "$notes")
	if [[ $prerelease == true ]]; then flags+=(--prerelease); fi
	assets=()
	while IFS= read -r f; do assets+=("$f"); done < <(find "$release/assets" -type f | sort)
	run gh release create "v$version" "${flags[@]}" "${assets[@]}"
	;;
homebrew)
	[[ $prerelease != true ]] || {
		info "homebrew: $version is a pre-release; the tap keeps the last final version"
		exit 0
	}
	[[ -d $tap/.git ]] || die "homebrew: --tap DIR must be a checkout of the tap repository"
	run mkdir -p "$tap/Formula"
	run cp "$release/meta/cucinactl.rb" "$tap/Formula/cucinactl.rb"
	run git -C "$tap" add Formula/cucinactl.rb
	if [[ $dry == 0 ]] && git -C "$tap" diff --cached --quiet; then
		info "homebrew: Formula/cucinactl.rb is already at $version"
		exit 0
	fi
	run git -C "$tap" -c user.name="cucina-release" -c user.email="cucina-release@users.noreply.github.com" \
		commit -m "cucinactl $version"
	run git -C "$tap" push origin HEAD
	;;
*) die "unknown step '$step' (images, chart, github, homebrew)" ;;
esac
