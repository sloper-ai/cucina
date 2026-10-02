# SPDX-License-Identifier: FSL-1.1-ALv2
"""Failure-path test of the tier-tag guard (R-TEST-8h; R-TEST-7: one test per guard).

`tier_checked` applies //bazel:tier_check.bzl%tier_tags_aspect to its target the
same way `.bazelrc` applies it to every top-level target; the analysis test
asserts that an untiered `*_test` is rejected with the documented message.
"""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("//bazel:tier_check.bzl", "tier_tags_aspect")

def _tier_checked_impl(_ctx):
    return []

tier_checked = rule(
    implementation = _tier_checked_impl,
    attrs = {"target": attr.label(aspects = [tier_tags_aspect])},
)

def _untiered_rejected_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, "has no tier-* tag")
    return analysistest.end(env)

untiered_rejected_test = analysistest.make(_untiered_rejected_impl, expect_failure = True)
