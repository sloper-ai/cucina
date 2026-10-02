<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# macOS worker images

Build, version, test and retain Cucina's private Tart worker images (R-MAC-3/4/7, R-VER-1..3,
R-OPS-2/-7, R-CACHE-2/-3, R-DATA-5). Source: [`workers/macos`](../../workers/macos/README.md).
The authoritative guest contract is [hostd §1](../dev/hostd.md). Decisions: ADRs
[0350](../adr/0350-macos-worker-image.md), [0351](../adr/0351-macos-build-directory.md),
[0352](../adr/0352-xcode-source-for-worker-images.md), [0353](../adr/0353-macos-runner-concurrency.md).

## Image identity and rollout

The local image is `cucina-worker-macos:<xcode>-<cucina_version>`. Its tag is recorded in
`/etc/cucina/image-version` and `/usr/local/cucina/image.json` (schema 1); future pushes add the same version as OCI
labels. Changing a WorkerPool's image reference initiates a new rollout generation: hostd pre-pulls the image and
re-clones each VM at its next idle stop. Ordinary scale-to-zero stops preserve the VM disk and L1.

The default is macOS 27 (Golden Gate), Xcode 27.0 **27A266a**. The pinned base already contains that exact build at
`/Applications/Xcode_27.app`; the build moves it to `/Applications/Xcode.app`. The image includes unmodified,
SHA-256-verified Buildbarn worker/runner binaries, the darwin/arm64 worker agent, and both Cucina licence/notices files.
Nothing Buildbarn-related starts before hostd injects configuration and credentials.

### Fixed exec-side toolchain paths (R-XPLAT-8)

| Item | Path / value |
| --- | --- |
| Only Xcode | `/Applications/Xcode.app` (real directory; no other Xcode or Command Line Tools) |
| `DEVELOPER_DIR`, `xcode-select -p` | `/Applications/Xcode.app/Contents/Developer` |
| SDK used by `-isysroot` | `/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX.sdk` |
| Toolchain binaries | `/Applications/Xcode.app/Contents/Developer/Toolchains/XcodeDefault.xctoolchain/usr/bin/` |
| Xcode override key | `27.0.0.27A266a`, recorded in `image.json.xcode.xcodeVersionOverride` |

`xcrun` may return the versioned `MacOSX27.0.sdk` path. Smoke checks require that it and the fixed alias resolve to the
same SDK, rather than requiring identical spelling. Clients need not hold or upload the SDK.

## Build and verify

On Apple silicon, use a host OS **at least as new as the guest**, Tart 2.40.1, Packer 1.16.1, Go, Bazelisk, jq and
shellcheck. Keep bulk files on the designated data volume and credentials on encrypted storage. The base must already
be cached in `TART_HOME`; the Makefile checks before Packer clones it. Never launch a second pull while another owns
Tart's download lock.

```sh
export TART_HOME="$CUCINA_DEV_STORAGE/tart"
make -C workers/macos validate
make -C workers/macos image-macos XCODE=27.0 CUCINA_VERSION=0.1.0-dev
```

The one-command pipeline is sequential, even with parallel make:
1. Verify/download pinned worker/runner binaries; build `cucina-worker-agent` or use `WORKER_AGENT=<binary>`.
2. Packer clones the digest-pinned Cirrus base, grows the disk, selects Xcode, prepares the standard build account,
   disables background updates/indexing, creates case-sensitive APFS, installs the payload, reboots and runs the
   in-guest smoke gate. Staging uses `/private/var/tmp`, which survives that reboot, and is removed afterwards.
3. Clone a throwaway VM for three boots and `tart exec` smoke/render checks. Reports distinguish guest-agent and
   GUI-session readiness, plus virtual capacity, backing-file length and allocated extents.
4. Fetch checksum-pinned **host-only** bb_storage/bb_scheduler and run a tiny remote C++ compile/link/test with both
   runner queues. No local fallback or action-cache acceptance is allowed. These servers are not baked into the image.
5. Report sizes; all test clones are stopped and deleted. No publishing is part of this command.

