# SPDX-License-Identifier: FSL-1.1-ALv2
"""Structural guard for the test-tier policy (R-TEST-8h).

`tier_tags_aspect` is applied to every top-level target through `.bazelrc`
(`build --aspects=//bazel:tier_check.bzl%tier_tags_aspect`). It fails analysis
of any `*_test` rule in this repository that has no `tier-*` tag, so
`bazel build //...`/`bazel test //...` reject untiered tests. Declare tests with
the macros in //bazel:tiers.bzl (or pass `tags = ["tier-<name>"]` to third-party
test macros such as write_source_files).
"""

def _tier_tags_aspect_impl(target, ctx):
    kind = ctx.rule.kind
    if target.label.workspace_name == "" and kind.endswith("_test"):
        tags = getattr(ctx.rule.attr, "tags", None) or []
        if not [t for t in tags if t.startswith("tier-")]:
            fail(("%s (%s) has no tier-* tag. Declare tests with //bazel:tiers.bzl " +
                  "(cucina_go_test, cucina_rust_test, cucina_sh_test, cucina_scenario) " +
                  "or add tags = [\"tier-<static|unit|integration|simulation|system|acceptance>\"] " +
                  "(R-TEST-8h).") % (target.label, kind))
    return []

tier_tags_aspect = aspect(
    implementation = _tier_tags_aspect_impl,
    doc = "Fails analysis of *_test targets without a tier-* tag (R-TEST-8h).",
)
