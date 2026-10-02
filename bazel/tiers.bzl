# SPDX-License-Identifier: FSL-1.1-ALv2
"""Test-tier policy as Bazel symbolic macros (R-TEST-2, R-TEST-8h).

Every test in this repository declares exactly one tier. The tier sets the
Bazel `size` (and with it the default timeout), the tags (`tier-<name>` plus
`manual`/`requires-docker`/`no-remote-exec` where the tier needs them) and, for
`integration`, optional envtest assets. Select tiers on the command line with
`--test_tag_filters=tier-unit` etc. See docs/dev/bazel.md.

    load("//bazel:tiers.bzl", "cucina_go_test")

    cucina_go_test(
        name = "plan_test",
        srcs = ["plan_test.go"],
        embed = [":scaling"],
        tier = "unit",
    )

The `//bazel:tier_check.bzl%tier_tags_aspect` aspect (enabled in .bazelrc)
rejects any `*_test` target that lacks a `tier-*` tag.
"""

load("@rules_go//go:def.bzl", "go_test")
load("@rules_rs//rs:rust_test.bzl", "rust_test")
load("@rules_shell//shell:sh_test.bzl", "sh_test")
load("//bazel:envtest.bzl", "ENVTEST_DATA", "ENVTEST_TARGET_COMPATIBLE_WITH", "envtest_env")

# Tier -> Bazel test attributes. Sizes follow R-TEST-2; timeouts default from
# the size (small=60 s, medium=300 s, large=900 s, enormous=3600 s) and may only
# be tightened per target.
TIERS = {
    # Lint/check targets: buildifier, gitleaks, diff tests of checked-in
    # generated code and goldens, buf lint, kubeconform, ...
    "static": struct(size = "small", tags = []),
    # Pure cores; < 1 s, no I/O, no sleeps, one process.
    "unit": struct(size = "small", tags = []),
    # Real components against fakes; < 30 s, localhost only.
    "integration": struct(size = "medium", tags = []),
    # Deterministic controller simulation (nightly sweep).
    "simulation": struct(size = "large", tags = ["manual"]),
    # kind lane: the only tier allowed to use Docker; never on RBE.
    "system": struct(size = "large", tags = ["manual", "requires-docker", "no-remote-exec"]),
    # The real AWS + Mac campaign (§10) as e2e scenarios.
    "acceptance": struct(size = "enormous", tags = ["manual", "no-remote-exec"]),
}

TIER_NAMES = sorted(TIERS.keys())

def tier_tags(tier, tags = None):
    """Returns the tag list for a test of `tier`, merged with caller `tags`.

    Args:
      tier: one of TIER_NAMES.
      tags: extra caller tags (may be None).

    Returns:
      A sorted, de-duplicated list of tags.
    """
    if tier not in TIERS:
        fail("unknown test tier %r; expected one of %s" % (tier, TIER_NAMES))
    tags = list(tags or [])
    if "exclusive" in tags:
        fail("tag 'exclusive' disables remote execution; use 'exclusive-if-local' (R-TEST-8h)")
    for t in tags:
        if t.startswith("tier-"):
            fail("tag %r: the tier is set by the `tier` attribute, not by tags" % t)
    return sorted({t: None for t in tags + TIERS[tier].tags + ["tier-" + tier]}.keys())

def _plus(value, extra):
    """`value + extra` for possibly-None / select() attribute values."""
    if value == None:
        return extra
    return value + extra

def _merge_env(value, extra):
    if value == None:
        return extra
    return value | extra

def _envtest_kwargs(tier, envtest, data, env, target_compatible_with):
    if not envtest:
        return {}
    if tier != "integration":
        fail("envtest assets are only available to tier = \"integration\" tests")
    return {
        "data": _plus(data, ENVTEST_DATA),
        "env": _merge_env(env, envtest_env(native.package_name())),
        "target_compatible_with": _plus(target_compatible_with, ENVTEST_TARGET_COMPATIBLE_WITH),
    }

_TIER_ATTRS = {
    "tier": attr.string(
        mandatory = True,
        configurable = False,
        values = TIER_NAMES,
        doc = "Test tier (R-TEST-2): static, unit, integration, simulation, system or acceptance.",
    ),
    # The tier owns the size; use `timeout` to tighten a single target.
    "size": None,
}

