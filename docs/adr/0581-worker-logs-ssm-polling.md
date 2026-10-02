<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0581 — Worker logs: SSM Run Command polling for EC2, host diagnostics for Tart

* Status: accepted (2026-10-02)

## Context
`cucinactl workers logs` (R-OBS-4, R-CLI-3) must reach EC2 workers that have no inbound ports and no credentials, and Tart VMs
behind a Mac host's NAT. SSM Session Manager streaming needs the session-manager plugin on the client and IAM for every user;
CloudWatch agent shipping adds standing cost and per-worker configuration. SSM `GetCommandInvocation` returns at most 24,000
characters of output inline.

## Decision
* EC2: the controller runs a short read-only script through `ssm:SendCommand` (`AWS-RunShellScript` / `AWS-RunPowerShellScript`)
  and reads it with `ssm:GetCommandInvocation` (adapter `mgmt.InstanceShell`). Linux tails journald
  (`journalctl -u <unit> --show-cursor`, then `--after-cursor`); Windows reads the WinSW log file from a byte offset. Each poll
  returns ≤ 22 KB; `follow` polls every 5 s and ends after 30 min. One token bucket (1 call/s, burst 5) is shared by all streams,
  and at most 8 log/diagnostics streams run at once. The only script inputs are an enum unit, an integer and a cursor matching
  `^[A-Za-z0-9=;_-]+$`. Output is redacted before it is sent.
* Tart: the host's diagnostics stream (`HostAdmin.Diagnostics` with VM, unit and tail) returns a snapshot; `follow` is not
  available until `CollectDiagnostics` carries `vm_name`/`unit`/`tail_lines` (requested from the lead as an additive change).
* Unit locations are a contract with the worker images (docs/dev/mgmt.md "Worker log locations").

## Consequences
No client-side plugin, no standing cost, works from any `cucinactl`. Linux follow can drop lines if a unit logs more than 22 KB
within one poll (marked in the stream); Windows follow is lossless. Each poll is one SSM command (free for standard commands,
visible in CloudTrail).
