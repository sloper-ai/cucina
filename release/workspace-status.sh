#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
# Bazel workspace status for release builds (R-OPS-7, ADR 0150):
#
#   bazel build --stamp --workspace_status_command=release/workspace-status.sh //release:all
#
# STABLE_CUCINA_VERSION      the release version: $CUCINA_VERSION (dry runs) or the VERSION file
# STABLE_CUCINA_COMMIT       the source commit (empty outside a git checkout)
# STABLE_CUCINA_DIRTY        true for uncommitted/unknown sources (never published)
# Frozen source copies may set CUCINA_SOURCE_COMMIT + CUCINA_SOURCE_DIRTY + SOURCE_DATE_EPOCH;
# this does not make a dirty snapshot publishable. Never point a snapshot at another tree's .git.
# STABLE_CUCINA_SOURCE_DATE_EPOCH  the commit time (or $SOURCE_DATE_EPOCH): archive and image timestamps
# STABLE_CUCINA_REPOSITORY   OWNER/REPO the release is published from ($CUCINA_REPOSITORY,
#                            $GITHUB_REPOSITORY or sloper-ai/cucina)
#
# The values are validated again by `cucina-release buildinfo`; a bad version fails the build.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
version="${CUCINA_VERSION:-$(tr -d ' \t\r\n' <"$root/VERSION")}"
if [[ -n ${CUCINA_SOURCE_COMMIT:-} ]]; then
	commit="$CUCINA_SOURCE_COMMIT"
	dirty="${CUCINA_SOURCE_DIRTY:?a frozen snapshot must explicitly record whether its sources are dirty}"
	epoch="${SOURCE_DATE_EPOCH:?a frozen snapshot must supply the commit time}"
else
	commit="$(git -C "$root" rev-parse HEAD 2>/dev/null || true)"
	epoch="${SOURCE_DATE_EPOCH:-$(git -C "$root" log -1 --format=%ct 2>/dev/null || echo 0)}"
	dirty=true
	if [[ -n $commit ]] && changes="$(git -C "$root" status --porcelain --untracked-files=normal)" && [[ -z $changes ]]; then
		dirty=false
	fi
fi
repository="${CUCINA_REPOSITORY:-${GITHUB_REPOSITORY:-sloper-ai/cucina}}"

echo "STABLE_CUCINA_VERSION ${version}"
echo "STABLE_CUCINA_COMMIT ${commit}"
echo "STABLE_CUCINA_DIRTY ${dirty}"
echo "STABLE_CUCINA_SOURCE_DATE_EPOCH ${epoch}"
echo "STABLE_CUCINA_REPOSITORY ${repository}"
