#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
# Guards: R-MAC-8/R-SEC-3, §12 — the T14 runner refuses unsafe names/credential directories before side effects.
set -eu
PATH=$PATH:/usr/bin:/bin:/usr/sbin:/sbin
export PATH
if [ -n "${TEST_SRCDIR:-}" ]; then
	PKG="$TEST_SRCDIR/${TEST_WORKSPACE:-_main}/macos/pkg"
else
	PKG=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd -P)
fi
w=$(mktemp -d "${TEST_TMPDIR:-${TMPDIR:-/tmp}}/pkg-safety.XXXXXX")
w=$(CDPATH='' cd -- "$w" && pwd -P)
trap 'rm -rf "$w"' EXIT
mkdir -p "$w/bin"
# Empty in-memory fleet: a runner must reject bad inputs before it ever needs an image.
printf '#!/bin/sh\nprintf "[]\\n"\n' >"$w/bin/tart"
chmod +x "$w/bin/tart"
export PATH="$w/bin:$PATH" TART_HOME="$w/fleet"
# shellcheck disable=SC2016 # literal shell metacharacters must be rejected, not evaluated
for name in cucina-pkgtest-safety 'cucina-pkgtest-../escape' 'cucina-pkgtest-$(false)'; do
	if "$PKG/scripts/t14-vm.sh" run --vm "$name" --work "$w/work" >"$w/out" 2>&1; then
		printf 'FAIL: unsafe runner invocation succeeded\n' >&2
		exit 1
	fi
	if [ -e "$w/work" ]; then
		printf 'FAIL: runner created a credential workspace outside ~/.config/cucina\n' >&2
		exit 1
	fi
done
printf 'PASS: unsafe T14 invocations have no filesystem side effects\n'

# Guards: §12 — failed S3 tagging must never trigger an untagged destructive cleanup.
# Stateful CLI fake: creation succeeds, tagging fails, tag lookup refuses ownership; deletion leaves evidence.
cat >"$w/bin/aws" <<'EOF'
#!/bin/sh
case "$1 $2" in
's3api create-bucket') touch "$FAKE_AWS_STATE/created" ;;
's3api put-bucket-tagging' | 's3api get-bucket-tagging') exit 1 ;;
's3 rm' | 's3api delete-bucket') touch "$FAKE_AWS_STATE/deleted" ;;
*) exit 1 ;;
esac
EOF
chmod +x "$w/bin/aws"
mkdir -p "$w/home/.config/cucina" "$w/aws"
printf 'fixture\n' >"$w/input.pkg"
if HOME="$w/home" CUCINA_SECRETS_DIR="$w/home/.config/cucina" FAKE_AWS_STATE="$w/aws" \
	AWS_REGION=us-west-1 CUCINA_RUN_ID=test-run CUCINA_EXPIRES=2026-10-09T00:00:00Z \
	"$PKG/scripts/publish-s3-temp.sh" create --pkg "$w/input.pkg" --version 1.2.3 >"$w/s3.out" 2>&1; then
	printf 'FAIL: failed bucket tagging reported success\n' >&2; exit 1
fi
[ -e "$w/aws/created" ] || { printf 'FAIL: fake did not exercise creation\n' >&2; exit 1; }
[ ! -e "$w/aws/deleted" ] || { printf 'FAIL: cleanup deleted a bucket without verified ownership tags\n' >&2; exit 1; }
printf 'PASS: failed S3 tagging never authorizes deletion\n'

# Guards: R-MAC-9 — failed CI setup does not leave an unlocked signing keychain behind.
cat >"$w/bin/security" <<'EOF'
#!/bin/sh
case $1 in
create-keychain)
	for last in "$@"; do :; done
	touch "$last" ;;
delete-keychain) rm -f "$2" ;;
set-key-partition-list) exit 1 ;;
list-keychains)
	[ "${4:-}" = -s ] || printf '    "%s/Library/Keychains/login.keychain-db"\n' "$HOME" ;;
*) : ;;
esac
EOF
chmod +x "$w/bin/security"
printf 'fixture\n' >"$w/input.p12"
printf 'fixture\n' >"$w/input.pass"
if HOME="$w/home" "$PKG/scripts/ci-keychain.sh" create --p12 "$w/input.p12" --pass-file "$w/input.pass" \
	--keychain "$w/test.keychain-db" >"$w/keychain.out" 2>&1; then
	printf 'FAIL: failed keychain setup reported success\n' >&2; exit 1
fi
[ ! -e "$w/test.keychain-db" ] || { printf 'FAIL: failed setup left its signing keychain behind\n' >&2; exit 1; }
printf 'PASS: failed CI keychain setup cleans up its private identity\n'
