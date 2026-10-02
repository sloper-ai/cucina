<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Runbook: add or remove a Mac host

**Use when** you add a Mac mini to the fleet or take one out (retirement, repair, replacement). The long form of adding a host, from Apple Business enrollment to MDM profiles, is [`docs/macos/mac-mini-setup.md`](../macos/mac-mini-setup.md); this runbook is the operator's side of it and what to check.
**Severity:** Planned. **Time:** an hour of your time, plus the enrollment wait.

## Add a host

### Before the Mac arrives

1. **Decide the site and a token.** A site is a group of hosts that share an enrollment token and a network. Create a token that is **multi-use, expiring and bounded** (revoking or rotating it never affects hosts that are already enrolled):

   ```sh
   cucinactl hosts enroll-token create --site <site> --ttl 168h --max-hosts <count> --description "<what and when>"
   ```

   The token is shown once. Put it in the MDM's managed-preferences profile for the host agent; it is world-readable on every enrolled Mac, which is why it expires and why serial numbers are checked.

2. **Pre-register the serial numbers** (paste them from Apple Business), so the hosts are admitted without a manual step. Hosts you did not pre-register stay *Pending* until an admin approves them:

   ```sh
   cucinactl hosts register --site <site> <serial> [<serial> ...]
   ```

3. **Check the image and pool are ready.** The macOS pool must exist and its image be available to hosts (`cucinactl pools list`, `cucinactl images`). Hosts pre-pull the images the controller marks desired.

### First boot

Follow the setup guide: power, Ethernet and Setup Assistant; MDM delivers the trust certificate, the profiles and the package in that order. Then:

```sh
cucinactl hosts list                       # the new host: Pending (pending approval), then Online
cucinactl hosts approve <serial>           # only if you did not pre-register it
cucinactl hosts list -o json | jq '.hosts[] | select(.serial=="<serial>") | {phase, approved, running_vms, slots, macos_version, tart_version, images, disk_free_gib, cert_expiry}'
```

A pre-registered serial stays Pending only until it connects and enrolls; an unknown serial with a valid token is created Pending and waits for `approve`. There is no separate "approved" phase: the phases are Pending, Online, Offline, Draining, Cordoned and Denied, and the table adds `(pending approval)` or `(cordoned)` where they apply.

### Verify

* The host is **Online** and its **images** list contains the golden image.
* On the host: `fdesetup status` says FileVault is Off; `sudo launchctl print system/ai.sloper.cucina.hostd` shows the daemon running; `pmset -g` shows sleep 0 and auto-restart on; `sudo /usr/local/cucina/bin/cucina-host-setup status` summarises the daemon, the `cucina` user, auto-login and power settings.
* Run a small macOS build. A VM must start and register (`cucinactl workers list --pool <macos pool>`), and the build must pass. [MT-001](../testing/manual/MT-001.md) is the full acceptance check for the first host of a new design.
* Revoke or let the token expire when the batch is enrolled: `cucinactl hosts enroll-token revoke <id>`.

## Remove a host

1. **Drain it** so no new VMs start and running actions finish, then wait for zero VMs:

   ```sh
   cucinactl hosts drain <serial>
   cucinactl hosts list                    # wait until it shows no running VMs
   ```

   If it is unreachable or dead, skip to step 2; the controller marks an offline host unavailable and retries its in-flight actions elsewhere.

2. **Remove it from Cucina.** This deletes the host's record, releases its token slot and **deny-lists the host's identity until the certificate it held would have expired plus an hour** (seven days and an hour at most), so its certificate stops working at once:

   ```sh
   cucinactl hosts remove <serial>
   ```

   Use it when the serial is retiring, when the host's key is lost or exposed, or when the host must enroll again after an erase (an enrolled serial that presents a new key is refused until it is removed). **For repair or maintenance, drain and cordon instead**: a removed serial that enrolls again comes back Pending and can be approved, but its identity stays refused until the deny-list entry lapses (`cucinactl keys revocations` lists it), and with it the host's cache upstream (L2), so keep such a host cordoned until then.

3. **Remove the software and release the Mac.** Either remove the Cucina package through the MDM (on macOS 27, with the package delivered through the `com.apple.configuration.package` declaration and `UninstallBehavior.Remove` set before the first install, removing the declaration deletes the installed files; restart the Mac afterwards), or deploy the uninstaller
   package (`make -C macos/pkg uninstall-pkg UNINSTALL_ARGS=--purge`), or run the uninstall script on the host. `--purge` also removes the VMs, the images and the `cucina` user:

   ```sh
   sudo /usr/local/cucina/bin/cucina-host-uninstall --purge --yes
   ```

   Then release the device from Apple Business (or erase it for reuse) and from your MDM. Package removal through Apple Business Blueprints leaves the files in place, so use one of the methods above ([setup guide, Day 2](../macos/mac-mini-setup.md#7-day-2)).
4. **Clean up what remains.** The host's certificate is short-lived and the deny-list entry covers it until it expires; confirm with `cucinactl hosts list` that the serial is gone. If the host's token was a one-off, revoke it.

### Undo

A removed host can enroll again from the start with a token for its site: it comes back Pending, and `cucinactl hosts approve <serial>` (or `register` before it connects) admits it; mind the deny-list window described above. A drained host is put back with `cucinactl hosts uncordon <serial>`.

## Troubleshooting

* **Stuck Pending**: the serial is neither pre-registered nor approved; `cucinactl hosts approve <serial>` (also for a serial that was removed and enrolled again).
* **Enrolled before, refused now** (`already enrolled; an administrator must remove it`): the host has a new key (an erase or a reinstall); `cucinactl hosts remove <serial>`, then let it enroll again and approve it.
* **Enrollment refused**: the token is revoked, expired, over its host count, or for another site; create a new token and update the MDM profile.
* **Offline** (the host-offline alert): the heartbeat stopped. Check power and network first; the controller has already retried the host's in-flight actions elsewhere. A Mac that does not return by itself needs a visit ([MT-004](../testing/manual/MT-004.md) is the same recovery); `cucinactl hosts diag <serial>` needs the host to be reachable. A host that will not come back is drained and removed (above).
* **Online but no VMs start**: the host is cordoned (`cucinactl hosts uncordon`), at its two-VM cap, missing the image, or low on disk (`cucinactl hosts list`, `cucinactl hosts diag <serial>`).
* **Locked keychain or Local Network errors in the host log**: see the setup guide's troubleshooting section.

## Escalate

`cucinactl hosts diag <serial>` writes the host's logs and facts to `cucina-host-<serial>-<time>.tar.gz`; attach it (with the serial redacted if you post it publicly).
