<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Worker images (EC2 Linux and Windows)

Packer builds every EC2 worker image from this repository with pinned Buildbarn and tool downloads (R-POOL-8).
Linux sources are exact AMIs in the private source lock; Windows workers use the exact registered base AMI.
Building a new Windows base still selects a matching vendor image, and distro package repositories remain mutable;
installed versions are recorded, but builds are not bit-for-bit reproducible from package pins. Each AMI carries a
version label and a pool generation as tags; `WorkerPool.spec.image.amiSelector` picks the newest AMI by tag, and a new version
starts a new pool generation (R-OPS-2). Pins live in `workers/{linux,windows}/versions.json` (`pins`); every build
writes what it actually installed to the `installed` section of the same file (R-VER-1).

| Image family | Template | Source | Builder | Root volume |
| --- | --- | --- | --- | --- |
| `linux-ubuntu-x86_64` (default Linux) | `workers/linux/packer` | Exact private pin selected from Canonical `ubuntu/images/hvm-ssd-gp3/ubuntu-resolute-26.04-amd64-server-*` | m7i.large (2 vCPU) | 8 GiB gp3 |
| `linux-ubuntu-arm64` (Graviton) | same, `ARCH=arm64` | Exact private pin selected from Canonical `...-26.04-arm64-server-*` | m7g.large (2 vCPU) | 8 GiB gp3 |
| `linux-al2023-{x86_64,arm64}` (alternative) | same, `VARIANT=al2023` | Amazon `al2023-ami-2023.*-kernel-6.18-*` | as above | 8 GiB gp3 |
| `windows-base` (also the `windows-client` AMI) | `workers/windows/packer`, stage `base` | Amazon `Windows_Server-2025-English-Full-Base-*` | m7i.large (2 vCPU) | 40 GiB gp3 |
| `windows-worker` | stage `worker` (source: exact registered `windows-base`) | own `windows-base` | m7i.large (2 vCPU) | 60 GiB gp3 |

Build and probe defaults are two-vCPU `.large` instances. `BUILDER_TYPE` selects the Packer builder; historical
measurements below retain their original instance types and must not be mistaken for new small-instance results.

## Build, publish, roll out

```sh
source .work/env.sh                    # AWS profile/region, CUCINA_RUN_ID/CUCINA_EXPIRES, caches (e2e environment)
make -C workers/linux agent ARCH=x86_64 && make -C workers/linux agent ARCH=arm64 && make -C workers/windows agent
make -C workers/linux   image-linux ARCH=x86_64 VARIANT=ubuntu      # ~7 min
make -C workers/linux   image-linux ARCH=arm64  VARIANT=ubuntu      # ~7 min
make -C workers/linux   image-linux ARCH=x86_64 VARIANT=al2023      # ~4 min (alternative)
make -C workers/windows image-windows STAGE=base                    # ~27 min (VS install itself ~2.5 min)
make -C workers/windows verify-bazel                                # rules_cc MSVC autodetection on the new base (~6 min)
make -C workers/windows image-windows STAGE=worker FAST_LAUNCH=1 SYSPREP_GATE=$CUCINA_DEV_STORAGE/logs/images/verify-bazel.result
make -C workers/linux validate && make -C workers/windows validate  # packer fmt/validate, shellcheck, PSScriptAnalyzer
make -C workers/linux measure-boot ARCH=x86_64 VARIANT=ubuntu N=5   # boot measurements (terminates the instances)
workers/windows/scripts/measure-boot.sh --count 2                 # Windows cold start; no user data on agent images
workers/linux/scripts/selftest-instance.sh --ami <ami>              # launch, agent selftest + not-a-worker check, terminate
workers/linux/scripts/prune-amis.sh <family>                        # keep current + previous AMI
```

* Network, builder security group and the admin `/32` come from `~/.config/cucina/aws-e2e/base-outputs.json` (never
  committed). Builders run in the public subnet with a public IPv4; SSH/WinRM is reachable from the admin `/32` only.
  Packer generates a throwaway key pair per build; Windows builds use EC2's random Administrator password
  (GetPasswordData) and WinRM over HTTPS with a self-signed certificate that the last step removes again.
