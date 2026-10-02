#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
# T14 helper: publishes a package to a TEMPORARY, tagged, private S3 bucket and hands out pre-signed HTTPS URLs,
# so the scenario can verify the manifest URL + SHA-256 like an MDM would. Never touches account-level settings
# (S3 Block Public Access stays as it is; the bucket itself stays private: only pre-signed URLs work).
# Every bucket and object carries cucina:env=e2e, cucina:run=$CUCINA_RUN_ID, cucina:expires=$CUCINA_EXPIRES (§12);
# cleanup deletes only buckets whose tags match the current run.
#
# usage: publish-s3-temp.sh create --pkg FILE --version X.Y.Z [--expires-in SECONDS] [--state FILE]
#        publish-s3-temp.sh verify  [--state FILE]
#        publish-s3-temp.sh cleanup [--state FILE | --all-for-run]
# Requires: AWS CLI with a valid session (profile/region from .work/env.sh; region must be us-west-1),
# CUCINA_RUN_ID and CUCINA_EXPIRES. The state file (default ~/.config/cucina/t14/s3-publish.env, mode 0600) holds the
# pre-signed URLs; they embed temporary credentials, so they are never printed in full. Pre-signed URLs expire
# after --expires-in (default 6 h) or when the session credentials that signed them expire, whichever is first.
set -eu

. "$(dirname -- "$0")/lib.sh"
SCRIPTS=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)

[ $# -ge 1 ] || cucina_die "usage: publish-s3-temp.sh create|verify|cleanup [options] (try --help)"
cmd=$1
shift
pkg='' version='' expires_in=21600 all_for_run=0
state=${CUCINA_SECRETS_DIR:-$HOME/.config/cucina}/t14/s3-publish.env
while [ $# -gt 0 ]; do
	case $1 in
	--pkg) pkg=$2 && shift 2 ;;
	--version) version=$2 && shift 2 ;;
	--expires-in) expires_in=$2 && shift 2 ;;
	--state) state=$2 && shift 2 ;;
	--all-for-run) all_for_run=1 && shift ;;
	-h | --help) sed -n '2,/^set -eu/p' "$0" | sed '$d' && exit 0 ;;
	*) cucina_die "unknown argument: $1" ;;
	esac
done

cucina_need aws
region=${AWS_REGION:-${AWS_DEFAULT_REGION:-}}
[ "$region" = us-west-1 ] || cucina_die "AWS region must be us-west-1 (source .work/env.sh); got '${region:-unset}'"
if [ -z "${CUCINA_RUN_ID:-}" ] || [ -z "${CUCINA_EXPIRES:-}" ]; then
	cucina_die "CUCINA_RUN_ID and CUCINA_EXPIRES must be set (source .work/env.sh)"
fi
run_tag=$(printf '%s' "$CUCINA_RUN_ID" | tr '[:upper:]' '[:lower:]' | tr -c 'a-z0-9-' '-')
redact() { printf '%s' "$1" | sed 's/?.*/?<redacted>/'; }

# bucket_matches_run BUCKET — true if the bucket's tags say it belongs to this e2e run.
bucket_matches_run() {
	aws s3api get-bucket-tagging --region "$region" --bucket "$1" --query 'TagSet[].[Key,Value]' --output text 2>/dev/null |
		awk -F '\t' -v run="$CUCINA_RUN_ID" '$1 == "cucina:env" && $2 == "e2e" { e = 1 }
			$1 == "cucina:run" && $2 == run { r = 1 } END { exit !(e && r) }'
}

delete_bucket() {
	bucket_matches_run "$1" || cucina_die "bucket $1 is not tagged for run $CUCINA_RUN_ID; not deleting"
	aws s3 rm "s3://$1" --recursive --region "$region" --only-show-errors
	aws s3api delete-bucket --region "$region" --bucket "$1"
	cucina_info "deleted bucket $1"
}

