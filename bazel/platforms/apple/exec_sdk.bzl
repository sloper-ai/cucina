# SPDX-License-Identifier: FSL-1.1-ALv2
"""The macOS SDK as a directory on the macOS exec machine (R-XPLAT-8).

hermetic-llvm's macOS toolchain takes its sysroot from `@macos_sdk//sysroot` (a DirectoryInfo):
`-isysroot {path}`, `--sysroot={path}`, the include-checker allowlist, the toolchain module map,
and rules_rs's SDKROOT. By default `@macos_sdk` downloads Apple's SDK on the *client* (the Bazel
host), so a Linux client would fetch it from Apple's CDN and upload it to the CAS.

Cucina replaces that repository with `exec_macos_sdk`, which points at
`/Applications/Xcode.app/.../SDKs/MacOSX<ver>.sdk`, the SDK inside the
pinned VM image of the macOS exec machine. The SDK itself is never fetched, never uploaded and
never part of the CAS, and Mac and Linux clients produce identical action keys.

Remote configurations pin the version of the compiling pool (`--@cucina_platforms//apple:sdk_version=27.0`,
emitted by `cucinactl bazelrc` and the :cucina configs): the versioned name `MacOSX27.0.sdk` puts the
SDK version into every compile/link command line and the input root, hence into the action key, and a
machine without that SDK fails the action (`no such sysroot directory`) instead of silently using
another one. The default (no version) is `MacOSX.sdk`, Xcode's unversioned link, so local builds work
on any Mac with Xcode at /Applications/Xcode.app.

How the pieces fit hermetic-llvm's `cc_sysroot` (`-isysroot {sysroot}`, `--sysroot={sysroot}`,
`allowlist_include_directories`, `data`) and rules_rs:
* `BuildSettingInfo.value` is the absolute SDK path. rules_cc formats a BuildSettingInfo as a raw
  string (a DirectoryInfo would become `%{path:...}`, which must be relative), so compile and link
  command lines carry `-isysroot /Applications/.../MacOSX<ver>.sdk`.
* `DirectoryInfo.path` is the same absolute path: it becomes an absolute builtin include directory,
  so Bazel's include checker accepts the SDK headers clang reports in its `.d` files. No files.
* `DefaultInfo` holds one unresolved symlink artifact `MacOSX<ver>.sdk -> <absolute path>`: rules_rs
  needs exactly one file for SDKROOT (`${pwd}/<symlink>`). It is a symlink node in the input root,
  never the SDK's contents (Buildbarn advertises absolute symlinks as ALLOWED).
"""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("@bazel_skylib//rules/directory:providers.bzl", "create_directory_info")

# Fixed toolchain paths of Cucina's macOS worker images (docs/operations/macos-images.md): exactly
# one Xcode, a real directory at /Applications/Xcode.app; the SDK directory has versioned links.
DEVELOPER_DIR = "/Applications/Xcode.app/Contents/Developer"
SDK_PATH_TEMPLATE = DEVELOPER_DIR + "/Platforms/MacOSX.platform/Developer/SDKs/MacOSX{version}.sdk"

def exec_sdk_path(version, override = ""):
    """Returns the absolute SDK path on the exec machine.

    Args:
      version: the SDK version, e.g. "27.0" (MacOSX27.0.sdk), or "" for the unversioned
        MacOSX.sdk of /Applications/Xcode.app (local builds on any Mac with Xcode there).
      override: an absolute SDK path that replaces the computed one (e.g. a Command Line Tools
        SDK, /Library/Developer/CommandLineTools/SDKs/MacOSX.sdk), or "".

    Returns:
      The absolute path of the SDK directory.
    """
    if override:
        if not override.startswith("/") or "{" in override or "}" in override:
            fail("@cucina_platforms//apple:sdk_path must be an absolute path, got %r" % override)
        return override
    if "/" in version or "}" in version or "{" in version:
        fail("invalid macOS SDK version %r (expected e.g. \"27.0\")" % version)
    return SDK_PATH_TEMPLATE.format(version = version)

def _exec_macos_sdk_impl(ctx):
    version = ctx.attr._sdk_version[BuildSettingInfo].value
    target = exec_sdk_path(version, ctx.attr._sdk_path[BuildSettingInfo].value)
    link = ctx.actions.declare_symlink(target.rsplit("/", 1)[-1])
    ctx.actions.symlink(
        output = link,
        target_path = target,
        progress_message = "Linking the exec-side macOS SDK %{output}",
    )
    return [
        DefaultInfo(files = depset([link])),
        BuildSettingInfo(value = target),
        create_directory_info(
            entries = {},
            # Deliberately empty: the SDK's headers exist only on the exec machine, so they are
            # neither inputs nor part of the toolchain module map.
            transitive_files = depset(),
            path = target,
            human_readable = "macOS SDK on the macOS exec machine ({})".format(target),
        ),
    ]

exec_macos_sdk = rule(
    implementation = _exec_macos_sdk_impl,
    doc = "The macOS SDK of the macOS exec machine, selected by @cucina_platforms//apple:sdk_version (or :sdk_path).",
    attrs = {
        "_sdk_path": attr.label(default = Label(":sdk_path")),
        "_sdk_version": attr.label(default = Label(":sdk_version")),
    },
)
