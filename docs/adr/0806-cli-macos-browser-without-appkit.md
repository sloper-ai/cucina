<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0806 — cucinactl opens the browser with `open -u` on macOS (no AppKit)

* Status: accepted (2026-10-02)

## Context
R-LIB-3 names `webbrowser` (hardened) for `cucinactl login`. On macOS it calls NSWorkspace through `objc2-app-kit`, so the
binary links AppKit. Bazel links macOS Rust binaries against hermetic-llvm's trimmed macOS SDK (ADR 0102), which ships only
CoreFoundation, Foundation, Kernel, OSLog, Security and SystemConfiguration: `bazel build //cli/cucinactl` failed with
`framework not found for -framework AppKit`, and release binaries come from Bazel (R-BUILD-3).

## Decision
On macOS, `login` runs `/usr/bin/open -u <url>` (LaunchServices, the same default-handler lookup NSWorkspace performs) after
checking the URL is http(s); `webbrowser` stays the launcher on Linux and Windows (target-specific dependency). The URL is
always printed and `--browser-command` / `--no-browser` / `--manual` behave as before.

## Consequences
No AppKit (and no objc2-app-kit) in the macOS binary; it links with the hermetic SDK. If R-XPLAT-8's exec-side SDK makes
AppKit available, switching back is a one-line change.
