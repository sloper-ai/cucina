<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->

# MDM distribution of the Cucina host agent

Administrators start with the step-by-step guide [docs/macos/mac-mini-setup.md](../macos/mac-mini-setup.md). The pages
here are the reference behind it:

| Page | Covers |
| --- | --- |
| [pkg.md](pkg.md) | package contents and layout, pre/postinstall, upgrade and uninstall, building (make, Bazel), manifest and publishing (R-MAC-8, R-OPS-7) |
| [signing.md](signing.md) | private signing certificate (default), rotation, blast radius, consequences, optional Developer ID + notarization (R-MAC-9) |
| [profiles.md](profiles.md) | configuration-profile templates 01–07, `render.sh`, auto-login choice, FileVault, firewall, Local Network privacy (R-MAC-10) |
| [ddm.md](ddm.md) | declarative software updates (macOS 27), the package declaration, the background-tasks variant |
| [t14-kit.md](t14-kit.md) | T14: package, MDM simulation and enrollment in a throwaway VM, temporary HTTPS publication |

Sources: `macos/pkg/`, `macos/profiles/`, `macos/ddm/`. Decisions: ADRs 0750–0754. hostd's side of the contract
(paths, launchd label, preference keys): [docs/dev/hostd.md](../dev/hostd.md).
