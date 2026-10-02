# SPDX-License-Identifier: FSL-1.1-ALv2
"""Public macros of @cucina_platforms."""

load(":catalog.bzl", "RUNNERS")

def cucina_test_exec_platform(name, target_platform, pool, runner, constraint_values = [], **kwargs):
    """Declares a test exec platform ("twin") for your own target platform (R-XPLAT-2b).

    Bazel 9's default test toolchain runs a test on the first execution platform that satisfies
    every constraint of the target platform. The generated `@cucina_platforms//test:test_on_<P>`
    platforms cover hermetic-llvm's platforms; a custom target platform (for example a libstdc++
    variant) needs its own twin, appended to --extra_execution_platforms after the compile exec
    platforms:

        platform(
            name = "linux_x86_64_gnu.2.28_libstdcxx",
            constraint_values = [
                "@platforms//os:linux",
                "@platforms//cpu:x86_64",
                "@llvm//constraints/libc:gnu.2.28",
                "@llvm//constraints/cxxstdlib:libstdcxx.17.0.0",
            ],
        )

        cucina_test_exec_platform(
            name = "test_on_linux_x86_64_gnu.2.28_libstdcxx",
            target_platform = ":linux_x86_64_gnu.2.28_libstdcxx",
            pool = "linux-x86-64",
            runner = "native",
        )

    Args:
      name: the platform's name.
      target_platform: label of the target platform whose constraints the twin inherits.
      pool: a pool platform of platforms/pools.json, e.g. "linux-x86-64".
      runner: a runner of that pool, e.g. "native" or "qemu-rv64g".
      constraint_values: extra constraint values (normally none).
      **kwargs: forwarded to platform() (visibility, tags, ...).
    """
    runners = RUNNERS.get(pool)
    if runners == None:
        fail("cucina_test_exec_platform(%s): unknown pool %r; known pools: %s" % (name, pool, sorted(RUNNERS.keys())))
    properties = runners.get(runner)
    if properties == None:
        fail("cucina_test_exec_platform(%s): pool %r has no runner %r; runners: %s" % (name, pool, runner, sorted(runners.keys())))
    native.platform(
        name = name,
        parents = [target_platform],
        constraint_values = constraint_values,
        exec_properties = properties,
        **kwargs
    )