Useful controls: `PKR_VAR_cpu_count`, `PKR_VAR_memory_gb` size the bake; `VCPUS`/`MEMORY_MIB` size smoke clones;
`BOOTS` selects the number of timing samples. `SMOKE_BUILD_DIRECTORY` defaults to `nfsv4`.
Normal test sizing is hostd's formula: `(cores-2)/2` vCPUs and `(RAM-8 GiB)/2` memory.

Logs are under `$CUCINA_DEV_STORAGE/logs`; boot reports under `$CUCINA_DEV_STORAGE/macimage/reports`; remote-build
results under `$CUCINA_DEV_STORAGE/macimage/bench`. For explicit diagnostics, Packer accepts `-on-error=abort` with
`-var vm_name_override=cucina-imgtest-<name>`; that leaves an owned diagnostic VM rather than silently cleaning it.
Clean only the names you created: `make -C workers/macos clean-test-vms VMS='cucina-imgtest-<owned-name>'`.
`FORCE=1` replaces an existing local golden image; prefer a new version for normal rollouts.

## Xcode sources and new releases

1. Prefer a Cirrus `ghcr.io/cirruslabs/macos-<release>-xcode:<tag>` containing the required build. Pin its digest and
   exact Xcode build in `versions.json`; remove all other Xcodes during provisioning.
2. Otherwise supply an expanded Xcode bundle with `XCODE_APP=<path>`. The optional Packer share is read-only and used
   only to copy Xcode during the bake—not for build directories or CAS. The default campaign needs no share.
3. **Manual acquisition:** the operator signs in to <https://developer.apple.com/download/all/> with their Apple ID,
   downloads the requested `.xip` and expands it. Never store Apple ID passwords, cookies or app-specific passwords
   in the repository, CI configuration or image. The build performs licence acceptance and first launch in the guest.

A new Xcode is a `versions.json` entry plus `packer/xcode-<ver>.pkrvars.hcl`, then
`make -C workers/macos image-macos XCODE=<ver> CUCINA_VERSION=<version>`. After validation and a separately authorized
private release, update the pool's image reference/generation. Separate Xcode-keyed pools support side-by-side versions;
Tahoe/macOS 26 and beta variants are not downloaded or tested by this campaign. Update hosts through MDM before rolling
out a guest newer than their OS.

## Storage, concurrency and caching

* **Minimum virtual disk: 250 GB.** Tart's `--disk-size` uses decimal GB and cannot shrink an image. The current Tart
  adapter passes its `diskGiB` value through to that CLI only when growth is needed; equal/smaller requests keep the
  image's existing capacity. Set **250** to express the intended capacity clearly; a request for 120 does not shrink it.
* `/Volumes/cucina` is root-owned, case-sensitive APFS with ownership enabled and Spotlight off. Native build/input
  cache directories live there; persistent L1 and file pool are under `/var/db/cucina`, root 0700. L1 defaults to
  **40 GiB** and is separate from the native cache that bb_worker clears at startup.
* **NFSv4 is the measured choice** (ADR 0351). The image supports both modes; hostd/controller must resolve `auto`
  accordingly or send `buildDirectory=nfsv4` explicitly.
* Both Xcode and generic arm64 platforms offer vCPU slots on the same VM. CPU/memory oversubscription is possible
  during mixed workloads; lower concurrency factors for memory-heavy pools (ADR 0353).

| Budget item | Size / policy |
| --- | --- |
| Downloaded Golden Gate base | Tart reports 140 GB virtual, approximately 81 GB allocated |
| Golden worker | 250 GB virtual; exact backing-file/allocated sizes are in the generated boot report |
| Each persistent VM's growth | 40 GiB L1 + up to 16 GiB native input cache + file-pool high-water mark + build outputs |
| Host L2 | 200 GiB default (not part of the image) |

APFS clones share extents until modified; `du`/per-file allocation can count shared blocks more than once. Backing-file
EOF also need not equal virtual disk capacity. Budget several images, two VM working sets and host L2; keep meaningful
free space on a 1 TB host rather than sizing from compressed download size alone.

Tart's chunked OCI disk layers allow unchanged chunks to be reused across image versions. Supported macOS 27 hosts can
use **stacked OCI clones** (immutable parent + writable overlay); locally built campaign images use ordinary clones.
For sites, use a zot mirror with scheduled **sync** from private GHCR and idle-hour pre-pulls. Do not assume unverified
pull-through support for Tart media types. Hostd's cache budget uses `tart prune --space-budget=<n>` (decimal GB; no
`GB` suffix). No pruning or publishing was performed by this image task; downloaded bases are retained.

