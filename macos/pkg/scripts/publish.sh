#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
# `make pkg-publish`: uploads one signed package version and its manifest as versioned, IMMUTABLE artifacts to an
# unauthenticated HTTPS location (R-MAC-8, R-OPS-7), then verifies them as a Mac would (verify-manifest.sh).
#
#   --target github (default, production path, R-OPS-7): assets of the existing GitHub Release v<version> (created
#       by the tag-triggered release workflow). Asset URL https://github.com/<repo>/releases/download/v<ver>/<name>
#       answers 302 to a short-lived signed URL on release-assets.githubusercontent.com (macOS's downloader follows
#       it; whether Apple Business's own fetcher does is checked in MT-001, see ADR 0753). Existing assets are never
#       replaced (gh release upload without --clobber).
#   --target s3: objects under s3://<bucket>/<prefix>/<version>/ written with If-None-Match: * (no overwrite). The
#       bucket and its public read access (bucket policy or CloudFront) are set up by the operator; this script
#       never changes bucket policies or Block Public Access. URLs use --base-url (default: the bucket's
#       virtual-hosted S3 endpoint).
# Nothing is published unless this script is run explicitly; --dry-run prints the plan.
#
# usage: publish.sh --version X.Y.Z --pkg FILE [--uninstall-pkg FILE] [--signer-cert FILE] [--target github|s3] [--dry-run]
#                   [--repo OWNER/REPO] [--tag TAG] [--bucket NAME] [--prefix PATH] [--region R] [--base-url URL]
set -eu

. "$(dirname -- "$0")/lib.sh"
SCRIPTS=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)

target=github version='' pkg='' uninstall_pkg='' signer_cert='' dry_run=0
repo=sloper-ai/cucina tag='' bucket='' prefix=cucina-host region=${AWS_REGION:-us-west-1} base_url=''
while [ $# -gt 0 ]; do
	case $1 in
	--target) target=$2 && shift 2 ;;
	--version) version=$2 && shift 2 ;;
	--pkg) pkg=$2 && shift 2 ;;
	--uninstall-pkg) uninstall_pkg=$2 && shift 2 ;;
	--signer-cert) signer_cert=$2 && shift 2 ;;
	--dry-run) dry_run=1 && shift ;;
	--repo) repo=$2 && shift 2 ;;
	--tag) tag=$2 && shift 2 ;;
	--bucket) bucket=$2 && shift 2 ;;
	--prefix) prefix=$2 && shift 2 ;;
	--region) region=$2 && shift 2 ;;
	--base-url) base_url=${2%/} && shift 2 ;;
	-h | --help) sed -n '2,/^set -eu/p' "$0" | sed '$d' && exit 0 ;;
	*) cucina_die "unknown argument: $1 (try --help)" ;;
	esac
done
cucina_validate_version "$version"
[ -f "$pkg" ] || cucina_die "--pkg FILE is required"
[ -z "$uninstall_pkg" ] || [ -f "$uninstall_pkg" ] || cucina_die "no such file: $uninstall_pkg"
case $(pkgutil --check-signature "$pkg" 2>&1 | sed -n 's/^ *Status: //p') in
*signed*) ;;
*) cucina_die "refusing to publish an unsigned package (run scripts/sign.sh first)" ;;
esac
[ -n "$tag" ] || tag=v$version
base=$(cucina_asset_base "$version")
ubase=cucina-host-uninstall-$(printf '%s' "$version" | tr '.' '-')

stage=$(mktemp -d "${TMPDIR:-/tmp}/cucina-publish.XXXXXX")
trap 'rm -rf "$stage"' EXIT INT TERM
cp "$pkg" "$stage/$base.pkg"
[ -z "$uninstall_pkg" ] || cp "$uninstall_pkg" "$stage/$ubase.pkg"

case $target in
github)
	cucina_need gh
	pkg_url=https://github.com/$repo/releases/download/$tag/$base.pkg
	manifest_url=https://github.com/$repo/releases/download/$tag/$base.plist
	;;
s3)
	cucina_need aws
	[ -n "$bucket" ] || cucina_die "--target s3 needs --bucket"
	[ -n "$base_url" ] || base_url=https://$bucket.s3.$region.amazonaws.com
	pkg_url=$base_url/$prefix/$version/$base.pkg
	manifest_url=$base_url/$prefix/$version/$base.plist
	;;
*) cucina_die "--target must be github or s3" ;;
esac

"$SCRIPTS/make-manifest.sh" --pkg "$stage/$base.pkg" --version "$version" --url "$pkg_url" \
	--manifest-url "$manifest_url" --out-dir "$stage" >/dev/null
files="$base.pkg $base.plist $base.pkg.sha256 $base.json $base.ddm-package.json"
[ -z "$uninstall_pkg" ] || {
	(cd "$stage" && shasum -a 256 "$ubase.pkg") >"$stage/$ubase.pkg.sha256"
	files="$files $ubase.pkg $ubase.pkg.sha256"
}
# The signer's PUBLIC certificate, so administrators can build the trust profile (01-cucina-trust) and verify it.
if [ -n "$signer_cert" ]; then
	[ -f "$signer_cert" ] || cucina_die "no such file: $signer_cert"
	openssl x509 -in "$signer_cert" -noout >/dev/null 2>&1 || cucina_die "$signer_cert is not a PEM certificate"
	signer_sha1=$(openssl x509 -in "$signer_cert" -outform DER | shasum -a 1 | cut -c1-8)
	cp "$signer_cert" "$stage/cucina-host-signer-$signer_sha1.pem"
	files="$files cucina-host-signer-$signer_sha1.pem"
fi

cucina_info "publishing Cucina host $version to $target: $files"
if [ "$dry_run" = 1 ]; then
	printf 'dry-run: pkg URL %s\ndry-run: manifest URL %s\n' "$pkg_url" "$manifest_url"
	exit 0
fi

case $target in
github)
	gh release view "$tag" --repo "$repo" >/dev/null 2>&1 ||
		cucina_die "release $tag does not exist in $repo (the release workflow creates it)"
	existing=$(gh release view "$tag" --repo "$repo" --json assets --jq '.assets[].name')
	for f in $files; do
		if printf '%s\n' "$existing" | grep -qxF "$f"; then cucina_die "asset $f already exists in $tag (immutable)"; fi
	done
	# shellcheck disable=SC2086 # one file name per word
	(cd "$stage" && gh release upload "$tag" --repo "$repo" $files)
	;;
s3)
	for f in $files; do
		case $f in *.plist) ct=application/xml ;; *.json) ct=application/json ;; *.sha256 | *.pem) ct=text/plain ;; *) ct=application/octet-stream ;; esac
		aws s3api put-object --region "$region" --bucket "$bucket" --key "$prefix/$version/$f" --body "$stage/$f" \
			--content-type "$ct" --cache-control 'public, max-age=31536000, immutable' --if-none-match '*' >/dev/null ||
			cucina_die "upload of $prefix/$version/$f failed (it may already exist: versions are immutable)"
	done
	;;
esac
"$SCRIPTS/verify-manifest.sh" "$manifest_url" --expect-version "$version" --expect-sha256 "$(cucina_sha256 "$pkg")"
printf 'published %s\n  package:  %s\n  manifest: %s\n  SHA-256:  %s\n  bundle:   %s\n' \
	"$version" "$pkg_url" "$manifest_url" "$(cucina_sha256 "$pkg")" "$CUCINA_PKG_ID"
