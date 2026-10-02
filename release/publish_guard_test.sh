#!/bin/bash
# SPDX-License-Identifier: FSL-1.1-ALv2
# R-OPS-7: dry runs have no publishing effects; dirty/unstamped/non-tag builds cannot publish.
# Run ONLY as //release:publish_guard_test (block-network sandbox), never on the host.
# The publisher is a local fake with a file as durable state. No credentials are inherited.
# shellcheck disable=SC2016 # variables in the fake and child scripts expand only in the child
set -euo pipefail
[[ -n ${TEST_SRCDIR:-} && -n ${TEST_TMPDIR:-} ]] || exit 2
publish="$PWD/${1:?publish script}"
work="$TEST_TMPDIR"
mkdir -p "$work/bin" "$work/home" "$work/config" "$work/release/meta" "$work/release/assets"
printf '#!/bin/sh\n# SPDX-License-Identifier: FSL-1.1-ALv2\nprintf published > "$FAKE_PUBLISHED"\n' > "$work/bin/gh"
chmod +x "$work/bin/gh"
printf 'Draft notes\n' > "$work/release/meta/release-notes.md"
printf 'asset\n' > "$work/release/assets/example.txt"

for scenario in dry-run dirty unstamped unapproved non-tag approved; do
	dirty=false stamped=true approval=1 event=push args=()
	case "$scenario" in
	dry-run) dirty=true; args=(--dry-run) ;;
	dirty) dirty=true ;;
	unstamped) stamped=false ;;
	unapproved) approval=0 ;;
	non-tag) event=workflow_dispatch ;;
	esac
	printf '{\n  "version": "0.1.0",\n  "repository": "fixture-owner/fixture-repo",\n  "commit": "0000000000000000000000000000000000000000",\n  "prerelease": false,\n  "stamped": %s,\n  "dirty": %s\n}\n' \
		"$stamped" "$dirty" > "$work/release/meta/buildinfo.json"
	result=0
	/usr/bin/env -i PATH="$work/bin:/usr/bin:/bin" BASH_ENV=/dev/null \
		HOME="$work/home" XDG_CONFIG_HOME="$work/config" GH_CONFIG_DIR="$work/config/gh" \
		DOCKER_CONFIG="$work/config/docker" HELM_CONFIG_HOME="$work/config/helm" \
		FAKE_PUBLISHED="$work/published" FAKE_BIN="$work/bin/gh" CUCINA_PUBLISH="$approval" \
		GITHUB_ACTIONS=true GITHUB_EVENT_NAME="$event" GITHUB_REF=refs/tags/v0.1.0 \
		GITHUB_REPOSITORY=fixture-owner/fixture-repo GITHUB_SHA=0000000000000000000000000000000000000000 \
		/bin/bash -c '
			set -euo pipefail
			[[ $(command -v gh) == "$FAKE_BIN" ]] || exit 90
			for key in GH_TOKEN GITHUB_TOKEN GH_ENTERPRISE_TOKEN HOMEBREW_TAP_TOKEN AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY; do
				[[ -z ${!key:-} ]] || exit 91
			done
			exec /bin/bash "$@"
		' test "$publish" github --release "$work/release" ${args[@]+"${args[@]}"} > "$work/$scenario.log" 2>&1 || result=$?
	if [[ $scenario == approved ]]; then
		[[ $result == 0 && -e $work/published ]] || { printf 'valid fixture could not reach the isolated fake\n' >&2; exit 1; }
	elif [[ $scenario == dry-run ]]; then
		[[ $result == 0 && ! -e $work/published ]] || { printf 'dry run failed or changed the fake state\n' >&2; exit 1; }
	else
		[[ $result == 1 && ! -e $work/published ]] || { printf '%s: expected guard rejection, got exit %s\n' "$scenario" "$result" >&2; exit 1; }
	fi
done
