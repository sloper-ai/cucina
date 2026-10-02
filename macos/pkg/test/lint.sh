#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
# Static checks for macos/** (tier: static). Guards R-MAC-8 ("idempotent scripts, shellcheck-clean, POSIX") and
# R-MAC-10 ("plutil -lint all"): every shell script passes shellcheck as POSIX sh, every property list / profile
# template / LaunchDaemon plist passes plutil -lint, every DDM declaration is valid JSON with Type/Identifier/
# ServerToken/Payload, and every Cucina-authored source file carries the SPDX line (brief rule 8).
# usage: lint.sh   (SHELLCHECK=/path/to/shellcheck to pin the binary; CI installs shellcheck 0.11.0)
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd -P) # macos/
fails=0
bad() {
	printf 'FAIL  %s\n' "$*"
	fails=$((fails + 1))
}

SHELLCHECK=${SHELLCHECK:-shellcheck}
if ! "$SHELLCHECK" --version >/dev/null 2>&1; then
	for c in "$HOME"/.local/share/mise/installs/shellcheck/*/shellcheck-v*/shellcheck /opt/homebrew/bin/shellcheck; do
		if [ -x "$c" ]; then SHELLCHECK=$c && break; fi
	done
fi
"$SHELLCHECK" --version >/dev/null 2>&1 || {
	echo "lint.sh: shellcheck not found (set SHELLCHECK=...)" >&2
	exit 2
}

# Shell scripts: *.sh plus any file whose first line is a #!/bin/sh shebang (preinstall, postinstall, payload).
scripts=$(
	cd "$ROOT"
	find . -type f ! -path '*/build/*' | sort | while read -r f; do
		if [ "${f%.sh}" != "$f" ] || head -n 1 "$f" | grep -q '^#!/bin/sh'; then echo "$f"; fi
	done
)
for f in $scripts; do
	(cd "$ROOT" && "$SHELLCHECK" -s sh -x -P pkg/scripts "$f") || bad "shellcheck $f"
done

# The small kcpassword codec uses system Perl, not shell (no non-system runtime at install time).
perl -c "$ROOT/pkg/payload/cucina-kcpassword" >/dev/null 2>&1 || bad "kcpassword Perl syntax"

# Property lists (templates are valid plists by construction: placeholders live in strings, data and comments).
for f in $(cd "$ROOT" && find . -type f \( -name '*.mobileconfig' -o -name '*.plist' \) ! -path '*/build/*' | sort); do
	plutil -lint "$ROOT/$f" >/dev/null || bad "plutil -lint $f"
done

# DDM declarations (JSON).
for f in $(cd "$ROOT" && find ./ddm -type f -name '*.json' | sort); do
	plutil -convert xml1 -o /dev/null "$ROOT/$f" 2>/dev/null || {
		bad "invalid JSON $f"
		continue
	}
	for k in Type Identifier ServerToken Payload; do
		plutil -extract "$k" raw -o /dev/null "$ROOT/$f" 2>/dev/null || plutil -extract "$k" json -o /dev/null "$ROOT/$f" 2>/dev/null ||
			bad "$f lacks $k"
	done
done

# SPDX headers on Cucina-authored text sources (JSON cannot carry comments; it is exempt).
for f in $(cd "$ROOT" && find . -type f ! -name '*.json' ! -path '*/build/*' ! -name '.DS_Store' | sort); do
	head -n 4 "$ROOT/$f" | grep -q 'SPDX-License-Identifier: FSL-1.1-ALv2' || bad "missing SPDX line: $f"
done

[ "$fails" = 0 ] || {
	echo "lint.sh: $fails problem(s)" >&2
	exit 1
}
echo "lint.sh: ok ($(printf '%s\n' "$scripts" | grep -c .) shell scripts, profiles, plists, declarations)"