## Private publishing (prepared, not executed)

The only intended destination is the **private** `ghcr.io/sloper-ai/cucina-worker-macos` package because the image
contains Xcode. Before a release, verify package visibility is private. `scripts/push.sh` wraps `tart push` with version
labels and refuses without `CUCINA_PUBLISH=1`; publishing is a separate, explicitly authorized release operation.
Credentials are passed via `TART_REGISTRY_USERNAME`/`TART_REGISTRY_PASSWORD`, never stored in the image or keychain.

Hosts and site mirrors use a separate **read-only** package credential delivered by the controller to hostd over mTLS
at pull time. The credential-flow decision belongs to hostd's ADRs. GHCR storage/transfer are currently free, including
private packages, but GitHub may change that with notice. Nothing was published during this campaign.

## Local validation evidence

The final local image is **`cucina-worker-macos:27.0-0.1.0-dev`** (macOS 27.0/26A428, Xcode 27.0/27A266a).
A pristine Packer build passed in **3m44s**. Its guest smoke passed **52 checks**, and the host-side render call site
and tiny NFSv4 remote C++ build/test passed, including both runner property sets as an unprivileged user.

At **7 vCPUs / 20 GiB**, the final three starts measured:

| Phase | Samples | Median / maximum |
| --- | --- | --- |
| Guest Agent (`tart exec true`) | 18.3, 16.5, 25.5 s | 18.3 / 25.5 s |
| Builder GUI ready | 18.9, 17.8, 27.4 s | 18.9 / 27.4 s |

An earlier completed three-boot series included an **88.1 s GUI-ready outlier**; do not treat the final series as a
worst-case guarantee. These are boot subphase measurements, **not** the full Execute-at-zero → first-action NFR-P1;
controller-driven T13, WAN/cache behavior and post-shutdown L1 hit ratios remain campaign checks.

Virtual capacity is **250,000,000,000 bytes**; backing-file length **92,125,028,352 bytes**; allocated file extents
**87,929,040,896 bytes**. After cleanup, `du` reported **158 GiB** under TART_HOME (76 GiB retained base cache,
82 GiB golden VM, subject to shared-extent double counting). All `cucina-imgtest-*` VMs created by this task were deleted.
The source/base image and stopped golden remain; nothing was published. ADR 0351 contains the full native/NFSv4 data.

## Security and troubleshooting

* Cirrus' known admin credentials exist during the bake. Shared vmnet NAT is not an isolation boundary between VMs:
  build only alongside trusted guests, with no external port forwarding. Finalization disables SSH password and
  keyboard-interactive authentication, disables Screen Sharing and rotates the admin password.
* Actions run as standard `builder`, not root. Root owns the worker's PKI/state and log directory; each daemon owns its
  own log. Hostd must preserve those permissions. Use one pool per trust level.
* No controller identity, registry token or worker key is baked in. Local auto-login uses macOS's reversible
  `/etc/kcpassword` representation, root 0600. Its login keychain is prepared with the same password.
* Golden Gate's `sysadminctl` can return success while failing to update that file (`SACSetAutoLoginPassword error:22`)
  in SSH/system context. The image invokes it in the existing base administrator GUI bootstrap and verifies the stored
  password prefix using the pkg's shared codec. Smoke tests actual GUI and noninteractive keychain behavior.
* The pinned Cirrus base has SIP disabled. The build does not change host SIP; it removes inherited Rosetta payload,
  cache and the supplemental Apple receipts that `pkgutil --forget` leaves behind. No x86_64 runner is offered.

For a failed VM, inspect `tart run` output, `image.json`, `xcode-select -p`, `diskutil apfs list`, and the Guest Agent
job `system/org.cirruslabs.tart-guest-daemon`. Before hostd injection run
`tart exec <vm> sudo -n -- /usr/local/cucina/libexec/cucina-smoke`; afterwards pass `--configured` to expect running
Buildbarn jobs. A console owner alone is insufficient evidence of a usable session—the smoke also checks the login
keychain without an interactive prompt.
