# SPDX-License-Identifier: FSL-1.1-ALv2
"""Local source location for the envtest inventory gate, never a remote input."""

def _envtest_source_root_impl(ctx):
    # Resolve in repository configuration, not by following a test runfile. Native
    # Windows runfiles can be copies/manifest entries rather than Unix symlinks.
    ctx.file("defs.bzl", "ENVTEST_SOURCE_ROOT = %r\n" % (str(ctx.workspace_root) + "/internal/envtest"))
    ctx.file("BUILD.bazel", 'exports_files(["defs.bzl"], visibility = ["//visibility:public"])\n')

envtest_source_root = repository_rule(
    implementation = _envtest_source_root_impl,
    configure = True,
    local = True,
)
