<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->

# T14 kit: package, MDM simulation and enrollment in a throwaway macOS VM

T14 (§10.3) covers the host package and, when a controller environment is supplied, enrollment on a fresh macOS 27 VM
standing in for a brand-new Mac mini. The dummy-endpoint runner validates packaging, not enrollment. Install
packages **only inside throwaway Tart VMs** (`cucina-pkgtest-<n>`, cloned from the pulled
`ghcr.io/cirruslabs/macos-golden-gate-xcode:27` under `TART_HOME`), never on the dev Mac (§12). Nested macOS
virtualization is unavailable inside a VM: hostd must report that, and the root-daemon → `cucina`-user Tart path stays
covered by MT-001.

## 1. Automated package part

```sh
export CUCINA_AGENT=e2e; source .work/env.sh
tart list | grep macos-golden-gate-xcode                       # image pulled?
make -C macos/pkg hostd                                         # or any darwin/arm64 cucina-hostd build
macos/pkg/scripts/t14-vm.sh run --hostd macos/pkg/build/cucina-hostd
# For real enrollment, append --controller-url URL --ca-cert FILE --token-file FILE.
# Append --keep only for diagnosis; remember to delete that clone afterwards.
```

`t14-vm.sh` clones and boots the VM (headless), installs a throwaway SSH key through the Tart Guest Agent, copies the kit,
and then, inside the VM:

1. **Signs** versions 0.1.0 and 0.1.1 and the uninstaller with separate throwaway private application/installer
   identities (`make-signing-cert.sh --purpose … --p12-out`, seven-day keys generated in the VM). `ci-keychain.sh`
   imports both into one throwaway keychain; the installer certificate is trusted on the signing VM before
   `sign.sh --identity … --installer-identity … --keychain …`. `check-pkg.sh` checks each signing role independently;
   the keys/keychain are deleted after signing. No Developer ID or host-Mac trust changes occur.
2. **Simulates MDM** (`simulate-mdm.sh`): trusts the signer in the System keychain (what `01-cucina-trust` does), writes
   `/Library/Managed Preferences/ai.sloper.cucina.hostd.plist` (+ a 0600 local copy) and the install settings
   (`RestartAfterFirstInstall=false`, the scenario restarts explicitly). Auto-login is left to the package (default path).
3. `installer -pkg cucina-host-0-1-0.pkg -target /` **without `-allowUntrusted`**.
4. Restart → auto-login as `cucina`; restart with auto-login off → the daemon runs with nobody logged in; repair
   auto-login (`cucina-host-setup autologin --reset-password`) → restart → `cucina` again.
5. Upgrade in place to 0.1.1 with sidecar state/L2/VM markers and a fingerprint of the real identity key. The kit never
   overwrites a host identity with a synthetic marker.
6. Uninstall with the script, reinstall, uninstall with the uninstaller package.
7. Deletes the VM (unless `--keep`).

Every observation is a `PASS`/`FAIL`/`INFO` line (`macos/pkg/test/t14-guest-checks.sh`) collected in
`~/.config/cucina/t14/<vm>/results.txt`; the script exits non-zero on any `FAIL`. Each run also records its raw input
binary SHA-256, redacted hostd facts and measured guest CPU/memory allocation (the runner uses 2 CPUs / 8 GiB).
SSH keys, tokens, exported certificates
and logs stay in this mode-0700 private workspace. The EXIT handler deletes its clone on success or failure unless
`--keep` was requested. A kept VM still consumes a slot: explicitly run `t14-vm.sh down --vm <name>` afterwards.

### Verified scope (2026-10-02)

A fresh macOS 27.0 (26A428) VM passed **63 package checks, zero failures**, including private application/installer
signatures with timestamps, installation without `-allowUntrusted`, auto-login/unlocked keychain after reboot and repair,
root daemon startup with nobody logged in, real identity-key preservation, and script/package uninstall. Six additional
purge observations passed with the dedicated user logged out (user, home, VM/image tree, software/state/logs, receipt,
identity). After final hostd source stabilization, a fresh rebuild and newly signed packages repeated all **63 lifecycle
checks with zero failures** on a measured 2-CPU / 8-GiB guest; the supplemental purge observations above belong to the
earlier build. The final rebuild's `facts` returned `virtualization.available=false`, reason `nested-macos`.

These are synthetic package versions 0.1.0 → 0.1.1 around the same current hostd build; VM/cache preservation uses
sidecar files because nested macOS guests cannot run. Real enrollment, token reuse/revocation, managed-profile enforcement,
Apple Business delivery and physical-host power recovery remain unverified here. Private CI signing is scripted; the hosted
release workflow itself was not executed. No test VM is retained after validation.

## 2. The same steps by hand (exact commands)

For diagnosis, the automated runner's `--keep` leaves a staged kit in the guest at
`/Users/admin/.config/cucina/t14`, with public certificates and signed packages in `out/`. It also leaves the root-owned
mode-0600 `/var/db/cucina-t14-throwaway` sentinel. Destructive helpers require **both** this marker and a `VirtualMac`
hardware model; they refuse a physical Mac. Do not stage persistent inputs in `/tmp` (the base clears it on reboot).