* Every resource carries `cucina:env`, `cucina:run`, `cucina:expires` (`tags`, `run_tags`, `run_volume_tags`,
  `snapshot_tags`). Builders terminate on success and on failure.
* Linux source selection (ADR 0307): `~/.config/cucina/aws-e2e/source-amis.json` (0600), keyed by image family,
  contains the deliberately selected `ami_id`, source name, region, architecture, selection time and recorded
  versions. `SOURCE_AMIS_FILE` overrides its location. A missing lock/entry fails before Packer; builds never
  silently select a newer Linux base. Update the private pin deliberately for an OS upgrade. Windows worker builds
  use the exact `windows-base.ami_id` in the private AMI registry; no VS base rebuild is needed for a worker-only update.
* AMI registry for the e2e environment: `~/.config/cucina/aws-e2e/amis.json` (0600),
  `{ "<family>": { ami_id, name, image_version, generation, created, source_ami, previous: {...} } }`.
* AMI tags: `cucina:image-family`, `cucina:image-version` (`YYYYMMDD.HHMM` of the build), `cucina:generation`
  (1 + the highest generation of the family; override with `GENERATION=`), `cucina:os`, `cucina:arch`,
  `cucina:variant`/`cucina:stage`, `cucina:bb-version`, `cucina:worker-agent` (`yes` when the agent is baked in),
  `cucina:source-ami`, `cucina:source-name`.
* Retention: `prune-amis.sh` keeps the newest two AMIs of a family (current + rollback), deregisters older ones and
  deletes their snapshots; for Windows it first runs `fast-launch.sh disable` and waits.
* Adopting a new OS/VS/Buildbarn release (R-VER-3): edit the `pins` block, rebuild (the smoke checks are part of every
  build: qemu runners, agent `selftest`, MSVC compile/run, Windows service start/stop), then bump the pool generation.
* The Windows build can wait for the Bazel/MSVC verification before sysprep (`SYSPREP_GATE=<result file>`, first line
  `pass`); it fails closed otherwise.

## What is in the images

### Legal payload (R-ARTIFACT)

Both Windows stages and every Linux variant stage and install the repository's `LICENSE.md`,
`THIRD_PARTY_NOTICES.md`, and all maintained `tools/notices/texts/*.txt` files. The final destinations are:

* Linux: `/usr/share/doc/cucina/{LICENSE.md,THIRD_PARTY_NOTICES.md,licenses/*.txt}`.
* Windows: `C:\ProgramData\cucina\doc\{LICENSE.md,THIRD_PARTY_NOTICES.md,licenses\*.txt}`.

The installers require a complete nonempty payload, verify copies against the staged inputs, and the image smoke
checks require the installed files. Cleanup retains them, along with package-manager/vendor copyright files.
`bazelisk test //workers/linux:notices_test --runs_per_test=20` exercises payload preservation and missing-input
rejection; Windows PowerShell copy/rejection checks and both Packer validations also run locally.
Fresh-instance selftest now verifies the legal payload and writes private attestations under
`~/.config/cucina/aws-e2e/images/attestations/`: actual file SHA-256/byte sizes, binary hashes, installed versions,
source/AMI metadata and fatal selftest results. Pre-payload rollback/comparison AMIs are not retroactively compliant;
only images whose new attestations pass qualify.

### Common worker layout (aligned with `cucina-worker-agent`, docs/dev/worker-agent.md)

