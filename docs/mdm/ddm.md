<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->

# Declarative device management for Cucina hosts

Example declarations live in `macos/ddm/` (JSON, schema: apple/device-management `declarative/`). They are for MDMs that
accept custom declarations; Apple Business's built-in service uses its own Software Update configuration (*verify in the
Apple Business UI* whether custom declarations can be uploaded). Replace identifiers, versions and dates with yours; every
change needs a new `ServerToken`.

## Software updates (macOS 27: DDM only)

macOS 27 removed the legacy MDM software-update commands (`ScheduleOSUpdate` and friends); updates are managed with
declarations only. Cucina uses two rings (canary hosts first, then the fleet), each with:

* `softwareupdate.settings.ring*.json` (`com.apple.configuration.softwareupdate.settings`, macOS 15+, supervised):
  major-upgrade deferral 90 days; minor-update deferral 7 days (canary) / 14 days (fleet); non-OS (system) updates 3/7
  days; `Download: AlwaysOn`, `InstallOSUpdates: AlwaysOff` (OS updates only through enforcement, after a drain),
  `InstallSecurityUpdate: AlwaysOn` and Background Security Improvements (`RapidSecurityResponse.Enable`, with rollback)
  for automatic security responses; no notifications (headless); standard users (the `cucina` user) cannot install OS
  updates; no beta programs.
* `softwareupdate.enforcement.specific.ring*.json` (macOS 14+, supervised): `TargetOSVersion` and
  `TargetLocalDateTime` — the deadline, in the host's local time, inside the ring's maintenance window (the examples
  enforce 27.1 at 02:00 a week apart). Choose a date after the deferral period has made the update visible.
* `activation.ring*.json` (`com.apple.activation.simple`): activates the ring's two configurations. Scope each activation
  to the ring's device group in the MDM.

**After a drain.** A declaration cannot wait for Cucina, so schedule the drain before the deadline: run
`cucinactl hosts drain <host>` for the ring's hosts (cron or CI job) an hour before `TargetLocalDateTime`, and
`cucinactl hosts uncordon <host>` once `cucinactl hosts list` shows the host back on the new version. An undrained host
still updates; its running actions are retried elsewhere (UC8).

## The package as a declaration

`package.cucina-host.example.json` (`com.apple.configuration.package`, macOS 26+, supervised) installs the package from a
**ManifestURL** (the `cucina-host-X-Y-Z.plist` that `make-manifest.sh`/`pkg-publish` produce; `make-manifest.sh` also
writes the exact declaration as `<base>.ddm-package.json`). `InstallBehavior.Install = Required`;
`UninstallBehavior.Remove = true` (macOS 27) makes removing the declaration delete the installed files — it must be set
before the first install, and it does not track what postinstall or hostd create later. When DDM manages the package,
`InstallEnterpriseApplication` for the same package fails, so use one mechanism per host.

## Variant: DDM background tasks (SHOULD, macOS 15+, supervised)

Instead of the package's LaunchDaemon, an MDM can deliver hostd as a managed background task:
`services.background-tasks.cucina-hostd.json` (`com.apple.configuration.services.background-tasks`, `TaskType
ai.sloper.cucina.hostd`) references three `com.apple.asset.data` assets:

* `hostd-files` — a zip of the package payload under `/usr/local/cucina` (binaries, `tart.app` as released, docs),
  expanded by macOS into the tamper-resistant
  `/var/db/ManagedConfigurationFiles/BackgroundTaskServices/Services/ai.sloper.cucina.hostd/`;
* `hostd-launchd` — `background-tasks/ai.sloper.cucina.hostd.plist`, the same daemon pointing into that directory;
* `host-setup-launchd` — `background-tasks/ai.sloper.cucina.host-setup.plist`, a one-shot job at boot that runs
  `cucina-host-setup install --no-daemon --prefix <that directory>` (user, auto-login, power, DHCP lease, directories).

Build the assets from a signed package: `macos/ddm/make-bgtask-assets.sh --pkg cucina-host-0-1-0.pkg --version 0.1.0
--base-url https://…/v0.1.0` (writes the zip, both plists and the asset declarations with sizes and SHA-256 hashes).
Set the hostd preference `TartPath` to `<that directory>/tart.app/Contents/MacOS/tart` (profile 02,
`CUCINA_TART_PATH`). Deploy **either** the package **or** this variant on a host (same launchd label). Advantages: the
files cannot be modified locally, and updating the declaration restarts the tasks. Limits: needs an MDM that supports
custom declarations and assets, and it has not been exercised on a real MDM yet (MT-001 covers the package path).
