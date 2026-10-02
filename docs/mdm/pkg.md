<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->

# The Cucina host package (R-MAC-8)

One **distribution-style** package (`productbuild`), product and component identifier `ai.sloper.cucina.host`, Apple
silicon only, startup volume only, macOS 26 or later. It works with both MDM delivery mechanisms: the
`InstallEnterpriseApplication` command and the declaration `com.apple.configuration.package` (macOS 26+, supervised).

## 1. Contents

| Path on the host | What | Notes |
| --- | --- | --- |
| `/Library/LaunchDaemons/ai.sloper.cucina.hostd.plist` | LaunchDaemon (root:wheel 0644) | `cucina-hostd run`, RunAtLoad, KeepAlive (crash-only), ThrottleInterval 10, ExitTimeOut 120 (time for hostd to stop its VMs), NumberOfFiles 65536, stderr to `/Library/Logs/Cucina/hostd.stderr.log` |
| `/usr/local/cucina/bin/cucina-hostd` | host agent | darwin/arm64, cgo; signed with hardened runtime |
| `/usr/local/cucina/bin/bb_storage` | Buildbarn bb_storage `20260930T153215Z-086b011` (host L2) | pinned release, SHA-256 verified, then signed |
| `/usr/local/cucina/tart.app` | **Tart 2.40.1, exactly as released** | never re-signed (restricted `com.apple.vm.networking`); not relocatable, not version-checked (the package is authoritative) |
| `/usr/local/cucina/bin/tart` | wrapper for humans | `exec …/tart.app/Contents/MacOS/tart "$@"` (Homebrew's pattern); hostd calls the binary directly |
| `/usr/local/cucina/bin/cucina-host-setup` | host preparation (postinstall body) + `status`, `autologin`, `hostname`, `ssh`, `local-network` | POSIX sh, idempotent |
| `/usr/local/cucina/bin/cucina-host-uninstall` | uninstaller | `--purge`, `--keep-state`, `--yes`, `--dry-run` |
| `/usr/local/cucina/share/doc/{LICENSE.md,THIRD_PARTY_NOTICES.md,tart/LICENSE}` | licences | FSL-1.1-ALv2 (Cucina, Tart), notices for Buildbarn and Go modules |
| `/usr/local/cucina/{VERSION,share/cucina/components}` | versions | package, Tart and bb_storage versions |
| `/etc/newsyslog.d/ai.sloper.cucina.conf` | log rotation | for `hostd.stderr.log` and `install.log` (hostd rotates its own logs) |

Created at install time (not in the payload, so upgrades never touch them): `/var/db/cucina/{hostd,l2,pkg}` (root 0700:
identity, enrollment state, L2 cache, package markers), `/Library/Logs/Cucina` (root:admin 0750), the user `cucina` and
its `~/.tart` (VMs and images). The host identity key lives in the System keychain (service `ai.sloper.cucina.hostd`,
account `host-identity-key`). Contract with hostd: [docs/dev/hostd.md §2](../dev/hostd.md).

## 2. Scripts

* **preinstall** (root): refuses non-startup volumes and non-arm64 Macs; on upgrade stops the daemon with
  `launchctl bootout` (hostd stops its VMs on SIGTERM within ExitTimeOut). Never touches VMs, caches or identity.
* **postinstall** (root): `cucina-host-setup install` — the `cucina` user with an on-device random password and
  auto-login (if missing), `pmset -a sleep 0 womp 1 autorestart 1`, `systemsetup -setrestartfreeze on`, vmnet DHCP lease
  600 s, state/log directories, firewall allowance for hostd, FileVault warning, Local Network escape-hatch note,
  `launchctl bootstrap`, and a one-time restart after the first install (ADR 0751).

Both are idempotent, POSIX sh, shellcheck-clean and need no network. Install-time behaviour can be tuned with the
preference domain **`ai.sloper.cucina.host`** (managed or local), kept separate from hostd's strict domain (ADR 0754):

| Key | Type | Default | Effect |
| --- | --- | --- | --- |
| `CreateUser` | bool | `true` | create the Tart user (`RunAsUser` from hostd's domain, default `cucina`) if missing |
| `ManageAutoLogin` | bool | `true` | enable/repair auto-login for a user the package created |
| `RestartAfterFirstInstall` | bool | `true` | restart once after the first install when nobody is logged in and Setup Assistant is done |
| `LocalNetworkAllowedEthernetAddresses` | array of CIDR | — | set the Local Network escape hatch (macOS 15.5+, takes effect after a restart) |

## 3. Upgrade in place and uninstall

* **Upgrade:** install a newer version over the old one (MDM: new URL + SHA-256, or a new manifest). preinstall stops
  hostd, the payload replaces binaries, Tart and the plist, postinstall starts hostd again. VMs (`~cucina/.tart`), the L2
  cache and the host identity are preserved. A downgrade works the same way (Tart is not version-checked).
* **Uninstall:** `sudo /usr/local/cucina/bin/cucina-host-uninstall [--purge] [--keep-state] --yes` stops hostd and any VM
  still running (SIGINT like `tart stop`, then SIGKILL after 60 s) and removes the daemon, `/usr/local/cucina`, the
  newsyslog rule, the logs, `/var/db/cucina`, the keychain identity and the package receipt. It keeps the `cucina` user,
  its auto-login and its VMs/images unless `--purge` (which also deletes them, and the user only if the package created
  it). `--keep-state` keeps the identity and the L2 cache for a reinstall without a new approval. Managed preferences and
  profiles belong to MDM and are never touched. For MDMs that can only deploy packages, `make -C macos/pkg uninstall-pkg`
  builds a payload-free uninstaller (`ai.sloper.cucina.host.uninstall`; `UNINSTALL_ARGS=--purge` for the purge variant).
* **DDM removal (macOS 27):** with `com.apple.configuration.package` and `UninstallBehavior.Remove = true` set before the
  first install, removing the declaration deletes the files the package installed; it does not run the uninstaller, so
  state under `/var/db/cucina` and the daemon (until the next restart) remain. Prefer the uninstaller for full removal.

## 4. Building

```sh
make -C macos/pkg pkg            # unsigned: build/cucina-host-0-1-0-unsigned.pkg (go build of ./cmd/cucina-hostd)
make -C macos/pkg pkg HOSTD_BIN=/path/to/cucina-hostd VERSION=0.1.1
make -C macos/pkg sign IDENTITY="Cucina Host Package Signing"     # signed: build/cucina-host-0-1-0.pkg
make -C macos/pkg check          # scripts/check-pkg.sh on what was built
make -C macos/pkg lint test      # shellcheck, plutil, JSON, SPDX; manifest and profile tests
```

`scripts/build-pkg.sh` takes every input as a file (`--hostd`, `--bb-storage`, `--tart-tarball`, `--license`,
`--notices`, `--version`, `--out`), verifies the pinned SHA-256s (`macos/pkg/pins.env`), verifies that `tart.app` is
intact (Team ID, CDHash, entitlement), stages the payload, signs Cucina's own binaries if an identity is given, runs
`pkgbuild` with a component plist and `productbuild` with `resources/distribution.xml.in`. Builds need no network if the
inputs are passed in; otherwise `scripts/fetch-deps.sh` downloads and verifies them once.

**AppleDouble entries.** macOS attaches the undeletable `com.apple.provenance` attribute to every file written by a
provenance-tracked process (terminal apps, IDEs, Bazel, CI agents), and `pkgbuild` archives extended attributes as
`._name` payload entries. `build-pkg.sh` filters them out of the payload and the BOM and re-flattens the component, then
fails if any remain (ADR 0750).

### Bazel

The unsigned package is built by a genrule on macOS (R-BUILD-1 "macOS pkg"); signing stays a local-only `bazel run`/make
step. Wiring for the bazel agent: add `include("//macos/pkg:deps.MODULE.bazel")` to the root `MODULE.bazel`, then:

```python
genrule(
    name = "pkg_unsigned",
    srcs = [
        "//cmd/cucina-hostd",
        "@cucina_pkg_bb_storage_darwin_arm64//file",
        "@cucina_pkg_tart//file",
        "//:LICENSE.md",
        "//:THIRD_PARTY_NOTICES.md",
        ":build_inputs",
    ],
    outs = ["cucina-host-unsigned.pkg"],
    cmd = "$(location :scripts/build-pkg.sh) --hostd $(location //cmd/cucina-hostd)" +
          " --bb-storage $(location @cucina_pkg_bb_storage_darwin_arm64//file)" +
          " --tart-tarball $(location @cucina_pkg_tart//file)" +
          " --license $(location //:LICENSE.md) --notices $(location //:THIRD_PARTY_NOTICES.md)" +
          " --version 0.1.0 --work $(RULEDIR)/pkg-work --out $@",
    exec_compatible_with = ["@platforms//os:macos"],
    target_compatible_with = ["@platforms//os:macos"],
    tags = ["no-sandbox"],  # pkgbuild writes through system helpers the darwin sandbox denies
)
```

The tests `//macos/pkg:manifest_test` and `//macos/profiles:render_test` (tier `integration`, macOS only) are already in
the tree; `manifest_test` is tagged `no-sandbox` for the same reason.

## 5. Manifest and publishing

`scripts/make-manifest.sh` writes, per version (`<base>` = `cucina-host-<MAJOR>-<MINOR>-<PATCH>`):

* `<base>.plist` — the **ManifestURL** document: `items[0].assets[0] = {kind: software-package, url, sha256}`,
  `items[0].metadata = {bundle-identifier: ai.sloper.cucina.host, bundle-version, kind: software, title, subtitle}`
  (schema `other/manifesturl.yaml` of apple/device-management);
* `<base>.install-enterprise-application.plist` — an example MDM command with the manifest inline;
* `<base>.ddm-package.json` — an example `com.apple.configuration.package` declaration (ManifestURL, `Install:
  Required`, `UninstallBehavior.Remove: true`);
* `<base>.json` — the values Apple Business's package form asks for (name, URL, SHA-256, bundle ID, version);
* `<base>.pkg.sha256`.

`make -C macos/pkg pkg-publish` (`scripts/publish.sh`) uploads a signed version as **immutable** artifacts and then
verifies them like a Mac would (`scripts/verify-manifest.sh`: manifest structure, download through redirects, SHA-256):

* **GitHub Releases (production, R-OPS-7):** assets of the existing release `v<version>` created by the release
  workflow; existing assets are never replaced. URLs:
  `https://github.com/sloper-ai/cucina/releases/download/v<version>/<base>.pkg` (and `.plist`). GitHub answers with one
  `302` to a short-lived signed URL on `release-assets.githubusercontent.com` (`application/octet-stream`). macOS's MDM
  agent downloads with Foundation's URL loading, which follows it (checked with a `URLSession` probe on macOS 27 and with
  `verify-manifest.sh`); whether **Apple Business's** own package
  validation follows it could not be verified without an Apple Business account (MT-001; ADR 0753). If it does not, use the
  S3/CloudFront target.
* **S3:** `TARGET=s3 BUCKET=… [BASE_URL=https://cdn…]`: objects written with `If-None-Match: *`; public read access
  (bucket policy or CloudFront) is the operator's setup. The package contains no secrets.
* Asset names use only lowercase letters, digits and hyphens before the extension, one URL per version, as Apple Business
  asks for package URLs.

The release also carries the signer's public certificate (`cucina-host-signer-<sha1>.pem`, `--signer-cert`) for the trust
profile. Nothing is published during development runs; T14 uses `scripts/publish-s3-temp.sh` (a temporary, tagged,
private bucket with pre-signed URLs; [t14-kit.md](t14-kit.md)).
