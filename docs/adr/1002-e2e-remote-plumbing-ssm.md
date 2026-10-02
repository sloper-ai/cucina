<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 1002 — e2e remote plumbing: SSM Run Command scripts, background jobs, port-forward transfers

* Status: accepted (2026-10-02)

## Context
The campaign drives Bazel on `linux-client` and `windows-client` without inbound ports (§12) and moves multi-megabyte artifacts
(BEP, execution logs, profiles, `cucinactl` binaries) without S3. SSM Run Command truncates output at 24,000 characters,
kills the process tree of Windows commands when they end, and records command parameters in the account's command history.
Builds run for hours, longer than a sensible single Run Command.

## Decision
* One `Host` interface for every machine (`test/e2e/remote`): synchronous runs with complete output, detached background jobs
  with status polling, and SHA-256-verified file transfer. The operations are small POSIX-sh and Windows PowerShell 5.1 scripts;
  a transport executes them (`/bin/sh -c` on the dev Mac, `AWS-RunShellScript`/`AWS-RunPowerShellScript` via the AWS SDK).
* A run writes the user's output to files on the host and returns only a status line plus base64 heads (12 KB + 4 KB); larger
  output is read back in 16 KiB chunks, so truncation never loses data.
* Background jobs detach with `systemd-run` (Linux, own unit/cgroup), `nohup` (macOS) or `Win32_Process.Create` via CIM
  (Windows, leaves the SSM job object). They write `pid` and `exit` atomically; status samples liveness first, then the exit
  file, then output sizes (two races found by the integration test are fixed this way).
* Small files go inline (24 KiB chunks); files above 256 KiB go through an SSM port-forwarding session
  (`aws ssm start-session --document-name AWS-StartPortForwardingSession`) to a one-shot loopback HTTP receiver on the VM
  (python3 / `HttpListener`) with a random path token. **Secrets** (service keys) always take the port-forward path
  (`PutPrivate`), are restricted to their owner, and are deleted after `cucinactl login --key "$(cat …)"`.
* Bazel runs as a non-root user on Linux (`sudo -u ubuntu -H`) under `env -i` with a minimal environment.
* Acceptance scenarios poll real infrastructure with real time (`remote.RealSleep`), the one place where waiting cannot use a
  fake clock; unit and integration tests inject a `Sleeper` and never sleep.

## Consequences
* The POSIX scripts are exercised by the integration tier on the dev Mac (BSD tools); the PowerShell variants are first
  exercised in the campaign (no Windows host offline).
* The dev Mac needs the Session Manager plugin (present via mise) for transfers above 256 KiB.