| | Linux | Windows |
| --- | --- | --- |
| Buildbarn binaries | `/usr/local/bin/bb_{worker,runner}` | `C:\bb\bin\bb_{worker,runner}.exe` |
| Rendered configuration (agent) | `/etc/cucina/bb/{worker,runner}.json` | `C:\ProgramData\cucina\bb\{worker,runner}.json` |
| Service environment (agent) | `/etc/cucina/env` (`EnvironmentFile=-`) | `C:\ProgramData\cucina\env` (copied into the services' `Environment` by the boot task) |
| PKI (agent) | `/etc/cucina/pki` (0700) | `C:\ProgramData\cucina\pki` (SYSTEM/Administrators) |
| Per-boot state, dead-man marks | `/run/cucina/{last-activity,last-contact}` | `C:\ProgramData\cucina\run\{last-activity,last-contact}` |
| Runner socket directory | `/run/cucina` | `C:\ProgramData\cucina\run` (runner account may write) |
| Worker agent | `/opt/cucina/bin/cucina-worker-agent` | `C:\bb\bin\cucina-worker-agent.exe` |
| Bootstrap hook | `/opt/cucina/bin/cucina-bootstrap` (ExecStartPre of `bb-runner.service`) | `C:\bb\bin\cucina-bootstrap.ps1` (run by the startup task `\cucina\cucina-boot`) |
| Build directory | FUSE under `/var/lib/cucina` (build root 0755) | WinFSP Mount Manager mount `\\.\B:` |
| Instance store | formatted + mounted at `/var/lib/cucina/ephemeral` (`/run/cucina/instance-store.json`) | agent (NTFS) or, for Dev Drive mode, the image (`D:`) |
| Action user | `bbrunner` uid/gid 2000 via `run_commands_as` (bb_runner runs as root) | virtual account `NT SERVICE\cucina-bb-runner` |
| Logs | journald (volatile): `journalctl -u bb-worker -u bb-runner -u cucina-worker-agent` | `C:\ProgramData\cucina\logs\{bb-worker,bb-runner,agent}.log` (shawl, rotated at 50 MB, one old file kept), `boot.log`, `deadman.log` |
| Image facts | `/etc/cucina/image.json`, `/etc/cucina/selftest.json` | `C:\ProgramData\cucina\image.json`, `...\image\*.json` |

Agent contract as implemented by the images:

* `bootstrap` runs before Buildbarn at every boot. Exit 0: start Buildbarn. Exit 2 (no EC2 user data at all: image
  build, EC2 Fast Launch preparation): "not a worker" — `not-a-worker` mark in the run directory, Buildbarn stays down,
  the instance stays up, the dead-man switch only enforces 30 min / 12 h uptime. Any other exit: fail closed (the agent
  has already requested power-off).
* Without the agent binary the hook succeeds trivially and the Buildbarn units are skipped unless a configuration is
  present (Linux `ConditionPathExists=`, Windows `cucina-boot`), so agent-less images boot cleanly to `running`.
* `supervise` runs as `cucina-worker-agent.service` / shawl service `cucina-worker-agent` (only with the binary).
* `bb-worker` gets SIGTERM / Ctrl-C on stop and 110 s to drain (inside a Spot notice).
* Image builds install a drop-in `CUCINA_AGENT_NO_POWEROFF=1` on the Linux units for the verification reboot (removed
  before capture), so an agent failure can never power the builder off (Packer builders get an empty user data
  document; early agent builds treated that as a failed bootstrap instead of "not a worker").
* Every build runs `cucina-worker-agent selftest` (Linux after the reboot, Windows in the worker smoke test) and fails
  on any failed check; `selftest-instance.sh` repeats it on a freshly launched instance of the finished AMI.

### Dead-man switch (R-POOL-7)

Every minute (systemd timer `cucina-deadman.timer` / scheduled task `\cucina\cucina-deadman`, armed from boot, never
during image builds) the worker powers itself off — terminate, because pools launch with
`InstanceInitiatedShutdownBehavior=terminate` — when idle > 30 min, without scheduler contact > 10 min, or up > 12 h,
reading the modification time of the agent's marks (missing = "at boot"). A "not a worker" instance is held to 30 min
of uptime only. Limits: `/etc/cucina/deadman.env`, `C:\ProgramData\cucina\deadman.json` (the agent syncs the pool's
limits into them); `deadman-disabled` in the run directory pauses it until the next boot. Status of the last
evaluation: `deadman-status.json` next to the marks. Works without the controller and without the agent; it is the
backstop for the agent's own `supervise` dead-man.

### Linux (R-POOL-4, ADR 0305, ADR 0306)

FUSE 3 (`user_allow_other`; bb_worker runs as root, render `mountMethod: DIRECT`, `allowOther: true`); chrony against
the Amazon Time Sync Service (169.254.169.123, fd00:ec2::123). Ubuntu uses only these sources; AL2023 prefers Amazon's
source and retains its stock fallback pool. Ubuntu SSM Agent comes from the pinned `.deb` (not the snap), AL2023 from
its package repository; both use dual-stack endpoints. Linux has units `cucina-format-instance-store` -> `bb-runner` (ExecStartPre bootstrap) -> `bb-worker`
(`After=network-online.target`), `cucina-worker-agent`, `cucina-deadman.timer`. Boot trims: no snapd, no SSH daemon
(SSM only), no apt/motd/fwupd/man-db timers, no apport/ModemManager/multipathd/udisks2/iscsi/lvm monitors, no
rsyslog/cron/atd/hibinit-agent/sysstat/systemd-firstboot, cloud-init limited to the EC2 datasource and a minimal module
list, volatile journal, `noatime`, fsck off, GRUB timeout 0, `networkd-wait-online --any`, and Ubuntu's initrd-less
boot kept working (`grub-initrd-fallback.service` must stay enabled). The x86_64 Ubuntu image adds qemu-user
(`qemu-user-binfmt`, fix-binary registrations) and cross glibc/libstdc++ runtimes for riscv64, s390x and armhf, usable
without `QEMU_LD_PREFIX`; every build runs `/opt/cucina/bin/cucina-qemu-smoke-test` (static musl + dynamic glibc
hello binaries built with a pinned zig) and `cucina-worker-agent selftest` after a reboot. The AL2023 artifact is
retained for boot comparisons; it omits qemu/cross runtimes and is not a drop-in replacement for the default x86_64
pool's emulated runners (ADR 0305). AL2023 arm64 is template-validated only, not built in this campaign.

### Windows (R-POOL-5, ADR 0301–0304)

* **base** — Windows Server 2025 Datacenter + VS 2026 Build Tools via the shared, standalone
  `workers/windows/scripts/install-vs.ps1` (fixed-version bootstrapper + local channel manifest) + VC++ redistributable +
  WinFSP + shawl + Git for Windows + Bazelisk (`C:\tools\bin\bazel.exe`); long paths, Developer Mode, 8.3 names and
  last-access updates off; Windows Update, Delivery Optimization, telemetry, maintenance, defrag, WER UI and other
  post-boot CPU hogs off; Defender exclusions; EC2Launch v2 without the wallpaper task; SSM Agent on dual-stack
  endpoints; time from 169.254.169.123; .NET assemblies precompiled (ngen) during the build. **Not sysprepped**:
  `ec2launch reset --clean`, so instances launched from it (Packer `worker` stage, `windows-client`) get a new random
  Administrator password and run their user data. `TMP`/`TEMP` for the windows-client: `C:\bb\tmp` (exists on clients
  and workers, writable for Users).
* **worker** — base + `bb_worker`/`bb_runner` (pinned, SHA-256) as shawl services, the `\cucina\cucina-boot` startup task
  (instance store, bootstrap hook, services), accounts, dead-man task, the agent; a live smoke test (agent `selftest`; runner starts as its virtual account and
  serves its socket; worker mounts WinFSP `B:`; SCM stop drains) runs before **`ec2launch sysprep --shutdown --clean`**.

Residual risk on Windows (R-SEC-5): `bb_worker` runs as LocalSystem (Mount Manager mounts, L1/filePool), so a
compromise of bb_worker itself — not of an action — owns the machine. Actions run as one shared low-privilege virtual
account (no per-action isolation, as with Linux `run_commands_as`), can read world-readable machine files and can touch
the run directory (dead-man marks, i.e. keep their own worker alive up to the 12 h limit). Pools are the trust boundary.

### Defender: exclusions vs Dev Drive (ADR 0303)

Default `defender_mode=exclusions`: path and process exclusions for the build/cache/toolchain paths and tools, real-time
protection on, no scheduled scans. `defender_mode=devdrive` additionally formats a raw instance-store disk as a Dev
Drive (trusted, performance mode; plain ReFS where unavailable) before bootstrap. Builds run inside the WinFSP mount
(`B:`), not on a disk, so a Dev Drive only covers L1/filePool/TMP while exclusions cover everything; measuring both
needs a pool with instance store and real build load (the e2e campaign) — until then exclusions stay the default.

## EC2 Fast Launch (Windows, R-POOL-2)

```sh
workers/windows/scripts/fast-launch.sh enable  --ami <ami> --count 4 --wait   # TargetResourceCount = pool max
workers/windows/scripts/fast-launch.sh status  --ami <ami>
workers/windows/scripts/fast-launch.sh tag     --ami <ami>                    # campaign tags on its snapshots
workers/windows/scripts/fast-launch.sh disable --ami <ami>                    # waits until it reports disabled
```

The prep launch template (m7i.large, private subnet, IMDSv2; Fast Launch rejects user data, terminate-on-shutdown and
ENI tags in it) comes from the e2e output `fast_launch_template_id`, else the script creates a tagged one.
`make image-windows STAGE=worker FAST_LAUNCH=1` enables it from the Packer post-processor. Observed: preparation runs in
AWS-managed capacity (no prep instances appear in the account); 4 snapshots were ready ~4 minutes after enabling and a
consumed snapshot is replaced within minutes. Snapshots are tagged `CreatedBy: EC2 Fast Launch` and described
`This is Fast Launch snapshot for image <ami>`. The service-linked role `AWSServiceRoleForEC2FastLaunch` already
existed in the account (created 2026-08-20) and is left in place. Disable (and wait) before deregistering an AMI, on
rollout and at teardown.

## Measurements (2026-10-02, us-west-1b)

### Linux boot comparison (c7i.large / c7g.large; 5 sequential launches)

These exploratory measurements used Ubuntu generation 5 / AL2023 generation 4, before the final empty-user-data
agent fix. They describe OS boot, **not a usable rollback image or enrollment readiness**. The final generations
retain the same boot tuning; their separate post-fix launch checks are below.

Launch = the `RunInstances` call on the dev machine; "running" = systemd startup finished
(`systemctl is-system-running`), Buildbarn units resolved; "SSM" = first `PingStatus=Online`.

| Image | launch→kernel p50 | launch→running p50 (max) | launch→SSM online p50 (max) | `systemd-analyze` (typical) |
| --- | --- | --- | --- | --- |
| Ubuntu 26.04 x86_64 (gen 5) | 6.5 s | **15.8 s** (23.2) | **20.7 s** (31.8) | kernel 1.4 s + userspace 8.1 s, no initrd |
| Ubuntu 26.04 arm64 (gen 5) | 7.5 s | **16.3 s** (25.0) | **19.7 s** (31.9) | kernel 0.6 s + userspace 8.4 s, no initrd |
| Amazon Linux 2023 x86_64 (gen 4) | 5.8 s | **12.0 s** (18.2) | **15.3 s** (22.3) | firmware+loader 1.4 s + kernel 0.3 s + initrd 0.9 s + userspace 5 s |

* AL2023 boots ~4 s faster to `running` and ~5 s faster to SSM than Ubuntu (smaller userspace: no snap/cloud-init
  network stage work, faster cloud-init 22.2); Ubuntu stays the default per R-POOL-4.
* The first launch of a freshly registered AMI is the slowest (cold snapshot: +5–10 s); later launches are stable.
* With a throwaway Buildbarn configuration (images without the agent) `bb-worker.service` was active at the same moment
  as `running` (Ubuntu x86_64 p50 15.3 s, AL2023 12.0 s) and the FUSE build directory was mounted.
* Trims that mattered (critical chain): keeping Ubuntu's initrd-less GRUB boot (initrd 3 s + up to 10 s of cold reads
  of a 43–58 MB initrd removed), masking `sshd-keygen`/`systemd-firstboot`/`hibinit-agent`/`systemd-boot-update`,
  reduced cloud-init (the remaining biggest item: cloud-init-main + -local ≈ 3.5 s on Ubuntu), agent bootstrap ≈ 2.7 s
  on first start (17 MB binary read from a cold snapshot).
