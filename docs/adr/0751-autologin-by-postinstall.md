<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->

# ADR 0751: Auto-login of the `cucina` user is set up by the package by default

* Status: accepted (2026-10-02, agent `pkg`)
* Requirements: R-MAC-2, R-MAC-10 (e), UC19, UC21

## Context

Tart needs the `cucina` user's login keychain unlocked, i.e. a GUI login after every restart. Two ways exist:
(1) MDM's loginwindow payload (`AutologinUsername`/`AutologinPassword`, macOS 14+, MDM-only), which requires the account
to exist with exactly that password; (2) the package's postinstall creates the user and enables auto-login itself.
Apple Business's built-in local-standard-user creation capability is unverified (verify in the Apple Business UI).
Option (1) also puts an account password into MDM and every rendered profile, potentially shared across hosts.

## Decision

Default is (2): if the user does not exist, postinstall creates `cucina` (standard user) with a 32-character random
password generated on the Mac, prepares its login keychain with that password, and atomically writes macOS's
`/etc/kcpassword` (root-only) plus the auto-login preference. ADR 0755 records why the headless sysadminctl path is not
reliable. No plaintext password file is retained; the package records that it created the user. If the
user exists, it only checks auto-login (local or MDM-managed) and repairs it for a package-created user that is not
logged in. After the **first** install it restarts the Mac once (2 minutes delay) when Setup Assistant is done and nobody
is logged in, so the auto-login session exists without hands-on steps; `RestartAfterFirstInstall=false` (install
settings, ADR 0754) disables that. Option (1) ships as the opt-in profile `05-cucina-autologin` with `CreateUser=false`.

## Consequences

Works with every MDM and with manual installs; no shared password. The random password briefly appears in
`sysadminctl`'s argument list during installation (root context on a fresh host). Repairing auto-login resets the
password and moves the old login keychain aside (VMs may need a re-clone). T14 checks the default path after reboot;
the alternative MDM loginwindow payload and real ADE flow require MT-001 on managed hardware.
