<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Upstream patches

Cucina runs unmodified upstream components (Buildbarn, Bazel, hermetic-llvm, Tart, and the Rust and Go libraries it builds on). When an upstream bug or gap blocks us, we do two things: carry the **smallest possible patch** so that we can keep working, and write it up so that the upstream
project can take it as it is. This directory is where those write-ups live. A carried patch is a debt: the goal is always to delete it once upstream has the fix.

## Layout

One directory per topic, named `<project>-<topic>` (for example `bazel-19209-windows-test-wrapper`):

```text
docs/upstream/<project>-<topic>/
  PR.md          the pull request or issue description, ready to paste upstream
  0001-*.patch   the change, as `git format-patch` output against the upstream ref named in PR.md
  EVIDENCE.md    optional: the reproduction and measurements behind the claim
```

## Rules

1. **Minimal and independent.** The patch changes only what the problem needs, applies to a named upstream tag or commit, and does not depend on anything Cucina-specific. Check that it applies: `git apply --check <patch>` in a clean checkout of that ref.
2. **Write for the maintainers, not for us.** `PR.md` has no Cucina names, no private details and no environment identifiers (account IDs, addresses, hostnames). It reads as if a stranger found the bug.
3. **A good description has these sections**: *Problem* (what goes wrong, for whom), *Reproduction* (exact steps and the versions used), *Change* (what the patch does and why this way), *Testing* (what you ran and what it showed, including a test added to the upstream project in its own style, if it has them),
   *Compatibility* (what else it could affect), and a link to the upstream issue if one exists.
4. **Licence.** Contributions are licensed under the upstream project's licence (Apache-2.0 for Buildbarn and Bazel). Follow the project's contribution rules (a CLA or a DCO sign-off, its code style, its test layout).
5. **Record the choice.** An ADR says why we carry a patch instead of waiting or working around it (for example [ADR 0023](../adr/0023-windows-tests-from-non-windows-clients.md)), and which of the two it is.

## How a carried patch is applied

Where the upstream is a Bazel module, apply the patch from `MODULE.bazel` (`single_version_override` or `archive_override` with `patches`); where it is a Go module, pin the commit in `go.mod` and carry the patch through a `replace` only if upstream has not merged it; for a tool we
download as a release binary, build the patched binary in the pinned tool definition and say so next to the pin. Whatever the mechanism, the pin names the exact upstream ref and the patch file, so the debt is visible in one place.

## Lifecycle

| Stage | What happens |
| --- | --- |
| Draft | The patch and `PR.md` exist here; we use the patch locally |
| Filed | Upstream issue or pull request opened; add its link to the table below |
| Merged | The change is in an upstream release; bump the pin, delete the carried patch and its directory, and note it in the ADR |
| Declined | Keep the patch, record why upstream declined, and revisit when the project or our needs change |

Check the table at every Bazel or Buildbarn upgrade ([Buildbarn upgrade](../operations/buildbarn-upgrade.md)): a release may have made a patch unnecessary, or made it stop applying.

## Index

| Topic | Upstream | Status |
| --- | --- | --- |
| Windows test wrapper for non-Windows Bazel clients (`bazel-19209-windows-test-wrapper`): fetch `tw.exe`/`xml.exe` on non-Windows hosts; `_CRT_RAND_S` before `<windows.h>` in the launcher | bazelbuild/bazel#19209 | Draft. Not carried: Cucina uses the `@bazel_tools` overlay instead ([ADR 0904](../adr/0904-windows-tests-bazel-tools-overlay.md)) |

Add a row when you add a directory.