* EBS volume-initialization rate 300 MiB/s on an earlier 8 GiB Ubuntu x86_64 root: `running` took 16.1/13.7/15.1 s
  (n=3, median 15.1), versus 24.3/16.9/16.3/12.9/13.8 s without it (n=5, median 16.3). The first uninitialized
  launch was slow, but later baseline runs were as fast or faster. This small, non-randomized sample does not show
  a reliable gain; the billed per-launch option stays off.

### Windows cold start (worker image, c7a.2xlarge, public subnet)

| Path | launch→OS boot | launch→EC2Launch ready | launch→SSM online | launch→bb_worker running¹ |
| --- | --- | --- | --- | --- |
| Slow path (no Fast Launch snapshot), n=2 | 122–123 s | 145–150 s | 166–167 s | 173–175 s |
| **Fast Launch** snapshot available, n=4 | **20–24 s** | 50–61 s | **72–88 s** (p50 74 s) | 82–107 s (n=2) |

¹ These earlier, agent-less image measurements used throwaway configuration written by EC2 user data, which
EC2Launch runs late. They do not establish final-image enrollment or action readiness. On the fixed-agent generation
4, one earlier Fast Launch run reached OS boot at 19.4 s, SSM at 58.5 s, and the startup task at 77.5 s. Task
Scheduler contributes about a minute after OS boot; it does **not** run bootstrap immediately. An automatic-service
experiment (generation 5) failed post-sysprep launches with a fatal Windows setup dialog and was rejected (ADR 0302).
The validated startup-task generation 4 is reused. Boot-task breakdown observed: runner first start 7–10 s (virtual
account first logon), worker start 0.3 s, WinFSP mount 1.3 s; the image-side disk probe (5–9 s) is skipped when the
agent is present. `--with-config` is deliberately refused on agent-bearing images because their user data must be
Cucina boot data, not a PowerShell test script.

