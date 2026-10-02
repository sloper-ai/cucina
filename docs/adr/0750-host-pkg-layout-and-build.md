<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->

# ADR 0750: Host package layout, setup helper and AppleDouble-free payloads

* Status: accepted (2026-10-02, agent `pkg`)
* Requirements: R-MAC-8, R-MAC-2, R-MAC-5, R-BUILD-1

## Context

The host package must carry hostd, bb_storage and Tart's untouched `tart.app`, prepare the host (user, power, DHCP
lease, directories), upgrade in place without touching VMs, caches or identity, and uninstall cleanly. Installer
packages run `preinstall`/`postinstall` once per install; administrators also need to re-run or inspect the same steps.
While building the first packages we found that `pkgbuild` archives every extended attribute of staged files as
AppleDouble `._name` payload entries, and macOS attaches the undeletable `com.apple.provenance` attribute to all files
written by provenance-tracked processes (terminal apps, IDEs, Bazel, CI agents): every payload entry got a `._` twin.

## Decision

* One component package (`ai.sloper.cucina.host`) in a distribution archive; everything under `/usr/local/cucina`
  (`bin/`, `tart.app`, `share/`), the LaunchDaemon in `/Library/LaunchDaemons`, a newsyslog rule under
  `/private/etc` (never `/etc`, a symlink). Paths follow hostd's contract (docs/dev/hostd.md §2). `tart.app` is
  non-relocatable and not version-checked: the package is authoritative, so rollbacks also roll Tart back.
* The postinstall logic lives in the payload as `/usr/local/cucina/bin/cucina-host-setup` (POSIX sh, idempotent,
  `--dry-run`); `postinstall` only execs it. The same tool offers `status`, `autologin --reset-password`, `hostname`,
  `ssh on|off` and `local-network`, and serves the DDM background-tasks variant (`--no-daemon --prefix`).
* State created at install time (`/var/db/cucina`, logs, the `cucina` user, `~cucina/.tart`) is never in the payload,
  so upgrades cannot touch it. `cucina-host-uninstall` removes software, logs, state and identity; `--purge` adds VMs,
  images and a package-created user; `--keep-state` keeps identity and L2.
* `build-pkg.sh` takes all inputs as files (Bazel genrule friendly), verifies pinned SHA-256s and tart.app's Team ID,
  CDHash and entitlement, and **strips AppleDouble entries** after `pkgbuild` (filter the payload cpio with bsdtar, rebuild
  the BOM with `mkbom -i`, re-flatten with `pkgutil --flatten`), failing if any remain.
* `pkgbuild` uses macOS system helpers that fail under Bazel's Darwin sandbox. The package build and manifest boundary
  test therefore use `no-sandbox` on macOS, but not `no-remote-exec`; signing remains local-only. This is a platform-tool
  constraint, not a skipped test.

## Consequences

Clean payloads regardless of who builds them. `Scripts` archives may still contain `._` siblings of the two scripts
(harmless; Installer only runs `preinstall`/`postinstall`). The hostd contract now agrees that state and identity are
removed by default and preserved only with `--keep-state` (docs/dev/hostd.md §2).
For purge, a package-created user must be logged out for deletion. A live console account is left for explicit deletion
after restart rather than forcibly killing its GUI session; the tool reports this deferred step. MDM erasure remains the
final decommissioning step before hardware is transferred.
