#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
# Release notes (R-OPS-7): the commit subjects since the previous release tag, grouped by
# conventional-commit type (other subjects are listed as they are), and the ADRs added since,
# filled into release/notes.md.tmpl. Needs the git history and tags (fetch-depth: 0 in CI).
#
# usage: release/notes.sh --version V [--previous-tag TAG] [--repo OWNER/REPO] > notes.md
set -euo pipefail
# shellcheck source=release/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

version='' previous='' repo="${CUCINA_REPOSITORY:-${GITHUB_REPOSITORY:-sloper-ai/cucina}}"
while [[ $# -gt 0 ]]; do
	case $1 in
	--version) version=$2 && shift 2 ;;
	--previous-tag) previous=$2 && shift 2 ;;
	--repo) repo=$2 && shift 2 ;;
	*) die "usage: notes.sh --version V [--previous-tag TAG] [--repo OWNER/REPO]" ;;
	esac
done
[[ -n $version ]] || die "--version is required"
tag="v$version"
core="${version%%-*}"
owner="$(printf '%s' "${repo%%/*}" | tr '[:upper:]' '[:lower:]')"
git_() { git -C "$release_root" "$@"; }

head="$(git_ rev-parse HEAD)"
if [[ -z $previous ]]; then
	previous="$(git_ describe --tags --abbrev=0 --match 'v[0-9]*.[0-9]*.[0-9]*' --exclude "$tag" HEAD 2>/dev/null || true)"
fi
range=HEAD
since="the beginning (first release)"
if [[ -n $previous ]]; then
	range="$previous..HEAD"
	since="[$previous](https://github.com/$repo/releases/tag/$previous)"
fi

changes="$(git_ log --no-merges --format='%h%x09%s' "$range" | awk -F '\t' '
	{
		s = $2; h = $1
		if (match(s, /^(feat|fix|perf|refactor|docs|test|build|ci|chore|revert)(\([^)]*\))?!?: /)) {
			type = substr(s, 1, RLENGTH); body = substr(s, RLENGTH + 1)
			sub(/(\(.*)?!?: $/, "", type)
			if (substr(s, 1, RLENGTH) ~ /!: $/) { sec = "Breaking changes" }
			else if (type == "feat") { sec = "Features" }
			else if (type == "fix") { sec = "Fixes" }
			else if (type == "perf") { sec = "Performance" }
			else { sec = "Other changes" }
		} else { sec = "Other changes"; body = s }
		n[sec]++
		if (n[sec] <= 200) { line[sec] = line[sec] "- " body " (" h ")\n" }
	}
	END {
		split("Breaking changes|Features|Fixes|Performance|Other changes", order, "|")
		for (i = 1; i <= 5; i++) {
			k = order[i]
			if (!n[k]) continue
			printf "### %s\n\n%s", k, line[k]
			if (n[k] > 200) printf "- … and %d more\n", n[k] - 200
			printf "\n"
		}
	}')"
[[ -n $changes ]] || changes="No changes."

if [[ -n $previous ]]; then
	adr_files="$(git_ diff --name-only --diff-filter=A "$previous" HEAD -- 'docs/adr/[0-9]*.md')"
else
	adr_files="$(git_ ls-files 'docs/adr/[0-9]*.md')"
fi
adrs=''
while IFS= read -r f; do
	[[ -n $f ]] || continue
	title="$(sed -n 's/^# *//p' "$release_root/$f" | head -n 1)"
	adrs+="- [${title:-$f}](https://github.com/$repo/blob/$tag/$f)"$'\n'
done <<<"$adr_files"
[[ -n $adrs ]] || adrs="No new ADRs."

export N_VERSION="$version" N_CORE_DASHED="${core//./-}" N_OWNER="$owner" N_REPOSITORY="$repo" N_TAG="$tag" \
	N_PREVIOUS="$since" N_CHANGES="${changes%$'\n'}" N_ADRS="${adrs%$'\n'}" \
	N_COMMIT_LINK="[\`${head:0:12}\`](https://github.com/$repo/commit/$head)"
# Fill @NAME@ fields in one pass (values may contain anything) and drop the template's comments.
awk '
	/^<!--.*-->$/ && NR <= 3 { next }
	{
		out = ""; rest = $0
		while (match(rest, /@[A-Z_]+@/)) {
			key = "N_" substr(rest, RSTART + 1, RLENGTH - 2)
			out = out substr(rest, 1, RSTART - 1) ((key in ENVIRON) ? ENVIRON[key] : substr(rest, RSTART, RLENGTH))
			rest = substr(rest, RSTART + RLENGTH)
		}
		print out rest
	}' "$release_root/release/notes.md.tmpl"