### Agent-fix checks before the legal-payload rebuild

Fresh launches with fixed agent `20261002.1034`, after the image-side fatal selftest was restored (historical image generations):

| Image | Generation | Launch→SSM online | Full selftest / no-user-data behavior |
| --- | --- | --- | --- |
| Ubuntu x86_64, current | 7 | 28.1 s | pass / stays up, Buildbarn stopped |
| Ubuntu x86_64, rollback | 6 | 24.8 s | pass / stays up, Buildbarn stopped |
| Ubuntu arm64 | 6 | 25.0 s | pass / stays up, Buildbarn stopped |
| AL2023 x86_64 | 5 | 15.8 s | pass / stays up, Buildbarn stopped |
| Windows, restored Fast Launch, c7a.xlarge | 4 | 64.1 s | pass, including disks / stays up, Buildbarn stopped |

Each row is one verification, not a p50 estimate. An additional Windows c7a.2xlarge Fast Launch sample reached OS
boot at 23.0 s, EC2Launch ready at 56.6 s, SSM at 63.4 s, startup-task start at 84.3 s, and no-user-data completion
at 86.1 s. CPU averaged 5.1% over the following 20 s. Without enrollment data, no Buildbarn process should start;
these timings **do not prove Execute→first-action NFR-P1**. Enrollment/action and Defender workload benchmarks
belong to the end-to-end campaign. All image-test instances and their volumes/network interfaces were removed.
These checks established a usable fixed-agent baseline; the later legal-payload rebuild supersedes the current images.

