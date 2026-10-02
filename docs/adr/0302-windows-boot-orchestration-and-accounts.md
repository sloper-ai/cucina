<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0302 — Windows worker boot orchestration and service accounts

* Status: accepted (2026-10-02)

## Context
On Linux, systemd orders `cucina-format-instance-store` -> `bb-runner` (ExecStartPre = bootstrap hook, fail closed)
-> `bb-worker`. Windows services have no ExecStartPre, and a service that depends on a one-shot "service" fails to
start once the one-shot exits. R-SEC-5 asks for a low-privilege account for actions "where feasible".
EC2 Fast Launch boots every pre-provisioned snapshot once on a prep instance, so whatever runs at boot also runs there.

## Decision
* Buildbarn and agent services are **demand-start**. An automatic-start one-shot service `cucina-boot` (shawl,
  `--no-restart`, LocalSystem) runs `C:\bb\bin\cucina-boot.ps1` once per boot (a Task Scheduler "at startup" task
  was measured to fire only ~60 s after boot on WS2025, the SCM starts the service within seconds): clear per-boot state (`C:\ProgramData\cucina\run`, runner socket dir) -> format a raw
  NVMe instance-store disk (non-fatal) -> bootstrap hook `cucina-bootstrap.ps1` (no-op without
  `cucina-worker-agent.exe`, otherwise `cucina-worker-agent bootstrap`; exit 2 = no EC2 user data, "not a worker":
  nothing starts and the instance stays up; any other non-zero = services are **not** started) ->
  `cucina-worker-agent` (run-supervisor) when installed -> `cucina-bb-runner` -> `cucina-bb-worker`, the last two only
  when `C:\ProgramData\cucina\bb\{runner,worker}.json` exist, with `C:\ProgramData\cucina\env` as their environment. Logs: `C:\ProgramData\cucina\logs\boot.log`; service
  output in `C:\ProgramData\cucina\logs\<unit>.log` (`bb-worker`, `bb-runner`, `agent`; shawl rotates at 50 MB).
* `bb_runner` (and therefore every action) runs as the **virtual account `NT SERVICE\cucina-bb-runner`**: no
  password, SID derived from the service name (stable across sysprep), member of Users only, plus
  `SeCreateSymbolicLinkPrivilege`; write access only to `C:\bb\tmp`, `C:\bb\run`, `C:\ProgramData\cucina\logs\bb-runner`.
* `bb_worker` runs as **LocalSystem**: it creates the WinFSP Mount Manager mount `\\.\B:` and owns L1/filePool
  (`C:\bb\cache`, `C:\bb\filepool`, SYSTEM/Administrators only). bb_worker's WinFSP file system grants the build
  directory to Everyone with owner = SYSTEM, so the runner account can use it but cannot change its ACLs.
* The dead-man switch is a scheduled task armed from boot (never during image builds).

## Consequences
Residual risk (documented in docs/operations/images.md): `bb_worker` is privileged, so a compromise of bb_worker
itself (not of an action) is a full machine compromise; all actions of one worker share one account (no per-action
isolation, same as Linux `run_commands_as`); actions can read world-readable machine files. Pools remain the trust
boundary ("one pool per trust level"). The runner socket lives in the agent's run directory
`C:\ProgramData\cucina\run`, so the runner account can also touch the dead-man marks there (keep its own worker alive
up to the 12 h limit).
