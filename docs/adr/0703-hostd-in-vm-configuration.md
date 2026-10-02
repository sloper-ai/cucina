<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0703 — hostd configures each VM at every boot and starts its Buildbarn jobs; dead-man from outside

* Status: accepted (2026-10-02)

## Context
R-MAC-4: nothing secret is baked into the image; hostd injects config and short-lived credentials at boot via the Tart
Guest Agent or SSH; `bb_worker` is a LaunchDaemon, `bb_runner` a LaunchAgent in the auto-logged-in session. R-POOL-7:
hostd enforces the dead-man limits for its VMs even when the controller is gone. The pinned `bb_worker` has no
"actions in progress" gauge (ADR 0572).

## Decision
* The image ships the launchd plists outside the auto-load directories; at every start hostd renders the configs on the
  host (`internal/bbconfig`), pushes a tar stream over `tart exec -i … sudo -n tar -x` **stdin** (never argv), creates the
  planned directories and bootstraps the runner (`gui/<uid>`) and worker (`system`) jobs (docs/dev/hostd.md §1.2).
  A VM that reboots on its own runs no Buildbarn job until hostd reconfigures it with fresh credentials.
* `bb_worker` runs as `image.json` `workerUser` (default: the build user; `root` when NFSv4 build directories need mounts).
* The dead-man switch runs in hostd: uptime 12 h, idle 30 min (activity = a change of `bb_worker`'s build-executor
  counter scraped by hostd, and before an idle stop a `tart exec` probe for children of `bb_runner`, so one long action is
  not cut), scheduler unreachable 10 min (relay dial outcomes and live connections). A dead-man stop sets the VM's intent
  to stopped until the controller asks again.

## Consequences
* No in-VM Cucina agent is needed; the image only carries Buildbarn and the plists.
* Start latency includes one `tart exec` round trip per step (≈ 6 calls); measured in T13 against NFR-P1.
* The busy probe needs `pgrep` in the guest (base macOS).