### Legal-payload image verification

All three rebuilt worker images carry agent `20261002.legal` and passed fresh-instance fatal selftest, no-user-data
behavior, and on-image legal checks. Each contains **nine legal documents, 879,699 bytes total**, byte-identical to
the repository inputs. Private attestations also record hashes/sizes of the three installed executables.

| Worker image | Generation | Verification instance | Launch→SSM | Legal payload / selftest |
| --- | --- | --- | --- | --- |
| Ubuntu x86_64 | 8 | c7i.large, 2 vCPU | 24.4 s | pass / pass |
| Ubuntu arm64 | 7 | m7g.large, 2 vCPU | 23.7 s | pass / pass |
| Windows, Fast Launch | 6 | m7i.large, 2 vCPU | 69.4 s | pass / pass, including disks |

These are single boot verifications, not performance benchmarks. The already-running larger bakes were allowed to
finish when the campaign switched to two-vCPU instances; subsequent ARM baking and all later checks used `.large`.
Ubuntu x86_64 generation 7 is retained as the T12 baseline and generation 8 is the compliant successor. Old rollback,
base/client and AL2023 comparison images remain pre-payload artifacts; they are not represented as compliant.

### Standing cost and spend

* Final retained set: **8 AMIs and 12 snapshots** (8 AMI snapshots + 4 completed, tagged Fast Launch snapshots for
  Windows generation 6). At $0.055/GB-month, their full snapshot sizes give an upper bound of **$15.30/month**
  (about $0.51/day). This is not an observed bill: shared incremental blocks can cost less. Initial Fast Launch
  preparation and replenishment also consume instance time.