def _package_relative_env(runfiles_env):
    """{VAR: label} -> ({VAR: path valid from the go_test's working directory}, [labels]).

    rules_go runs a go_test from its package directory inside the runfiles tree,
    while $(rootpath) is relative to the runfiles root of the main repository.
    """
    if not runfiles_env:
        return {}, []
    package = native.package_name()
    up = "/".join([".."] * len(package.split("/"))) if package else "."
    env = {}
    for var, label in runfiles_env.items():
        env[var] = "%s/$(rootpath %s)" % (up, label)
    return env, list(runfiles_env.values())

def _cucina_go_test_impl(name, visibility, tier, envtest, runfiles_env, tags, data, env, target_compatible_with, pure, **kwargs):
    rf_env, rf_data = _package_relative_env(runfiles_env)
    if rf_env:
        data = _plus(data, rf_data)
        env = _merge_env(env, rf_env)
    extra = _envtest_kwargs(tier, envtest, data, env, target_compatible_with)
    go_test(
        name = name,
        visibility = visibility,
        size = TIERS[tier].size,
        tags = tier_tags(tier, tags),
        data = extra.get("data", data),
        env = extra.get("env", env),
        target_compatible_with = extra.get("target_compatible_with", target_compatible_with),
        # Pure Go unless the test opts into cgo (R-BUILD-2).
        pure = pure if pure != None else "on",
        **kwargs
    )

cucina_go_test = macro(
    doc = "A rules_go `go_test` with a mandatory test tier (R-TEST-8h). Gazelle emits it via map_kind.",
    implementation = _cucina_go_test_impl,
    inherit_attrs = go_test,
    attrs = _TIER_ATTRS | {
        "envtest": attr.bool(
            default = False,
            configurable = False,
            doc = "Add envtest assets (etcd, kube-apiserver, kubectl) and KUBEBUILDER_ASSETS (integration tier only).",
        ),
        "runfiles_env": attr.string_keyed_label_dict(
            configurable = False,
            doc = """Env vars naming runfiles, e.g. {"BB_STORAGE": "@bb_release//:bb_storage"}.
The labels are added to `data` and each variable gets a path that is valid from the
test's working directory (rules_go runs go_test from the package directory, so a
plain `$(rootpath ...)` in `env` would be wrong).""",
        ),
    },
)

def _cucina_rust_test_impl(name, visibility, tier, tags, **kwargs):
    rust_test(
        name = name,
        visibility = visibility,
        size = TIERS[tier].size,
        tags = tier_tags(tier, tags),
        **kwargs
    )

cucina_rust_test = macro(
    doc = "A rules_rs `rust_test` with a mandatory test tier (R-TEST-8h).",
    implementation = _cucina_rust_test_impl,
    inherit_attrs = rust_test,
    attrs = _TIER_ATTRS,
)

def _cucina_sh_test_impl(name, visibility, tier, tags, **kwargs):
    sh_test(
        name = name,
        visibility = visibility,
        size = TIERS[tier].size,
        tags = tier_tags(tier, tags),
        **kwargs
    )

cucina_sh_test = macro(
    doc = "A rules_shell `sh_test` with a mandatory test tier (R-TEST-8h).",
    implementation = _cucina_sh_test_impl,
    inherit_attrs = sh_test,
    attrs = _TIER_ATTRS,
)

def _cucina_scenario_impl(name, visibility, tier, scenario_id, requires, tags, args, pure, **kwargs):
    # Scenarios run against a real environment (§10); they never run on RBE and
    # never as part of a plain `bazel test //...`.
    go_test(
        name = name,
        visibility = visibility,
        size = TIERS[tier].size,
        tags = tier_tags(tier, (tags or []) + ["scenario"] + ["requires-" + r for r in requires]),
        args = _plus(args, ["--id=" + scenario_id] if scenario_id else []),
        pure = pure if pure != None else "on",
        **kwargs
    )

cucina_scenario = macro(
    doc = """An e2e scenario of the test/e2e harness (R-TEST-8d), wrapping a `go_test`.

Stub: the harness owner (test/e2e) fills in the runner. Defaults to the
`acceptance` tier; `requires` mirrors the scenario's declared prerequisites
(aws, mac-host, idp, destructive) as `requires-<x>` tags.""",
    implementation = _cucina_scenario_impl,
    inherit_attrs = go_test,
    attrs = {
        "tier": attr.string(
            default = "acceptance",
            configurable = False,
            values = ["simulation", "system", "acceptance"],
            doc = "Scenario tier; acceptance unless the scenario only needs fakes or kind.",
        ),
        "scenario_id": attr.string(configurable = False, doc = "Scenario ID (e.g. T3)."),
        "requires": attr.string_list(configurable = False, doc = "Prerequisites: aws, mac-host, idp, destructive."),
        "size": None,
    },
)
