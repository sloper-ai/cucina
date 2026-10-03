#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
# CI boundary: verified event commits, complete PR history, and path-selected lanes.
# Usage: change-context.sh <base-sha> <head-sha> [--check-tests]
# Stdout is a GitHub-output-compatible record; failures publish no selection.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
die() { printf 'change-context: %s\n' "$*" >&2; exit 2; }
[ "$#" -ge 2 ] && [ "$#" -le 3 ] || die 'expected base SHA, head SHA and optional --check-tests'
base="$1"
head="$2"
case "${3:-}" in '' | --check-tests) ;; *) die 'unknown option' ;; esac
for sha in "$base" "$head"; do
    [[ "$sha" =~ ^[0-9a-f]{40}$ ]] || die 'expected full lowercase commit SHA'
    resolved="$(git rev-parse --verify "${sha}^{commit}")" || die 'commit object is unavailable'
    [ "$resolved" = "$sha" ] || die 'SHA does not name the exact commit'
done
[ "$(git rev-parse --is-shallow-repository)" = false ] || die 'complete history is required (checkout fetch-depth: 0)'
git merge-base --is-ancestor "$head" HEAD || die 'event head is not part of the checked-out revision'
merge_base="$(git merge-base "$base" "$head")" || die 'commits have no common history'

if [ "${3:-}" = --check-tests ]; then
    # Reuse the hook's implementation, including fail-closed Git/classifier errors.
    "$here/../../.githooks/lib/test-change.sh" range "$base..$head"
fi

work="$(mktemp -d "${TMPDIR:-/tmp}/cucina-change-context.XXXXXX")"
trap 'rm -rf "$work"' EXIT
# Include both sides of a rename; preserve deleted paths and unusual filenames.
git -c core.quotepath=false -c diff.relative=false diff --no-ext-diff --no-textconv --no-renames \
    --name-only -z "$merge_base" "$head" -- >"$work/paths"
simulation=false
system=false
while IFS= read -r -d '' path; do
    case "$path" in
        .github/workflows/ci.yml | .github/workflows/nightly.yml | tools/ci/* | \
        .bazelrc | .bazelversion | BUILD.bazel | MODULE.bazel | MODULE.bazel.lock | \
        go.mod | go.sum | Cargo.toml | Cargo.lock | rust-toolchain.toml | mise.toml | \
        bazel/* | tools/pinned.bzl | api/v1alpha1/*)
            simulation=true
            system=true
            ;;
    esac
    case "$path" in
        sim/* | invariants/* | internal/scaling/* | internal/controller/* | \
        internal/reconcile/* | internal/hostd/* | internal/hostlink/* | \
        internal/workeragent/* | internal/providers/* | internal/ports/* | \
        internal/fakes/* | internal/domain/* | internal/buildqueue/* | internal/pools/* | \
        internal/config/* | internal/cost/* | internal/enroll/* | \
        cmd/cucina-controller/* | cmd/cucina-hostd/* | cmd/cucina-worker-agent/*)
            simulation=true
            ;;
    esac
    case "$path" in
        charts/* | api/crds/* | internal/controller/* | internal/reconcile/* | \
        internal/auth/* | internal/sts/* | internal/bbconfig/* | internal/canary/* | \
        cmd/cucina-controller/* | cmd/cucina-sts/* | release/kind-values.yaml | release/BUILD.bazel)
            system=true
            ;;
    esac
done <"$work/paths"
printf 'base=%s\nhead=%s\nmerge_base=%s\nsimulation=%s\nsystem=%s\n' \
    "$base" "$head" "$merge_base" "$simulation" "$system"
