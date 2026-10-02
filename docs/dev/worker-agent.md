<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# cucina-worker-agent

`cmd/cucina-worker-agent` (wiring) and `internal/workeragent` (logic) make an EC2 instance a Buildbarn worker at every boot
and keep it from outliving its purpose. One pure-Go binary (`CGO_ENABLED=0`) for `linux/amd64`, `linux/arm64`,
`windows/amd64` and `darwin/arm64` (Tart VMs use only `render`). Contracts: `docs/contracts.md` §5.2; decisions: ADR 0570–0574.

| Subcommand | Run by | Does |
| --- | --- | --- |
| `bootstrap [--boot-data F] [--deadline 2m] [--build-user bbrunner] [--key-reader ACCT]` | Linux: `bb-runner.service` `ExecStartPre=/opt/cucina/bin/cucina-bootstrap`; Windows: `cucina-boot.ps1` → `cucina-bootstrap.ps1` | enroll, write PKI, mount L1, render configs; exit 0 only when everything `bb_runner`/`bb_worker` need exists, otherwise power off |
| `supervise` (alias `run-supervisor`) `[--metrics-listen 127.0.0.1:9982] [--worker-service S]` | `cucina-worker-agent.service` / shawl service `cucina-worker-agent` | dead-man switch, Spot drain, certificate hygiene, `/metrics` |
| `render --settings F --machine F --out DIR` | hostd / `cucina-render` in Tart VMs, tests, debugging | `worker.json`, `runner.json`, `env`; prints the plan (directories, notes) as JSON |
| `selftest [--out F] [--build-user bbrunner]` | image smoke tests | JSON report (exit 1 unless all pass): `imds`, `disks`, `time-sync`, `virtual-filesystem`, `buildbarn-binaries`, `build-user` |
| `version` | humans | `cucina-worker-agent <version> <os>/<arch>` (`-X main.version=…`) |

Global flags: `--log-level`, `--log-file` (Windows default `C:\ProgramData\cucina\logs\agent.log`; Linux logs go to journald),
`--imds-endpoint` (`CUCINA_IMDS_ENDPOINT`), `--root DIR` (re-roots every path; development), `--no-poweroff`
(`CUCINA_AGENT_NO_POWEROFF=1`; failures are logged, the instance stays up for SSM debugging; default on macOS).

## Files

| Linux | Windows | Mode | Writer |
| --- | --- | --- | --- |
| `/etc/cucina/bb/worker.json`, `runner.json` | `C:\ProgramData\cucina\bb\worker.json`, `runner.json` | 0644 | bootstrap (`internal/bbconfig`) |
| `/etc/cucina/pki/worker.crt`, `ca.crt` | `C:\ProgramData\cucina\pki\…` | 0644 | bootstrap |
| `/etc/cucina/pki/worker.key` (per boot, ADR 0571) | `C:\ProgramData\cucina\pki\worker.key` (protected DACL) | 0600 | bootstrap; removed by supervise at OS shutdown |
| `/etc/cucina/env` | `C:\ProgramData\cucina\env` | 0644 | bootstrap: `WorkerSettings.env` + `CUCINA_{POOL,NODE,STATE_ROOT,BUILD_ROOT,BUILD_DIRECTORY,L1_PLACEMENT,METRICS_PORT}` as `KEY='value'` (systemd `EnvironmentFile=` syntax) |
| `/etc/cucina/deadman.env` | `C:\ProgramData\cucina\deadman.json` | 0644 | bootstrap: the pool's dead-man limits for the images' shell/PowerShell timer |
| `/var/lib/cucina/agent.state.json` | `C:\ProgramData\cucina\agent.state.json` | 0644 | bootstrap: boot id/time, pool, node, generation, enrolled-at, certificate expiry, settings, machine facts |
| `/run/cucina/last-activity`, `last-contact` | `C:\ProgramData\cucina\run\…` | 0644 | bootstrap (boot), supervise: Unix seconds + `\n`; the mtime is the same instant |
| `/var/lib/cucina/{ephemeral,data}` | `D:` (image) or `C:\bb\{ephemeral,data}` | — | instance-store / EBS data-volume mounts (ADR 0570) |

`bbconfig` adds its own directories (state root `/var/lib/cucina` 0700, L1 and file pool below the volume, build mount
`/var/lib/cucina/build` or WinFSP `B:`, runner socket in `/run/cucina`), created by bootstrap from the render plan.

## bootstrap

1. **Same boot already done?** `agent.state.json` with this boot's id (`/proc/sys/kernel/random/boot_id`; Windows/macOS: boot
   time ± 30 s) and every output present, certificate valid ≥ 1 h → exit 0 without network calls (`bb-runner.service` restarts
   re-run `ExecStartPre`).
