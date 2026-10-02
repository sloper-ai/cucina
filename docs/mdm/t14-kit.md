<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->

# T14 kit: package, MDM simulation and enrollment in a throwaway macOS VM

T14 (§10.3) proves the host package and enrollment on a fresh macOS 27 VM standing in for a brand-new Mac mini. Install
packages **only inside throwaway Tart VMs** (`cucina-pkgtest-<n>`, cloned from the pulled
`ghcr.io/cirruslabs/macos-golden-gate-xcode:27` under `TART_HOME`), never on the dev Mac (§12). Nested macOS
virtualization is unavailable inside a VM: hostd must report that, and the root-daemon → `cucina`-user Tart path stays
covered by MT-001.

## 1. Automated package part

```sh
export CUCINA_AGENT=e2e; source .work/env.sh
tart list | grep macos-golden-gate-xcode                       # image pulled?
make -C macos/pkg hostd                                         # or any darwin/arm64 cucina-hostd build
macos/pkg/scripts/t14-vm.sh run --hostd macos/pkg/build/cucina-hostd \
  [--controller-url https://<endpoint>:8445 --ca-cert cucina-ca.pem --token-file <token-file>] [--keep]
```

`t14-vm.sh` clones and boots the VM (headless), installs a throwaway SSH key through the Tart Guest Agent, copies the kit,
and then, inside the VM:

1. **Signs** versions 0.1.0 and 0.1.1 and the uninstaller with a throwaway private identity: `make-signing-cert.sh
   --p12-out` (key generated in the VM, 7 days), `ci-keychain.sh create` (throwaway keychain, prompt-free),
   `sign.sh --keychain` (codesign hostd/bb_storage, productbuild --sign, `check-pkg.sh --signed`), then deletes the
   keychain and the PKCS#12.
2. **Simulates MDM** (`simulate-mdm.sh`): trusts the signer in the System keychain (what `01-cucina-trust` does), writes
   `/Library/Managed Preferences/ai.sloper.cucina.hostd.plist` (+ a 0600 local copy) and the install settings
   (`RestartAfterFirstInstall=false`, the scenario restarts explicitly). Auto-login is left to the package (default path).
3. `installer -pkg cucina-host-0-1-0.pkg -target /` **without `-allowUntrusted`**.
4. Restart → auto-login as `cucina`; restart with auto-login off → the daemon runs with nobody logged in; repair
   auto-login (`cucina-host-setup autologin --reset-password`) → restart → `cucina` again.
5. Upgrade in place to 0.1.1 with markers for hostd state, a VM bundle and the identity key item.
6. Uninstall with the script, reinstall, uninstall with the uninstaller package.
7. Deletes the VM (unless `--keep`).

Every observation is a `PASS`/`FAIL`/`INFO` line (`macos/pkg/test/t14-guest-checks.sh`) collected in
`$CUCINA_DEV_STORAGE/pkg/t14/results.txt`; the script exits non-zero on any `FAIL`.

## 2. The same steps by hand (exact commands)

```sh
VM=cucina-pkgtest-1
tart clone ghcr.io/cirruslabs/macos-golden-gate-xcode:27 $VM
tart run --no-graphics $VM &                 # background; log to $CUCINA_DEV_STORAGE/logs/
tart ip --wait 300 $VM
# copy the signed package, the signer certificate (PEM), the CA, the token file and macos/pkg/scripts/simulate-mdm.sh
tart exec -i $VM sh -c 'cat > /tmp/simulate-mdm.sh' < macos/pkg/scripts/simulate-mdm.sh    # same for each file

# inside the VM (tart exec $VM … or ssh admin@<ip>)
sudo sh /tmp/simulate-mdm.sh --signer-cert /tmp/signer.pem --ca-cert /tmp/ca.pem \
     --controller-url https://<endpoint>:8445 --token-file /tmp/token
pkgutil --check-signature /tmp/cucina-host-0-1-0.pkg      # "signed by a certificate trusted by macOS" after trust
sudo installer -pkg /tmp/cucina-host-0-1-0.pkg -target /  # NO -allowUntrusted
sudo launchctl print system/ai.sloper.cucina.hostd | grep -E 'state =|program ='
sudo sfltool dumpbtm | grep -B2 -A12 'ai.sloper.cucina.hostd'
fdesetup status
sudo /usr/local/cucina/bin/cucina-host-setup status
sudo shutdown -r now
# after the restart (SSH; the Guest Agent runs in the admin session, which no longer logs in)
stat -f %Su /dev/console                                   # cucina
sudo launchctl print system/ai.sloper.cucina.hostd | grep 'state ='
log show --last 10m --predicate 'subsystem == "ai.sloper.cucina"' | tail   # hostd: config read, enrollment, nested virtualization unavailable
sudo installer -pkg /tmp/cucina-host-0-1-1.pkg -target /  # upgrade in place
sudo /usr/local/cucina/bin/cucina-host-uninstall --yes      # uninstall (add --purge for VMs/images/user)
sudo sh /tmp/simulate-mdm.sh --undo --signer-cert /tmp/signer.pem
# on the dev Mac
tart stop $VM; tart delete $VM
```

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
credentials. `cleanup --all-for-run` sweeps every bucket of the run by tag.