case $cmd in
create)
	[ -f "$pkg" ] || cucina_die "--pkg FILE is required"
	cucina_validate_version "$version"
	case $expires_in in '' | *[!0-9]*) cucina_die "--expires-in must be seconds" ;; esac
	[ ! -f "$state" ] || cucina_die "$state exists (run cleanup first, or pass another --state)"
	bucket=cucina-e2e-pkg-$run_tag-$(openssl rand -hex 3)
	[ "${#bucket}" -le 63 ] || cucina_die "bucket name too long: $bucket"
	tagset="TagSet=[{Key=cucina:env,Value=e2e},{Key=cucina:run,Value=$CUCINA_RUN_ID},{Key=cucina:expires,Value=$CUCINA_EXPIRES}]"
	obj_tags="cucina%3Aenv=e2e&cucina%3Arun=$CUCINA_RUN_ID&cucina%3Aexpires=$(printf '%s' "$CUCINA_EXPIRES" | sed 's/:/%3A/g')"

	cucina_info "creating private bucket $bucket ($region)"
	aws s3api create-bucket --region "$region" --bucket "$bucket" \
		--create-bucket-configuration "LocationConstraint=$region" --object-ownership BucketOwnerEnforced >/dev/null
	cleanup_on_error() { aws s3 rm "s3://$bucket" --recursive --region "$region" --only-show-errors 2>/dev/null || true; aws s3api delete-bucket --region "$region" --bucket "$bucket" 2>/dev/null || true; }
	trap 'cleanup_on_error' EXIT
	aws s3api put-bucket-tagging --region "$region" --bucket "$bucket" --tagging "$tagset"
	aws s3api put-public-access-block --region "$region" --bucket "$bucket" --public-access-block-configuration \
		BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true

	base=$(cucina_asset_base "$version")
	prefix=cucina-host/$version
	pkg_url=$(aws s3 presign "s3://$bucket/$prefix/$base.pkg" --region "$region" --expires-in "$expires_in")
	manifest_url=$(aws s3 presign "s3://$bucket/$prefix/$base.plist" --region "$region" --expires-in "$expires_in")
	stage=$(mktemp -d "${TMPDIR:-/tmp}/cucina-s3.XXXXXX")
	cp "$pkg" "$stage/$base.pkg"
	"$SCRIPTS/make-manifest.sh" --pkg "$stage/$base.pkg" --version "$version" --url "$pkg_url" \
		--manifest-url "$manifest_url" --out-dir "$stage" >/dev/null
	for f in "$base.pkg:application/octet-stream" "$base.plist:application/xml" "$base.pkg.sha256:text/plain"; do
		aws s3api put-object --region "$region" --bucket "$bucket" --key "$prefix/${f%%:*}" --body "$stage/${f%%:*}" \
			--content-type "${f#*:}" --tagging "$obj_tags" --if-none-match '*' >/dev/null
	done
	sha=$(cucina_sha256 "$pkg")
	rm -rf "$stage"

	mkdir -p "$(dirname -- "$state")"
	chmod 0700 "$(dirname -- "$state")"
	umask 077
	cat >"$state" <<EOF
BUCKET=$bucket
REGION=$region
VERSION=$version
SHA256=$sha
BUNDLE_ID=$CUCINA_PKG_ID
PKG_URL=$pkg_url
MANIFEST_URL=$manifest_url
EXPIRES_AT=$(date -u -v+"${expires_in}"S +%Y-%m-%dT%H:%M:%SZ)
EOF
	trap - EXIT
	cucina_info "uploaded; state in $state"
	"$SCRIPTS/verify-manifest.sh" "$manifest_url" --expect-version "$version" --expect-sha256 "$sha"
	printf 'bucket: %s\npkg URL: %s\nmanifest URL: %s\nSHA-256: %s\nbundle ID: %s\n' "$bucket" \
		"$(redact "$pkg_url")" "$(redact "$manifest_url")" "$sha" "$CUCINA_PKG_ID"
	;;
verify)
	[ -f "$state" ] || cucina_die "no state file $state"
	cucina_load_pins "$state"
	# shellcheck disable=SC2153 # MANIFEST_URL, VERSION, SHA256 come from the state file
	"$SCRIPTS/verify-manifest.sh" "$MANIFEST_URL" --expect-version "$VERSION" --expect-sha256 "$SHA256"
	;;
cleanup)
	if [ "$all_for_run" = 1 ]; then
		for b in $(aws s3api list-buckets --query "Buckets[?starts_with(Name, 'cucina-e2e-pkg-')].Name" --output text); do
			if bucket_matches_run "$b"; then delete_bucket "$b"; fi
		done
	else
		[ -f "$state" ] || cucina_die "no state file $state (use --all-for-run to sweep by tags)"
		cucina_load_pins "$state"
		# shellcheck disable=SC2153 # BUCKET comes from the state file
		delete_bucket "$BUCKET"
	fi
	rm -f "$state"
	;;
*) cucina_die "unknown command: $cmd (create|verify|cleanup)" ;;
esac