* Earlier image work: 10 Windows builds (113.4 instance-minutes), 23 Linux builds (112.7 minutes), 61 logged
  test/probe launches (158.2 minutes, conservatively using log completion times). Rounded-up instance rates give
  about $3.50 ordinary EC2; allowing for Fast Launch preparation and storage gives an estimated $5–10, with a
  **$15 budget reserve**. This is not billing-verified.
* Resumed work first reused valid artifacts, built one Ubuntu successor for T12, and restored working Windows
  orchestration (~$0.12 ordinary EC2). The necessary legal-payload rebuilds and their checks added approximately
  **$0.16 ordinary EC2**. A conservative **$5 cumulative round reserve** covers both phases plus unobserved
  preparation/storage and an old-pool re-enable, below the $25 cap. Timestamped evidence stays private.
* Broken Windows generations 3/5 and pre-fix Linux rollback images were removed. Current worker images are Ubuntu
  x86_64 generation 8, ARM generation 7 and Windows generation 6; one previous per worker family, the Windows
  base/client and the AL2023 comparison image remain until campaign teardown. Older retained images lack the new
  legal payload and are explicitly only rollback/comparison inputs.
* Retiring Fast Launch requires updating the live pool's image reference and waiting for its observed generation
  first. A paused/max-zero pool still ensures Fast Launch for its selected image: generation 4 was re-enabled after
  an initial disable until the live pool converged on generation 6. After convergence, generation 4 was disabled
  and its prepared snapshots were verified gone; generation 6 alone retains four. Leave
  `AWSServiceRoleForEC2FastLaunch` in place. Image builders/tests, volumes and ENIs were all verified at zero.

## Installed versions (2026-10-02)

| Component | Version |
| --- | --- |
| Windows | Windows Server 2025 Datacenter 24H2, build 26100.33451 (source `Windows_Server-2025-English-Full-Base-2026.09.17`) |
| Visual Studio Build Tools | 2026, 18.10.3 (18.10.12224.181), channel `VisualStudio.18.Release` |
| MSVC | toolset 14.51.36231 (cl.exe 19.51.36260, `_MSC_FULL_VER` 195136260) |
| Windows SDK | 10.0.28000.0 |
| VC++ redistributable (x64) | 14.51.36247 |
| WinFSP | 2.1.25156 (WinFsp 2025) |
| shawl | 1.9.0 |
| Git for Windows | 2.56.0.windows.1 |
| Bazelisk | 1.29.0 (Bazel 9.2.0 via `.bazelversion`) |
| EC2Launch v2 / SSM Agent (Windows) | 2.5.2 / 3.3.5226.0 |
| Buildbarn | bb-remote-execution `20260930T173749Z-1a3be95` (bb_worker, bb_runner; SHA-256 in versions.json) |
| Ubuntu | 26.04.1 LTS, kernel 7.0.0-1012-aws, systemd 259.5, cloud-init 26.1, chrony 4.8, fuse3 3.18.2 |
| SSM Agent (Ubuntu) | 3.3.5390.0 (.deb) |
| qemu-user | 10.2.1 (`1:10.2.1+ds-1ubuntu3.2`), cross glibc 2.43 (riscv64, s390x, armhf) |
| Amazon Linux 2023 | 2023.12.20260930, kernel 6.18.51, systemd 252.23, cloud-init 22.2.2, chrony 4.3, fuse3 3.10.4, SSM Agent 3.3.5226.0 |

Bazel pins for MSVC (clients and workers): `--repo_env=BAZEL_VC=C:\BuildTools\VC`
`--repo_env=BAZEL_VC_FULL_VERSION=14.51.36231` `--repo_env=BAZEL_WINSDK_FULL_VERSION=10.0.28000.0`. Verified on an
instance from the base AMI with Bazel 9.2.0 and rules_cc 0.2.25 (`workers/windows/scripts/verify-bazel.sh`): build,
run and `bazel test` pass; the compile action uses `MSVC/14.51.36231/bin/HostX64/x64/cl.exe` and the 10.0.28000.0
include directories. No VS 2022 fallback is needed.