2. **Boot data** from the EC2 user data (IMDSv2) or `--boot-data`: `{"cucinaBootData":1,"enrollEndpoint":"host:port",
   "serverName":"…","caPem":"-----BEGIN CERTIFICATE-----…","cluster":"…","pool":"…","generation":"…"}` (≤ 4 KiB, public, ADR 0573).
   The controller builds it with `bootdata.Encode`. Launch tags `cucina:{cluster,pool,generation}` from IMDS are compared and a
   mismatch logged; the enrollment response is authoritative.
3. **Identity**: `/latest/dynamic/instance-identity/document` (raw bytes, byte-identical to the signed content) + the body of
   `/latest/dynamic/instance-identity/rsa2048` (base64 PKCS#7 RSA-2048 signature, whitespace removed) — the only form the
   controller verifies; the RSA-1024 `/signature` is never used.
4. **Key + CSR**: ECDSA P-256 generated for this boot and written (0600) before enrolling; a later attempt in the same boot
   (a bootstrap that died after enrolling) reuses it (`event=key.reused`), matching the controller's same-key crash-recovery
   window. The CSR (CN = instance ID, no extensions) is the only thing that leaves the process.
5. **EnrollWorker** over TLS 1.2+ trusting only `caPem`, verifying `serverName` (no TOFU), protocol 1.0 (`internal/proto`),
   `os`/`arch`/`agent_version`. Retry contract (ADR 0574):

   | gRPC result | Agent |
   | --- | --- |
   | `InvalidArgument`, `NotFound`, `AlreadyExists`, `PermissionDenied`, `Unauthenticated`, `FailedPrecondition`, `OutOfRange`, `Unimplemented`; server certificate not verifying | refused → power off immediately |
   | anything else (`Unavailable`, `ResourceExhausted`, `DeadlineExceeded`, `Aborted`, `Internal`, `Unknown`, network errors) | jittered exponential backoff (1 s → 15 s, ±50 %) until 2 min after start, then power off |

6. **Validate** the response: the leaf carries our public key, is valid ≥ 1 h and chains to the returned CA bundle for client
   auth; `settings.node` is empty (filled with the instance ID) or equal to it; pool consistent; endpoints and runners set.
7. **Write PKI**, **resolve L1** (ADR 0570), **render** with `internal/bbconfig` (plan notes logged as `config.note`), create the
   plan's directories, write configs, `env`, dead-man limits, timestamps, state.

Any failure logs `event=bootstrap.failed reason=<boot-data|imds|host|enrollment-refused|enrollment-unreachable|enrollment-invalid|storage|render|write>`,
powers off (EC2 terminates the instance: `InstanceInitiatedShutdownBehavior=terminate`) and exits 1 — except an instance with
**no user data at all** (EC2 Fast Launch pre-provisioning, image builds): `event=bootstrap.not_a_worker`, exit 2, no power-off.

## L1 placement (ADR 0570)

`auto` → instance-store NVMe (`Amazon EC2 NVMe Instance Storage`) → ephemeral EBS data volume (`Amazon Elastic Block Store`,
not the OS disk, no partition table) → no device (bbconfig: memory with ≥ 16 GiB RAM, else the root disk). An explicit
`instance-store`/`ebs` without such a device degrades in the same order (`event=l1.fallback`). A volume already mounted (the
images' `cucina-format-instance-store.service`, RAID 0 arrays, Windows `D:`) is reused; a blank device is formatted
(`mkfs.ext4 -O ^has_journal -E nodiscard,lazy_itable_init=1`; Windows NTFS quick, 64 KiB clusters) and mounted. RAID members
without an array, partitioned disks and the OS disk are never touched. Sizes come from bbconfig.

## supervise

Ticks every 5 s; each check runs at its own cadence:

* **Dead-man (R-POOL-7, ADR 0572)**: power off when `idle ≥ idle_limit` (default 30 min), `no scheduler contact ≥
  unreachable_limit` (10 min) or `uptime ≥ max_uptime` (12 h, monotonic OS uptime); unset settings take the defaults. Activity
  every 15 s from `bb_worker`'s `/metrics` on `127.0.0.1:<metrics_port>` (open file-pool files, completed actions; a failed
  scrape is idle). Contact every 30 s: `grpc.health.v1.Health/Check` with the worker certificate on `scheduler_endpoint`; any
  answer from the server counts. Timestamps survive agent restarts (read back from `/run/cucina`). Without a state file
  supervise waits, still powering off after 30 min of uptime (`not-bootstrapped`) or 12 h. The images' timers are the backstop
  with the same limits.
* **Spot** (when `handle_spot_interruption`): `/latest/meta-data/spot/instance-action` every 5 s; on a notice
  `systemctl stop --no-block bb-worker.service` / `sc.exe stop cucina-bb-worker` once.
* **Certificate**: exits 3 when the certificate expires within 1 h (certificates outlive `max_uptime`, so this means the
  controller issues too-short certificates).
* **Metrics** (`--metrics-listen`, default `127.0.0.1:9982`): `cucina_agent_uptime_seconds`, `cucina_agent_idle_seconds`,
  `cucina_agent_scheduler_reachable`.
* On SIGTERM/CTRL_C it exits 0; during an OS shutdown it removes the worker key first.

## render and the machine document

`--settings` is `WorkerSettings` protojson (strict). `--machine` is JSON mirroring `bbconfig.Machine` field by field (strict;
`TestMachineMirrorsRendererInput` keeps them in sync):

```json
{"os": "darwin", "arch": "arm64", "vcpus": 6, "memoryBytes": 17179869184,
 "stateRoot": "/var/db/cucina", "stateRootBytes": 214748364800, "buildRoot": "/Volumes/CucinaBuild",
 "runDir": "/var/run/cucina-runner", "pkiDir": "/var/db/cucina/pki", "caBundlePem": "-----BEGIN CERTIFICATE-----…",
 "storageIsHostL2": true, "storageServerName": "", "metricsHost": "", "buildUser": {"uid": 501, "gid": 20},
 "instanceStorePath": "", "instanceStoreBytes": 0, "dataVolumePath": "", "dataVolumeBytes": 0,
 "emulatorLdPrefixes": {}, "xcodeDeveloperDirectories": {"27.0": "/Applications/Xcode-27.0.app/Contents/Developer"},
 "nativeBuildDirectoryReason": "", "nativeCacheBytes": 0}
```

Shell callers may pass `"caBundleFile": "/path/ca.crt"` instead of an inline `caBundlePem`.
Output: `DIR/worker.json`, `DIR/runner.json`, `DIR/env` (0644) and `{"directories":[{"Path","Mode","BuildUserOwned"}],"notes":[…]}`
on stdout — create those directories before starting `bb_runner`/`bb_worker`.

## Logs

One JSON object per line (`log/slog`), `component=cucina-worker-agent`, `cmd`, `version`, and a stable `event`:
`bootstrap.{start,idempotent,repeat,done,failed}`, `bootdata.loaded`, `identity.loaded`, `tags.{unavailable,mismatch}`,
`imds.*.retry`, `enroll.retry`, `enroll.ok`, `l1.{placed,fallback}`, `config.{note,rendered}`, `supervise.{wait,start}`,
`activity.{unavailable,unobservable}`, `scheduler.{reachable,unreachable}`, `spot.{notice,drained,drain_failed}`,
`deadman.poweroff`, `cert.expiring`, `key.wiped`, `poweroff.{suppressed,failed}`. Key material, CSRs, signatures, identity
documents and `settings.env` values are never logged. Read on a worker: `journalctl -u bb-runner -u cucina-worker-agent -o cat`
(SSM), Windows `C:\ProgramData\cucina\logs\agent.log` and `C:\bb\log\boot.log`.

## Tests

```sh
go test ./internal/workeragent/... ./cmd/cucina-worker-agent/...
for t in linux/amd64 linux/arm64 windows/amd64 darwin/arm64; do
  GOOS=${t%/*} GOARCH=${t#*/} CGO_ENABLED=0 go build -o /dev/null ./cmd/cucina-worker-agent; done
bazel test //internal/workeragent/... //cmd/cucina-worker-agent/...   # + cross_compile_test (tier-static)
```

| Target | Tier | Covers |
| --- | --- | --- |
| `//internal/workeragent:workeragent_test` | unit | dead-man decision (table + property), supervisor with fake clock/FS (controller gone, idle, busy, restarts, Spot, certificate), L1 placement, sysfs/Get-Disk discovery, Linux volume reuse/format, paths (Linux + Windows), activity parsing, env rendering, machine-document parity |
| `//internal/workeragent/bootdata:bootdata_test` | unit | codec round trip (property), size limit, no private keys, versions |
| `//internal/workeragent/imds:imds_test` | integration | IMDSv2 token flow, user data, identity, tags, Spot notice against `imdsfake` |
| `//internal/workeragent/integration:integration_test` | integration | bootstrap end to end over TLS: success, transient then success, denied, identity rejected, unreachable for the deadline, rogue server certificate, foreign key, missing boot data; file modes, no key in logs, idempotent second run |
| `//internal/workeragent/hostos:hostos_test` | integration | `porttest.RunFS`/`RunClock` conformance of the real adapters |
| `//cmd/cucina-worker-agent:cucina-worker-agent_test` | integration | `render` CLI contract |
