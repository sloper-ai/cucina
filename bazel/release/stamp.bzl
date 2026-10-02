# SPDX-License-Identifier: FSL-1.1-ALv2
"""Version stamping for Cucina's binaries (R-OPS-7, ADR 0150).

Release builds run `bazel build --stamp --workspace_status_command=release/workspace-status.sh`,
which publishes STABLE_CUCINA_VERSION (from VERSION, or CUCINA_VERSION for dry runs) and
STABLE_CUCINA_COMMIT. Unstamped builds report DEV_VERSION everywhere, so a dev build can never
pass for a release. Owners of binaries wire their version variables with these helpers:

    load("//bazel/release:stamp.bzl", "go_version_x_defs")
    go_binary(name = "cucina-hostd", ..., x_defs = go_version_x_defs("main.version"))

    load("//bazel/release:stamp.bzl", "RUST_VERSION_ENV_FILES")
    rust_library(name = "cucinactl_lib", ..., rustc_env_files = RUST_VERSION_ENV_FILES, stamp = -1)
    # and in Rust: option_env!("CUCINA_VERSION").unwrap_or(env!("CARGO_PKG_VERSION"))
"""

DEV_VERSION = "0.0.0-dev"

_STAMPED = Label("//bazel/release:stamp")

def go_version_x_defs(version_var, commit_var = None):
    """rules_go `x_defs` that stamp the release version (and commit) into a Go binary.

    Args:
      version_var: the string variable for the version, e.g. "main.version" or
        "github.com/sloper-ai/cucina/internal/controller.Version".
      commit_var: optional string variable for the source commit.

    Returns:
      A select() for the `x_defs` attribute.
    """
    stamped = {version_var: "{STABLE_CUCINA_VERSION}"}
    if commit_var:
        stamped[commit_var] = "{STABLE_CUCINA_COMMIT}"
    return select({
        _STAMPED: stamped,
        "//conditions:default": {version_var: DEV_VERSION},
    })

# rules_rust `rustc_env_files` setting CUCINA_VERSION; use with `stamp = -1` on the crate that
# reads it (the process wrapper substitutes {STABLE_CUCINA_VERSION} only in stamped builds).
RUST_VERSION_ENV_FILES = select({
    _STAMPED: [Label("//bazel/release:version_stamped.env")],
    "//conditions:default": [Label("//bazel/release:version_dev.env")],
})
