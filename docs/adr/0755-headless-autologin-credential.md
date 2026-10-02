<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->

# ADR 0755: Prepare auto-login credentials and the login keychain without a GUI

* Status: accepted (2026-10-02, agent `pkg`)
* Requirements: R-MAC-2, R-MAC-8, UC19, UC21

## Context

On the macOS 27 T14 guest, `sysadminctl -autologin set` from root Installer/SSH changed `autoLoginUser` but left the
base image's `/etc/kcpassword` unchanged. It reported `SACSetAutoLoginPassword error:22` **with exit status zero**.
Reboot left the console at the login window. `sysadminctl -resetPasswordFor` likewise returned zero while refusing a
reset without secure-token unlock. The macOS image agent verified that the auto-login API works in an existing GUI
bootstrap context, but an ADE/postinstall path cannot depend on any user being logged in.

The login keychain must also be usable immediately after unattended login. Prepare it explicitly with the account
password before first login. The initial T14 probe incorrectly retained the SSH admin's HOME through sudo; the corrected
probe uses `sudo -H -u cucina`, targets the right login keychain, and has a bounded timeout instead of waiting on UI.

## Decision

* For this dedicated, standard, FileVault-off account, use root `dscl -passwd` for explicit password resets and verify
  with `dscl -authonly`; never infer success from sysadminctl's exit status alone. Initial account creation still uses
  sysadminctl, with authentication verified before enabling auto-login.
* Encode the password through the small `cucina-kcpassword` stdin/stdout helper: NUL termination, zero-padding to a
  multiple of 12 bytes, repeating public 11-byte macOS XOR mask. Write atomically as root:wheel 0600, then set
  `autoLoginUser`. This is **obfuscation, not encryption**; it provides no protection from root or physical disk access.
* Use system Perl for this format-only codec: no install-time network or additional runtime dependency. This deliberately
  avoids relying on a GUI-only API or a third-party credential installer. Recheck the format on every major macOS update.
* Create the new user's login keychain with the same random password, set it as default, and disable keychain idle/sleep
  locking. On explicit repair, move the old keychains aside before creating the replacement; never do this during an upgrade
  when the existing configuration is healthy, or while the user is logged in.
* Keep passwords out of logs and files except macOS's protected credential stores. Password arguments briefly appear in
  native account/keychain utility invocations; the encoder receives its input over stdin.

## Consequences

A throwaway VM reboot proved console auto-login and a noninteractive login-keychain write. A visible first-user Setup
Assistant process can still exist in an unmanaged VM; these account flags are not proof of ADE completion. Real MDM
pane skipping and unattended physical-Mac power recovery remain MT-001. FileVault is not disabled by the package.