The following commands run **inside that throwaway guest**, over the runner's SSH connection (the admin GUI Guest Agent
is unavailable once auto-login switches to `cucina`). Provide real enrollment inputs only when the acceptance environment exists.

```sh
KIT=/Users/admin/.config/cucina/t14
sudo sh "$KIT/macos/pkg/scripts/simulate-mdm.sh" --signer-cert "$KIT/out/installer.pem" \
  --ca-cert "$KIT/in/ca.pem" --controller-url https://<endpoint>:8445 --token-file "$KIT/in/token"
pkgutil --check-signature "$KIT/out/cucina-host-0-1-0.pkg"
sudo installer -pkg "$KIT/out/cucina-host-0-1-0.pkg" -target /  # NO -allowUntrusted
sudo launchctl print system/ai.sloper.cucina.hostd | grep -E 'state =|program ='
sudo sfltool dumpbtm | grep -B2 -A12 'ai.sloper.cucina.hostd'
fdesetup status
sudo /usr/local/cucina/bin/cucina-host-setup status
sudo shutdown -r now
# Reconnect over SSH after reboot; reassign KIT in the new shell.
KIT=/Users/admin/.config/cucina/t14
stat -f %Su /dev/console
sudo sh "$KIT/macos/pkg/test/t14-guest-checks.sh" mark
sudo installer -pkg "$KIT/out/cucina-host-0-1-1.pkg" -target /
sudo sh "$KIT/macos/pkg/test/t14-guest-checks.sh" after-upgrade 0.1.1
sudo /usr/local/cucina/bin/cucina-host-uninstall --yes
sudo sh "$KIT/macos/pkg/scripts/simulate-mdm.sh" --undo --signer-cert "$KIT/out/installer.pem"
```

Back on the development Mac, delete only your named clone with
`macos/pkg/scripts/t14-vm.sh down --vm cucina-pkgtest-1` (substitute the name used by the runner).

## 3. Expected observations

| Observation | Expected |
| --- | --- |
| `installer` without `-allowUntrusted` | succeeds once the signer is trusted; fails as untrusted before (`pkgutil --check-signature` shows the difference) |
| `launchctl print system/ai.sloper.cucina.hostd` | `state = running`, `program = /usr/local/cucina/bin/cucina-hostd` — also after a restart with nobody logged in |
| `sfltool dumpbtm` | an entry for `ai.sloper.cucina.hostd` (type daemon, enabled/allowed; *managed* only with the MDM-only login-items payload) |
| `fdesetup status` | `FileVault is Off.` |
| console user after restart | `cucina` (auto-login); its login keychain accepts writes without UI |
| managed configuration | hostd logs that it read `/Library/Managed Preferences/ai.sloper.cucina.hostd.plist` and starts enrollment (pending until `cucinactl hosts approve <serial>` unless pre-registered) |
| nested virtualization | hostd reports it unavailable (expected in a VM) |
| upgrade in place | receipt and `/usr/local/cucina/VERSION` show the new version; `/var/db/cucina`, `~cucina/.tart` and the System-keychain identity item unchanged; daemon running |
| uninstall | daemon unloaded; `/usr/local/cucina`, the plist, `/var/db/cucina`, logs and the identity item gone; receipt forgotten; `cucina` and its VMs kept unless `--purge` |

The VM's serial number for `cucinactl hosts register/approve`: `ioreg -c IOPlatformExpertDevice -d 2 | awk -F\" '/IOPlatformSerialNumber/ { print $4 }'`.
A second VM with the same token must enroll too; after `cucinactl hosts enroll-token revoke <id>` a third must be
refused while the first two keep working (the e2e scenario's part).

## 4. Temporary HTTPS publication (manifest URL + SHA-256)

```sh
macos/pkg/scripts/publish-s3-temp.sh create --pkg <signed cucina-host-0-1-0.pkg> --version 0.1.0   # tagged private bucket, pre-signed URLs
macos/pkg/scripts/publish-s3-temp.sh verify                         # manifest structure, download, SHA-256
macos/pkg/scripts/publish-s3-temp.sh cleanup                        # deletes objects + bucket (tag-checked)
```

The bucket (`cucina-e2e-pkg-<run>-<random>`, us-west-1) carries `cucina:env=e2e`, `cucina:run=$CUCINA_RUN_ID`,
`cucina:expires=$CUCINA_EXPIRES`; its own Block Public Access stays on and account-level settings are never touched. The
pre-signed URLs (stored only in `~/.config/cucina/t14/s3-publish.env`, 0600) expire after 6 hours or with the session
credentials. `cleanup --all-for-run` sweeps every bucket of the run by tag. Error cleanup also requires all three ownership tags;
if tagging failed, it refuses deletion and reports the bucket for operator inspection (S3 has no atomic create-and-tag
operation for this general-purpose bucket). Presigned manifests and temporary state are staged only under `~/.config/cucina`.
