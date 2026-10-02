<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# MSVC autodetection probe

Workspace used by `workers/windows/scripts/verify-bazel.sh` to prove that rules_cc's autodetected MSVC toolchain
works with the image's Visual Studio Build Tools (R-POOL-5). The `*.probe` files are renamed to
`MODULE.bazel`, `BUILD.bazel`, `.bazelrc` and `.bazelversion` on the test instance; they are stored under other
names so the repository's own Bazel build never loads this Windows-only package.
