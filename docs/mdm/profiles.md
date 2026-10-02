<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->

# Configuration profiles for Cucina hosts (R-MAC-10)

Templates: `macos/profiles/templates/*.mobileconfig`. Renderer: `macos/profiles/render.sh` (settings reference:
`render.sh --help`, example: `macos/profiles/example.env`). Each rendered profile is a system-scoped `Configuration`
with stable `PayloadIdentifier`s (`ai.sloper.cucina.profile.*`) and UUIDs, so re-uploading a changed profile replaces the
old one. Each also works as an Apple Business **Custom configuration**: `.mobileconfig`, < 1 MB (they are 1–4 KB), macOS
as the common platform.

## Template syntax

Templates are valid property lists (`plutil -lint` passes on them):

* `${CUCINA_NAME}` — a required value inside `<string>` or `<data>`; values are XML-escaped, certificates become DER
  base64.
* `<!-- @if CUCINA_NAME: <key>…</key><…/> -->` — an optional key, emitted only when the setting is given (typed values:
  integers, booleans, dictionaries).
* `<!-- @if CUCINA_NAME -->` … `<!-- @endif -->` — an optional block (second signer, CA payload).

`render.sh` validates every setting (HTTPS URL, integer ranges matching hostd's schema, booleans, CIDRs, SPKI pins),
fails on any missing value, lints the output, checks the 1 MB limit, writes files mode 0600 and refuses to write into a
git work tree (profile 02 contains the site enrollment token; profile 05 a password). `macos/profiles/test/render_test.sh`
proves all of this (`bazel test //macos/profiles:render_test`).

## The profiles

| # | File | Payload | Content |
| --- | --- | --- | --- |
| 01 | `01-cucina-trust` | `com.apple.security.root` ×1–5 | installer/application certificates; up to four signers for old/new trust during rotation, plus Cucina's CA (`--ca-cert`, unless `--no-trust-ca`). **Deliver first.** |
| 02 | `02-cucina-hostd-preferences` | `com.apple.ManagedClient.preferences` | domain `ai.sloper.cucina.hostd` (forced): `ControllerURL`, `CACertificate` (DER data) and/or `CAPinSHA256`, `SiteEnrollmentToken`, optional `ControllerServerName`, `IdentityLabel`, `Site`, `Labels`, `VMSlots` (1–2), `VMCPUCount`, `VMMemoryGiB`, `L2SizeGiB`, `VMMaxAgeHours`, `LogLevel`, `RunAsUser`, `TartPath`; domain `ai.sloper.cucina.host` (forced): the package's install settings (`CreateUser`, `ManageAutoLogin`, `RestartAfterFirstInstall`, `LocalNetworkAllowedEthernetAddresses`). Lands in `/Library/Managed Preferences/<domain>.plist`; hostd reads it with `CFPreferencesCopyAppValue` as root and rejects unknown keys ([hostd.md §3](../dev/hostd.md)). |
| 03 | `03-cucina-login-items` | `com.apple.servicemanagement` | rule `RuleType = Label`, `RuleValue = ai.sloper.cucina.hostd`: the daemon is managed (cannot be disabled in Login Items). MDM-only payload. A Label rule because the private certificate has no Team ID. |
| 04 | `04-cucina-energy` | `com.apple.MCX` | `SleepDisabled`; desktop AC power: System/Disk Sleep Timer 0, `Wake on LAN` 1, `Automatic Restart On Power Loss` 1. |
| 05 | `05-cucina-autologin` | `com.apple.loginwindow` | **alternative path, not rendered by default**: `AutologinUsername`/`AutologinPassword` (macOS 14+, MDM-only). The user must already exist with exactly that password. |
| 06 | `06-cucina-filevault-off` | `com.apple.MCX` | `dontAllowFDEEnable = true`. |
| 07 | `07-cucina-firewall` | `com.apple.security.firewall` | `EnableFirewall`, `EnableStealthMode` (default on), `BlockAllIncoming = false`, `AllowSigned`/`AllowSignedApp`, and `Applications` = `ai.sloper.cucina.hostd` allowed. |

### Auto-login: default and alternative (ADR 0751)

* **Default — the package does it.** postinstall creates the standard user `cucina` with a 32-character random password
  held in macOS's reversible auto-login secret `/etc/kcpassword` (root-only), prepares an identically password-protected
  login keychain, turns on auto-login, and restarts once after the first install so the session exists (ADR 0755). No password ever passes through MDM. It works with every MDM,
  without relying on local-standard-user creation in Apple Business (verify that capability in the Apple Business UI).
* **Alternative — profile 05.** For MDMs that create local accounts (for example Jamf): create `cucina` there with a
  password, render `--autologin-password-file`, deploy 05, and set `CreateUser=false` in profile 02. The password then
  lives in the MDM and in the profile; rotate it in both places together.

T14 tests the package-created account across a restart and probes its unlocked login keychain. The alternative MDM
loginwindow payload still needs verification on managed hardware (MT-001); a simulation is not MDM enrollment evidence.

### FileVault (profile 06)

The ADE skip key `FileVault` hides the Setup Assistant pane; `dontAllowFDEEnable` prevents turning FileVault on later.
macOS 26.4+ turns FileVault on during Setup Assistant by default, so check `fdesetup status` on every new host (the
package logs a warning, and hostd reports it in its host facts). Never combine with a FileVault-enforcing configuration.

### Firewall (profile 07) and Local Network privacy

VMs reach hostd's relays on the host's NAT network (TCP 8981 storage, 8983 scheduler; `bb_storage` itself listens on
loopback only). Never block all incoming connections. hostd is signed with the trusted private certificate
(`AllowSignedApp`) and named explicitly by its code-signing identifier; postinstall also adds it with
`socketfilterfw --add/--unblockapp` when the firewall is not profile-managed. Verify VM → host connectivity on the first
host (MT-001).

Local Network privacy (TN3179): macOS automatically allows local network access to **launchd daemons and anything running
as root**, and to command-line tools run from Terminal or SSH, but **not** to processes a daemon starts after dropping
privileges or to launchd agents. MDM cannot pre-approve Local Network access for such processes. Cucina therefore makes
every connection to a VM IP from hostd itself (root), never from the `tart` child running as `cucina`. Escape hatch
(macOS 15.5+): the system defaults `AllowedEthernetLocalNetworkAddresses` / `AllowedWiFiLocalNetworkAddresses` in domain
`com.apple.network.local-network` (CIDR strings) make every address in those networks non-local for every program; set
them with `cucina-host-setup local-network allow 192.168.64.0/24` or the install setting
`LocalNetworkAllowedEthernetAddresses`, then restart. macOS 27's `com.apple.configuration.app.settings` permission defaults
still show a consent prompt, so they do not help a headless host.

No TCC (PPPC) grants are needed: Virtualization.framework requires none. Should a PPPC rule ever be needed, pin the
private certificate: `identifier "ai.sloper.cucina.hostd" and anchor = H"<certificate SHA-1>"`.
