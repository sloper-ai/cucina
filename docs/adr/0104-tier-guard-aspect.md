<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0104 — The tier-tag guard is an aspect, not a `bazel query` test

* Status: accepted (2026-10-02)

## Context
R-TEST-8h asks for "a structural test (`bazel query`) [that] fails any `*_test` without a
`tier-*` tag". A Bazel test cannot run `bazel query` on its own workspace (sandbox, server lock,
no workspace path), and `genquery` forbids wildcard patterns, so neither can enumerate `//...`.

## Decision
`//bazel:tier_check.bzl%tier_tags_aspect` is applied to every top-level target by `.bazelrc`
(`build --aspects=...`). It fails analysis of any main-repository `*_test` rule without a
`tier-*` tag, with a message naming the tier macros. `bazel build //...`/`bazel test //...`
therefore reject untiered tests before anything runs. `//tools:tier_tags_test` is the guard's
failure-path test (R-TEST-7): an analysis test asserting the aspect rejects an untagged fixture.
The tier macros (`//bazel:tiers.bzl`) make the tag automatic; Gazelle emits `cucina_go_test`.

## Consequences
Enforcement is exactly as wide as the targets being built: `manual` tests are only checked when
named explicitly (the manual tiers get their tags from the macros anyway). The check costs one
aspect evaluation per top-level target. For an ad-hoc inventory, the equivalent query is
`bazel query 'kind(".*_test", //...) except attr(tags, "tier-", //...)'`.
