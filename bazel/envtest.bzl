# SPDX-License-Identifier: FSL-1.1-ALv2
"""envtest assets from runfiles (R-BUILD-1 "Kubernetes test assets", R-BUILD-5).

controller-tools `envtest-v1.36.2` archives (etcd, kube-apiserver, kubectl) are
pinned per OS/arch with the SHA-512 from envtest-releases.yaml in
tools/pinned.bzl (`@envtest//:binaries` selects the target platform's set).
`cucina_envtest_data` copies them into one directory (hardlinked) so that
`KUBEBUILDER_ASSETS` can point at it. envtest only uses loopback, so the tests
run sandboxed and remotely.

Prefer `cucina_go_test(tier = "integration", envtest = True)`, which adds
ENVTEST_DATA and envtest_env(<package>) for you.
"""

load("@bazel_lib//lib:copy_to_directory.bzl", "copy_to_directory")

ENVTEST_ASSETS = "//tools/envtest:assets"

ENVTEST_DATA = [ENVTEST_ASSETS]

def envtest_env(package):
    """KUBEBUILDER_ASSETS for a go_test declared in `package`.

    rules_go runs a go_test from its package directory inside the runfiles tree,
    and envtest execs `$KUBEBUILDER_ASSETS/etcd` relative to the working directory,
    so the path is relative to the test's package. Tests that chdir before
    starting envtest must set envtest.Environment.BinaryAssetsDirectory instead.

    Args:
      package: the test's package (native.package_name()).

    Returns:
      The env dict for the test.
    """
    up = "/".join([".."] * len(package.split("/"))) if package else "."
    return {"KUBEBUILDER_ASSETS": "%s/$(rootpath %s)" % (up, ENVTEST_ASSETS)}

# Assets exist for darwin_arm64, linux_amd64, linux_arm64 and windows_amd64.
ENVTEST_TARGET_COMPATIBLE_WITH = []

def cucina_envtest_data(name, visibility = None):
    """A directory with the platform's etcd, kube-apiserver and kubectl.

    Args:
      name: target name (a tree artifact; `$(rootpath :name)` is the directory).
      visibility: target visibility.
    """
    copy_to_directory(
        name = name,
        srcs = ["@envtest//:binaries"],
        include_external_repositories = ["*"],
        root_paths = ["."],
        visibility = visibility,
    )
