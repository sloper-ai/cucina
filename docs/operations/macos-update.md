<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Runbook: macOS update on a Mac host

**Use when** a Mac host must move to a new macOS release (security update, minor or major). The procedure is **drain, update, uncordon**, one host at a time (or one ring at a time). **Severity:** Planned. **Time:** 30 to 90 minutes per host, mostly waiting.
Software updates are driven by DDM declarations, because macOS 27 removed the legacy MDM update commands; see the [DDM guide](../mdm/ddm.md) and the [setup guide's software update declarations](../macos/mac-mini-setup.md#44-software-update-declarations).

## Rules

* **Hosts must run a macOS version at least as new as their guests.** Update hosts before you roll out a guest image on a newer macOS ([new Xcode, Visual Studio or OS release](new-xcode-vs-os-release.md)).
* **Do not update every host at once.** Update one host first (a canary), watch it for a day, then the rest in rings. Keep capacity: check that the other hosts can carry the load while one is out.
* **Drain before the update starts.** An update restarts the host. Use the MDM's maintenance window together with a drain, so that no VM is running when the restart happens.

## Procedure

1. **Record where you are.**

   ```sh
   cucinactl hosts list                    # macOS version, VMs, images
   ```

2. **Drain the host.** No new VMs start; running actions finish; then the VMs are stopped.

   ```sh
   cucinactl hosts drain <serial>
   cucinactl hosts list                    # repeat until the host shows no running VMs
   ```

   A busy VM finishes its action before it stops: **a host drain has no timeout**, so one long action holds the update. If you cannot wait, restart the host anyway; the scheduler or Bazel retries anything that is cut off.

3. **Update.** Let the DDM software-update declaration install it in the maintenance window (an enforcement declaration whose `TargetLocalDateTime` is the window; see the [DDM guide](../mdm/ddm.md)), or install it by hand in System Settings. The host restarts, the `cucina` user logs in automatically and the daemon reconnects on its own.

4. **Check the host came back right.** From the controller's side: `cucinactl hosts list` shows the host Offline during the update, then Online with the new macOS version. On the host (SSH or the MDM's remote tool):

   ```sh
   sw_vers
   fdesetup status                          # must say Off; macOS 26.4 and later can turn FileVault on in Setup Assistant
   pmset -g | grep -E 'sleep|womp|autorestart'
   sudo launchctl print system/ai.sloper.cucina.hostd | head -5
   sudo sfltool dumpbtm | grep -A6 ai.sloper.cucina.hostd      # the daemon is still an allowed login item
   stat -f%Su /dev/console                  # the cucina user is logged in after the restart
   ```

   If auto-login or FileVault changed, fix it through the MDM profile before you uncordon (the setup guide describes both).

5. **Uncordon and test.**

   ```sh
   cucinactl hosts uncordon <serial>
   ```

   Run a macOS build; a VM must start on the updated host and the build must pass. Hosts re-clone VMs from the golden image when the image version changes, so nothing else is needed.

6. **Next host.** Repeat for the rest of the ring, keeping enough capacity online.

## If it goes wrong

* **The host does not return**: check power and network (it should restart automatically). `cucinactl hosts list` shows it Offline; if it stays offline, go to the machine ([MT-004](../testing/manual/MT-004.md) is the same recovery). The controller has already retried its work elsewhere.
* **The daemon does not start after the update**: `sudo launchctl print system/ai.sloper.cucina.hostd` and the log (`sudo log show --predicate 'subsystem == "ai.sloper.cucina"' --last 30m`). A changed login-items rule or a profile that did not reapply is the usual cause.
* **VMs fail to start after the update**: Virtualization.framework changed behaviour or the keychain is locked for the `cucina` user. See the setup guide's troubleshooting; keep the host cordoned.
* **You cannot go back**: macOS downgrades need an erase and re-install through the MDM. Keep the host cordoned, remove it ([add or remove a Mac host](add-remove-mac-host.md)), and enroll it again after a clean install: it comes back Pending and needs approving. Removal deny-lists the host's identity until its old certificate would have expired plus an hour (up to seven days), and until then its cache upstream is refused, so keep it cordoned for that window. This is why you update a canary first.

## Roll back

`cucinactl hosts drain <serial>` (or leave it cordoned) keeps a bad host out of service. There is no downgrade; the recovery is re-installation.

## Verify

The host is Online with the new version, `fdesetup` is Off, the daemon and auto-login survived, a macOS build passes, and the other hosts in the pool are unaffected. [MT-005](../testing/manual/MT-005.md) is the full acceptance check.

## Escalate

Attach `cucinactl hosts diag <serial>` (it writes `cucina-host-<serial>-<time>.tar.gz`), the `sw_vers` before and after, and the MDM's update log for the host.
