<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->

# Setting up Mac minis as Cucina hosts (Apple Business + MDM)

This guide takes a brand-new Mac mini from the box to a Cucina host that runs macOS build VMs, with **no hands-on steps
beyond power, network and Setup Assistant** (UC19). Afterwards the host updates, recovers from power loss and is
decommissioned through MDM (UC20, UC21).

The primary path is **Apple Business's built-in device management** (Apple Business replaced Apple Business Manager and
Business Essentials on 2026-04-14; its built-in MDM is free). Other MDMs are covered in [§9](#9-appendix--third-party-mdms).
Some Apple Business capabilities could not be verified when this guide was written; they are marked
**verify in the Apple Business UI**. The package's `postinstall` handles local account creation and host preparation
without an MDM scripting feature. It cannot replace MDM-delivered trust/configuration, prevent FileVault through policy,
or guarantee enrollment ordering; verify those prerequisites before fleet rollout.

What runs on a host: the LaunchDaemon `ai.sloper.cucina.hostd` (root) dials **out** to the Cucina control plane, keeps up to
two Tart VMs (macOS + Xcode) for Buildbarn workers, and runs a host-level cache (L2). Tart runs as the dedicated standard
user `cucina`, which logs in automatically after every restart (Virtualization.framework needs that user's unlocked login
keychain).

## Parameters

No production cluster exists yet, so this guide works against any Cucina deployment. Collect these values from whoever
runs the control plane:

| Parameter | Example | Where it comes from |
| --- | --- | --- |
| `CONTROLLER_URL` | `https://cucina-hosts.example.com:8445` | The chart's host enrollment endpoint (`endpoints.worker.enrollmentPort`, default 8445). Hosts also reach the HostService (8446), storage (8981) and scheduler (8983) on the same host or the addresses enrollment returns. |
| CA bundle | `cucina-ca.pem` | Cucina's private CA (hostd verifies the controller with it). Alternatively an SPKI pin. |
| Site enrollment token | (secret, shown once) | `cucinactl hosts enroll-token create --site <site> --ttl 30d --max-hosts <n>` |
| Package URL, SHA-256, bundle ID | `https://github.com/sloper-ai/cucina/releases/download/v0.1.0/cucina-host-0-1-0.pkg`, `<64 hex>`, `ai.sloper.cucina.host` | The release (`cucina-host-0-1-0.json` lists all three) or `make -C macos/pkg pkg-publish` output |
| Manifest URL | `…/cucina-host-0-1-0.plist` | Same place; needed by MDMs that take a manifest (DDM `com.apple.configuration.package`, `InstallEnterpriseApplication`) |
| Package signer certificate | `cucina-host-signer-<sha1>.pem` | Release asset; the public half of the private signing identity ([signing](../mdm/signing.md)) |

The site token is world-readable on enrolled hosts (managed preferences are), so its exposure is limited by its expiry,
its host count and serial-number admission: a host only enrolls if its serial is pre-registered or approved
(`cucinactl hosts register`/`approve`, R-SEC-3). Revoking or rotating the token never affects enrolled hosts.

## 1. Prerequisites

**Accounts and devices**

* An Apple Business organization and a Managed Apple Account whose role may manage devices, Blueprints and device
  configurations.
* An iPhone with **Apple Configurator** (iOS 16 or later) if the Macs were not bought from Apple or a reseller linked
  to your organization.

**Hardware.** Each host runs at most two macOS VMs (Apple's licence and Virtualization.framework limit) plus the L2
cache. hostd sizes each VM at `(cores − 2) / slots` vCPUs and `(RAM − 8 GB) / slots` memory.

| Model | VMs | Per VM | Notes |
| --- | --- | --- | --- |
| **Mac mini M5 Pro, 18-core CPU, 64 GB, ≥ 1 TB, 10GbE** (recommended) | 2 | ≈ 8 vCPU / 28 GB | Fits two Xcode builds plus a 200 GiB L2 cache. Take 2 TB if you keep several Xcode versions (each golden image is ≈ 70 GB). |
| Mac mini M6, 32 GB | 2 smaller | ≈ 12 GB each | For lighter workloads or a canary host. |

**Network**

* **Wired Ethernet.** Setup Assistant's auto-advance only works on Ethernet, and 10GbE matters for cache traffic.
* A **DHCP reservation** per host (stable addresses help troubleshooting; hostd itself needs no inbound connections and
  works behind NAT).
* Keep `192.168.64.0/24` (Tart's NAT network on the host) out of your LAN ranges.
* **Outbound** access (no inbound rules are needed):

  | Destination | Port | Purpose |
  | --- | --- | --- |
  | `CONTROLLER_URL` host | TCP 8445, 8446, 8981, 8983 | enrollment, HostService (mTLS), storage (L2 upstream), scheduler (relayed for VMs) |
  | `ghcr.io`, `pkg-containers.githubusercontent.com` | TCP 443 | golden VM images (private GHCR package; short-lived pull credentials come from the controller) |
  | Package URL host: `github.com`, `release-assets.githubusercontent.com` (or your S3/CDN host) | TCP 443 | the Cucina package (redirect following verified with curl/URLSession, not yet Apple Business) |
  | Apple: device management, push and software update services (`*.apple.com`, `*.push.apple.com`, `*.cdn-apple.com`, `gdmf.apple.com`, `deviceenrollment.apple.com`, `mdmenrollment.apple.com`, `iprofiles.apple.com`, `albert.apple.com`, `time.apple.com`) | TCP 443, 2197, 5223; UDP 123 | ADE, MDM push, OS updates, certificate checks, time. Apple's complete list: "Use Apple products on enterprise networks". |

**Power.** A UPS is recommended. The host restarts by itself when power returns, but an abrupt power cut can still
damage a running VM's disk (hostd then re-clones it, which costs a cold L1 cache).

**FileVault must stay off on these hosts.** With FileVault on, macOS cannot log a user in automatically after a restart,
so the `cucina` user's login keychain stays locked and Tart cannot start VMs (`SecKeyCreateRandomKey_ios failed`,
R-MAC-2). The trade-off is physical: whoever can take the Mac mini can read its disk (build caches, VM images with Xcode,
the host's identity key). Keep hosts in a locked room or rack, and revoke a stolen host's identity (`cucinactl hosts
remove <serial>`).

The alternative is **FileVault on + SSH unlock (macOS 26+) + `fdesetup authrestart`** for planned restarts, with the
`cucina` login keychain created and unlocked from a script instead of a GUI login. Cucina does not default to it because
every unplanned restart (power loss, panic, forced update) leaves the host offline until a person or an automation holding
a FileVault password unlocks it over SSH (which defeats UC21), and Tart's own FAQ calls the scripted keychain unlock
fragile. If your policy requires encryption at rest, plan for that manual step after every outage.

## 2. Get the devices into Apple Business

1. **Bought from Apple or a linked reseller:** the Macs appear in Apple Business automatically (Devices). Note their
   serial numbers.
2. **Otherwise, with Apple Configurator for iPhone** (any Apple silicon Mac; the iPhone needs iOS 16 or later):
   1. Connect Ethernet and power. Start the Mac, choose a language, and **stop at "Select Your Country or Region"**
      (if you went further, restart the Mac).
   2. Open Apple Configurator on the iPhone, signed in to Apple Business, and hold it near the Mac; or click
      **Pair Manually** on the Mac and enter the six-digit code.
   3. Wait until the serial number has been uploaded, then click **Shut Down**.
   4. During the **30-day provisional period** a local user can still release the Mac from management. Keep new hosts
      physically controlled until it ends.
3. **Assign the devices to the device management service** (Devices > select > assign to *Built-in device management*),
   or set automatic assignment for Macs. If your Apple Business plan uses device subscriptions, new devices also need
   approval: select *Approve recently added devices for management without manual review* when you add them to the
   device Blueprint, or approve each one under Devices > Inventory after enrollment.

## 3. ADE enrollment settings

Create a **Blueprint assigned to the devices by serial number** ("Blueprints for service devices" or a custom Blueprint).
With a device Blueprint, Automated Device Enrollment needs no Managed Apple Account (device-assigned enrollment). Set:

* **Await configuration: on.** Setup Assistant waits until MDM has delivered the critical configurations (the trust
  certificate first, §4).
* **MDM not removable** (supervised).
* **Skip every skippable pane**, at least: Location, Apple ID / Apple Account, Siri, Apple Intelligence (`Intelligence`),
  FileVault, Software Update (`SoftwareUpdate`, macOS 15.4+), Screen Time, Privacy, Terms and Conditions, Diagnostics,
  Accessibility, Appearance, Touch ID (`Biometric`), Apple Pay, True Tone, iCloud Analytics and Storage, Unlock with
  Apple Watch, Terms of Address, Get Started (`Welcome`), Liquid Glass (macOS 27), Update Completed. **Remote
  Management** (the enrollment pane itself) can never be skipped. Apple's schema also lists `Restore` (Migration
  Assistant) as skippable on macOS; skip it if your MDM offers it.
* **Auto-advance:** Setup Assistant advances through all panes by itself. Requirements: Ethernet during the first boot,
  an MDM-created managed administrator account, and the account-creation pane skipped. Without auto-advance, someone has
  to click through the remaining panes once. *Verify in the Apple Business UI.*
* **A hidden managed local administrator** (`AccountConfiguration` → `AutoSetupAdminAccounts`, hidden) for
  break-glass access. Rotation plan: let the MDM rotate its password (`SetAutoAdminPassword`, macOS ADE) on a schedule
  (for example every 90 days) and after every use; store it only in the MDM or your password manager.
  *Verify in the Apple Business UI* whether the built-in service creates and rotates this account; the Cucina package does
  not need it.
* **Minimum macOS version** for enrollment, if offered: the latest macOS 27.x, so a new host never runs an OS older than
  its VM guests (R-VER-3).

Leave the Apple Business **FileVault** configuration out of this Blueprint (see §1).

## 4. What MDM pushes, in order

Order matters: a privately signed package installs only after its signing certificate is trusted.

| # | What | Cucina artifact | Apple Business |
| --- | --- | --- | --- |
| 1 | Trust for the package signer (+ Cucina CA) | `01-cucina-trust.mobileconfig` | Custom configuration, or a **Certificate** configuration with the signer certificate |
| 2 | Host agent settings | `02-cucina-hostd-preferences.mobileconfig` | Custom configuration |
| 2 | Managed login items (`Label` = `ai.sloper.cucina.hostd`) | `03-cucina-login-items.mobileconfig` | Custom configuration, or the package's **Add Items → Service Label** |
| 2 | Energy (never sleep, wake on LAN, restart after power loss) | `04-cucina-energy.mobileconfig` | Custom configuration, or **Energy Saver** (Restart after power failure, Wake for network access) |
| 2 | Auto-login | **nothing by default** (the package does it); `05-cucina-autologin.mobileconfig` only on MDMs that create the account | — |
| 2 | FileVault prevention | `06-cucina-filevault-off.mobileconfig` | Custom configuration |
| 2 | Firewall (on, stealth, hostd allowed) | `07-cucina-firewall.mobileconfig` | Custom configuration, or **Application Layer Firewall** with Stealth mode on and **Block all incoming connections off** |
| 3 | Software update policy | `macos/ddm/softwareupdate.*.json` | **Software Update** configuration with a custom schedule (§6) |
| 4 | The Cucina package | URL + SHA-256 + bundle ID | Devices > macOS Packages |
| 5 | macOS 27.x update before VMs run | — | Software Update configuration / minimum OS (§3) |

### 4.1 Render the profiles

The templates live in `macos/profiles/templates/`. Render them on an admin Mac, **outside any git checkout**
(`render.sh` refuses to write into one, because profile 02 contains the site token):

```sh
cp macos/profiles/example.env ~/.config/cucina/site-a.env   # edit CUCINA_CONTROLLER_URL etc.
printf '%s' '<site enrollment token>' > ~/.config/cucina/site-a.token && chmod 600 ~/.config/cucina/site-a.token
macos/profiles/render.sh --config ~/.config/cucina/site-a.env \
  --signer-cert installer.pem --signer-cert application.pem --ca-cert cucina-ca.pem \
  --token-file ~/.config/cucina/site-a.token --out ~/.config/cucina/profiles/site-a
```

Every rendered file lints (`plutil -lint`), is far below Apple Business's 1 MB limit, and has one common platform
(macOS). Details of each payload: [docs/mdm/profiles.md](../mdm/profiles.md).

### 4.2 Upload and assign

1. **Devices > Configurations > All Configurations > Custom**: upload each rendered `.mobileconfig`, choose macOS, save.
   (The built-in Certificate, Energy Saver and Application Layer Firewall configurations are fine substitutes for 01,
   04 and 07; never enable *Block all incoming connections*: the VMs connect to hostd over the host's internal network.)
2. **Devices > macOS Packages > Add**: Name `Cucina host agent`, URL = package URL, Hash = SHA-256, Bundle ID
   `ai.sloper.cucina.host`, version. The bundle ID is the **package identifier** — what macOS reports for declaratively
   installed packages (`package.list` status); the package contains no Cucina app bundle (`tart.app` carries Cirrus
   Labs' bundle ID). *Verify in the Apple Business UI* that the install status turns to installed. Under **Add Items**
   add *Service Label* `ai.sloper.cucina.hostd` so the daemon cannot be disabled in Login Items (same effect as
   profile 03).
3. Add the configurations, the Software Update configuration and the package to the hosts' Blueprint.

**Ordering in Apple Business — verify in the Apple Business UI.** Do not assume *Await configuration* serializes
Blueprint payloads or guarantees trust before the package. First assign the trust and configuration set; confirm it is
installed on the device; then assign the package (separate staged Blueprints/configuration assignments if needed).
Test this sequence on the first host. An untrusted-package failure means trust is missing or invalid; check the signer
and profile status as described in [§8](#8-troubleshooting) before retrying.

### 4.3 What the package does on the host

`postinstall` runs `/usr/local/cucina/bin/cucina-host-setup install`, which is idempotent and needs no network:

* creates the standard user **`cucina`** with a random password that never leaves the Mac, prepares its login keychain,
  and turns on auto-login; existing healthy accounts are preserved (ADRs 0751/0755);
* `pmset -a sleep 0 womp 1 autorestart 1` and `systemsetup -setrestartfreeze on` (settings no profile can set);
* vmnet DHCP lease 600 s (Tart's recommendation, avoids lease exhaustion);
* `/var/db/cucina` (state, L2 cache) and `/Library/Logs/Cucina`;
* allows hostd in the application firewall when the firewall is not profile-managed;
* bootstraps the LaunchDaemon `ai.sloper.cucina.hostd`;
* warns loudly if FileVault is on;
* **once, after the first install**, restarts the Mac 2 minutes later if nobody is logged in and Setup Assistant is done,
  so the `cucina` auto-login session (and its unlocked keychain) exists. Disable with the install setting
  `RestartAfterFirstInstall=false` (profile 02) if you prefer to restart through MDM.

Installed layout: `/usr/local/cucina/{bin,tart.app,share}`, `/Library/LaunchDaemons/ai.sloper.cucina.hostd.plist`,
`/etc/newsyslog.d/ai.sloper.cucina.conf`. Tart's official `tart.app` is shipped exactly as released (never re-signed).

### 4.4 Software update declarations

macOS 27 removed the legacy MDM software-update commands; updates are declarative only. Cucina ships examples in
`macos/ddm/` for MDMs that accept custom declarations: `softwareupdate.settings` per ring (major upgrades deferred 90
days, minor updates 7 days on the canary ring and 14 on the fleet, automatic downloads, automatic Background Security
Improvements and security responses, OS installs only by enforcement) and `softwareupdate.enforcement.specific` per ring
(target version + deadline in your maintenance window). In Apple Business use the **Software Update** configuration with
a **custom schedule** instead: its default (*recommended*) schedule force-installs upgrades after 14 days and updates
after 7 days at 5 p.m. local time, which can restart a busy host. *Verify in the Apple Business UI* whether custom
declarations can be uploaded. See [docs/mdm/ddm.md](../mdm/ddm.md).

### 4.5 Update to the latest macOS 27.x before VMs run

Hosts must run a macOS version at least as new as their VM guests (R-VER-3). Use the minimum-OS enrollment setting
(§3) or an enforcement deadline shortly after enrollment, and approve a new host in Cucina (§5) only once
`cucinactl hosts list` shows the expected macOS version.

## 5. First boot

1. **Before the Macs arrive**, pre-register their serial numbers (copy them from Apple Business):

   ```sh
   cucinactl hosts register C02XXXXXXXXX C02YYYYYYYYY --site site-a --label rack=a
   ```

   Pre-registered serials enroll without further approval. Unknown serials stay **pending** until
   `cucinactl hosts approve <serial>`.
2. **On site:** connect Ethernet and power, press the power button. Setup Assistant auto-advances, the Mac enrolls,
   receives the configurations, installs the package, restarts once, and logs in as `cucina`.
3. **Watch it register:**

   ```sh
   cucinactl hosts list            # the new host: approved, macOS version, VM slots, images
   cucinactl hosts approve <serial>   # only if it was not pre-registered
   ```

   hostd pre-pulls the golden images of the pools that select this host; the first pull of an Xcode image (≈ 70 GB)
   takes a while. `cucinactl hosts list` shows the progress.
4. **Run a first test build** from a developer Mac (`cucinactl bazelrc --platform macos >> .bazelrc`, then
   `bazel build --config=cucina-macos //your:target`), and check that it executed on a VM of the new host.
5. **Checks on the host** (Screen Sharing or SSH, §6):

   ```sh
   profiles status -type enrollment        # "Enrolled via DEP: Yes", "MDM enrollment: Yes (User Approved)"
   fdesetup status                         # "FileVault is Off."
   pmset -g | grep -E ' sleep|womp|autorestart'   # sleep 0, womp 1, autorestart 1
   sudo sfltool dumpbtm | grep -B2 -A8 ai.sloper.cucina.hostd   # enabled, allowed (managed with profile 03)
   sudo launchctl print system/ai.sloper.cucina.hostd | grep -E 'state|pid'
   sudo /usr/local/cucina/bin/cucina-host-setup status   # daemon, cucina user, auto-login, power, lease (no secrets)
   ```

## 6. Headless operations

* **No sleep, restart after power loss:** profile 04 plus the package's `pmset -a sleep 0 womp 1 autorestart 1`;
  `systemsetup -setrestartfreeze on` restarts the Mac after a system freeze.
* **Break-glass access.**
  * *Screen Sharing:* the MDM command `EnableRemoteDesktop` (supervised Macs; Apple Business: *verify in the Apple
    Business UI*; third-party MDMs expose it as "Enable Remote Desktop"). Log in with the hidden managed admin (§3).
  * *SSH:* no MDM switch exists. Run `sudo /usr/local/cucina/bin/cucina-host-setup ssh on` through your MDM's script
    feature (Apple Business: *verify in the Apple Business UI* whether it can run scripts) or in a Screen Sharing
    session; turn it off again with `ssh off`.
* **Hostnames:** the MDM `Settings` command (`HostName`, `DeviceName`) where available (Apple Business: *verify in
  the Apple Business UI*), or
  `sudo /usr/local/cucina/bin/cucina-host-setup hostname <name>` (sets ComputerName, LocalHostName and HostName; a
  `scutil --set LocalHostName` script does the same).
* <a id="macos-updates"></a>**macOS updates: drain → update in the maintenance window → uncordon.**
  1. `cucinactl hosts drain <host>` some time before the window: running actions finish, the VMs shut down, new work goes
     to other hosts. Drain the canary ring first.
  2. The enforcement deadline (`TargetLocalDateTime`) or the Apple Business custom schedule installs the update and
     restarts the Mac; the `cucina` user logs in automatically and hostd reconnects.
  3. Check `cucinactl hosts list` (new macOS version, host online), then `cucinactl hosts uncordon <host>`.
  Builds that were running on an undrained host when it restarted are retried elsewhere (UC8); draining only avoids the
  slowdown.

## 7. Day 2

* **Rotate the site enrollment token:** `cucinactl hosts enroll-token create --site site-a --ttl 30d --max-hosts <n>`,
  re-render profile 02 with the new token, replace it in MDM, then `cucinactl hosts enroll-token revoke <old-id>`.
  Enrolled hosts keep working (they hold their own identity).
* **Rotate a host identity:** hostd renews its short-lived certificate automatically before expiry. To force a new
  identity (suspected compromise): `cucinactl hosts remove <serial>`, run `sudo cucina-host-uninstall --yes` and reinstall
  the package (or wipe the host), then approve it again.
* **Rotate the private signing certificates** (yearly, before expiry; parallel trust): generate new application and
  installer identities with their respective `--purpose` values. Render profile 01 with old/new certificates for both
  roles (four `--signer-cert` arguments), push and wait for every host, then publish a new package signed with the new
  identities. Later remove the old pair from profile 01. Details and blast
  radius: [docs/mdm/signing.md](../mdm/signing.md).
* **Upgrade the host agent:** publish the new version (new URL per version), then in Apple Business **macOS Packages >
  the package > Updates > Update Package** with the new URL and hash (other MDMs: new manifest/version). The upgrade stops
  hostd, replaces binaries and Tart, and starts it again; VMs, caches and the host identity are preserved. Drain first if
  you want no running actions to be interrupted.
* **Add a new Xcode version** (R-VER-3): add the version/build pin and Packer variables in `workers/macos`, then build
  and smoke-check with `make -C workers/macos image-macos XCODE=<supported-version>` (27.0 is the current checked entry;
  a 27.1 entry is planned until that version exists in `versions.json`). Use the pinned Cirrus Xcode image when its build
  matches; otherwise obtain Apple's `.xip` with an authorized Apple Developer account and provide the extracted, licensed
  app via `XCODE_APP`. Never embed an Apple ID password, session, download credential or Xcode installation in this repository.
  Review the image's smoke evidence, publish through the operator-controlled image workflow, then add a parallel pool or
  change its image/generation and drain-roll existing VMs. Hosts pre-pull within their disk budget and must already run
  a macOS version ≥ the image's. See [the image runbook](../operations/macos-images.md) and ADR 0352.
* **Change VM size or image:** VM sizing via profile 02 (`VMSlots`, `VMCPUCount`, `VMMemoryGiB`) or the controller's
  host settings; the image via the pool (`WorkerPool` image reference). VMs are re-cloned at their next start.
* **Decommission a host:**
  1. `cucinactl hosts drain <host>`, then `cucinactl hosts remove <serial>`.
  2. Remove the software: on macOS 27 with an MDM that installs the package through the declaration
     `com.apple.configuration.package` with `UninstallBehavior.Remove = true` (set before the first install), removing the
     declaration deletes the installed files; restart the Mac afterwards so launchd drops the daemon. Otherwise deploy the
     uninstaller package (`make -C macos/pkg uninstall-pkg UNINSTALL_ARGS=--purge`) or run
     `sudo /usr/local/cucina/bin/cucina-host-uninstall --purge --yes` (stops hostd and its VMs, removes the daemon,
     binaries, logs, state, the host identity and, with `--purge`, the VMs, images and the `cucina` user). Note: package
     removal through Apple Business Blueprints leaves package files in place (they are not managed apps).
  3. Release the Mac from Apple Business, or erase it for reuse.

## 8. Troubleshooting

**Logs**

```sh
log show --last 1h --predicate 'subsystem == "ai.sloper.cucina"'      # hostd (unified logging)
sudo tail -f /Library/Logs/Cucina/hostd.log                          # hostd JSON log (rotated by hostd)
sudo tail /Library/Logs/Cucina/bb_storage.log /Library/Logs/Cucina/hostd.stderr.log
sudo tail /Library/Logs/Cucina/install.log /var/log/install.log      # package installs (pre/postinstall)
cucinactl hosts diag <host>                                          # collected remotely
```

| Symptom | Cause and fix |
| --- | --- |
| VMs fail to start: `SecKeyCreateRandomKey_ios failed`, `Failed to generate keypair`, `Interaction is not allowed with the Security Server` | The `cucina` login keychain is locked: the user is not logged in. Check `stat -f %Su /dev/console` (must be `cucina`) and `fdesetup status` (must be off). If auto-login was lost: `sudo cucina-host-setup autologin --reset-password` (new random password, old keychain moved aside), then restart. If FileVault is on, turn it off (`sudo fdesetup disable`) and deliver profile 06. |
| hostd cannot reach a VM / Local Network denial in the log | Local Network privacy exempts root daemons (hostd) but not privilege-dropped children; hostd makes every VM connection itself. If macOS still blocks the NAT subnet, set the escape hatch (macOS 15.5+) and restart: `sudo cucina-host-setup local-network allow 192.168.64.0/24` (or install setting `LocalNetworkAllowedEthernetAddresses`). |
| VMs cannot reach hostd (builds hang at start) | The application firewall blocks incoming connections to hostd: never enable *Block all incoming connections*; keep profile 07 (hostd allowed, signed with the trusted certificate). Check `/usr/libexec/ApplicationFirewall/socketfilterfw --listapps`. |
| Package install fails: untrusted / invalid signature | The trust certificate is missing, expired or not the signer of this package. Compare `pkgutil --check-signature <pkg>` (SHA-1 of the signer) with `security find-certificate -a -Z /Library/Keychains/System.keychain | grep -A1 'Cucina'`. Deliver profile 01 first, then re-push the package (Apple Business: Update Package, or remove and re-add it to the Blueprint). Manual installs outside MDM need the certificate trusted too. |
| Profile ordering: preferences arrived after the package | Harmless: hostd retries until its configuration is complete (exit code 2 = invalid configuration, launchd restarts it). Check with `sudo cucina-host-setup status`. |
| Host stays *pending* | Its serial is not registered: `cucinactl hosts approve <serial>`. |
| Enrollment refused (hostd exit code 3) | Token expired, revoked or out of hosts: create a new token and update profile 02. |
| FileVault turned itself on (macOS 26.4+ turns it on in Setup Assistant by default) | Skip the FileVault pane (ADE), deliver profile 06, check `fdesetup status` on every new host; disable it with `sudo fdesetup disable`. |
| hostd disabled in Login Items | Deliver profile 03 or the package's Service Label item; `sudo sfltool dumpbtm` shows the state. |
| VMs get no IP after many starts | DHCP leases exhausted: the package sets 600 s leases; if it happened before, `sudo rm /var/db/dhcpd_leases`. |
| Disk full | Too many images: hostd prunes to its budget (`tart prune --space-budget`); reduce pools per host or use a larger disk. |

## 9. Appendix — third-party MDMs

**Any MDM** that can deploy custom profiles and custom packages works the same way:

1. Upload the rendered profiles 01–07 (skip 05 unless the MDM creates the `cucina` account, see below). Scope them to the
   build hosts; deliver 01 before the package (pre-stage/enrollment configuration, or "await configuration").
2. Deploy the package by URL + SHA-256 + bundle ID, or upload the `.pkg` (the MDM then hosts it). MDMs that send
   `InstallEnterpriseApplication` or the DDM `com.apple.configuration.package` declaration use the manifest
   (`cucina-host-X-Y-Z.plist`) as is.
3. Software updates: the `macos/ddm` declarations if the MDM accepts custom declarations, otherwise its own
   update-enforcement feature with a maintenance-window deadline.
4. Optional: the DDM **background-tasks** variant (`macos/ddm/services.background-tasks.cucina-hostd.json`, macOS 15+,
   supervised) instead of the package's LaunchDaemon; see [docs/mdm/ddm.md](../mdm/ddm.md).

Notes per MDM (check each vendor's current documentation):

* **Jamf Pro:** its built-in CA can sign packages for PreStage enrollment; devices trust Jamf's CA through the MDM
  profile, so a Jamf-signed package needs no profile 01 (keep 01 if you also deploy the Cucina-signed package).
  Managed preferences: upload profile 02 or use *Application & Custom Settings* for the domains
  `ai.sloper.cucina.hostd` and `ai.sloper.cucina.host`. Jamf can create local accounts and manage a rotating local
  admin password; with Jamf creating `cucina`, deploy profile 05 with the same password and set `CreateUser=false`.
* **Iru (Kandji):** custom profiles and Custom Apps (package upload) with an enrollment/Liftoff flow; deliver 01 before the
  Custom App. Use its local-admin and password-rotation features for break-glass.
* **Mosyle:** custom profiles, PKG deployment and custom commands (useful for `cucina-host-setup ssh on` and hostname
  scripts).
* **Microsoft Intune:** macOS line-of-business apps must be signed with a **Developer ID Installer** certificate: build
  with the opt-in Developer ID path (`make -C macos/pkg sign SIGNING=developer-id …`, which also notarizes); upload
  profiles as custom configuration profiles; shell scripts are supported.
* **Fleet:** recommends Developer ID Installer–signed packages (use the Developer ID path); accepts custom configuration
  profiles and custom DDM declarations (the `macos/ddm` JSON files) and runs scripts.

With a Developer ID–signed package, profile 01 is only needed for Cucina's CA (or not at all if you pin the CA in profile
02); Managed Login Items can then also match the Team ID.

## Appendix B — verify on the first host (MT-001)

Capabilities that could not be verified without an Apple Business organization and real hardware. Check them once on
the first Mac mini and record the results in MT-001:

1. Auto-advance completes Setup Assistant over Ethernet with the Blueprint's settings (§3).
2. The hidden managed administrator (`AccountConfiguration`) is created, and its password can be rotated (§3).
3. Configurations (trust first) arrive before the package; the package installs without an "untrusted" failure (§4.2).
4. Apple Business accepts the package URL (GitHub release asset, `302` redirect) and the hyphenated `.pkg` name, and the
   install status turns to *installed* for bundle ID `ai.sloper.cucina.host` (§4.2, ADR 0753).
5. Custom declarations (software update rings) can be uploaded, or the built-in Software Update custom schedule is used
   (§4.4).
6. The first-install restart happens once, and the `cucina` user is logged in afterwards (§4.3).
7. `EnableRemoteDesktop`, scripts and the `Settings` command (hostname) are available (§6).
8. VMs reach hostd's relays through the application firewall with profile 07 (§8), and no Local Network denial appears.
9. hostd runs `tart` as `cucina` from the root LaunchDaemon (the path a VM-based test cannot cover).
