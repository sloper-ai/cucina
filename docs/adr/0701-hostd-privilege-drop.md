<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0701 — hostd runs `tart` as `cucina` via `launchctl asuser` + a setuid trampoline

* Status: accepted (2026-10-02); root path verified only by MT-001

## Context
R-MAC-2: Virtualization.framework VMs fail when started as root and need an existing, unlocked login keychain, so `tart`
must run as the dedicated standard user `cucina` (auto-login, FileVault off). hostd itself must stay root (System keychain,
vmnet settings, connections to VM IPs exempt from Local Network privacy). Orchard drops privileges of its whole worker
process (`setregid`/`setreuid` + `HOME`); that is not possible for hostd. A process that only changes its uid stays in the
system bootstrap namespace and audit session, where securityd may not find the user's unlocked login keychain.
Secrets (`TART_REGISTRY_PASSWORD`) must not appear in argv (visible to all local users).

## Decision
Every `tart` invocation is wrapped (pure, unit-tested command construction in `internal/hostd/privdrop`):
`/bin/launchctl asuser <uid> /usr/local/cucina/bin/cucina-hostd drop-exec --uid <uid> --gid <gid> --groups <…> -- <tart> <args…>`.
`launchctl asuser` (root) adopts the user's Mach bootstrap namespace and security audit session; the hidden `drop-exec`
trampoline sets groups, gid and uid, verifies the drop is irreversible (`setuid(0)` must fail) and `exec`s tart with an
explicit environment (`HOME`, `USER`, `LOGNAME`, `PATH`, and `TART_REGISTRY_*` only for pulls). Long-running `tart run`
children start in their own session so a hostd crash or upgrade never kills VMs (they are re-adopted from `tart list`).
`--privdrop=setuid` (credentials applied by the child at fork, Orchard-style) is kept as a fallback for MT-001; user mode
(`--user-mode`) runs tart as the current user with no drop. hostd refuses to start in root mode if `RunAsUser` is missing.

## Consequences
* Verified now: argv/env construction (unit tests), the trampoline parser, user mode end to end on the dev Mac.
  Not verifiable here (no root on the dev Mac, no nested VMs): that `asuser` + uid drop reaches the unlocked keychain —
  MT-001 must run both modes on a real Mac mini and record which works; re-verify on every macOS release (26.4 changed
  login-keychain protection).
* The `cucina` user must be logged in (auto-login) before VMs can start; hostd reports start failures otherwise.
