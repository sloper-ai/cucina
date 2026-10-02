<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0571 — The per-boot worker key lives next to its certificate in /etc/cucina/pki

* Status: accepted (2026-10-02)

## Context
The worker-agent task placed the per-boot ECDSA key "in memory/in a 0600 file under `/var/lib/cucina` or
`C:\ProgramData\cucina`, wiped at shutdown", and the certificate plus CA bundle under `/etc/cucina/pki/`. `bb_worker` runs as a
separate process, so the key must be a file. `internal/bbconfig` renders `bb_worker`'s mTLS client configuration from one key-pair
directory (`Machine.PKIDir` with `worker.crt` + `worker.key`, re-read every refresh interval), the same layout hostd writes into
Tart VMs.

## Decision
Write `worker.key` (PKCS#8 PEM) into the PKI directory next to `worker.crt` and `ca.crt`: `/etc/cucina/pki/` (Linux),
`C:\ProgramData\cucina\pki\` (Windows). The key is generated at every boot (never reused across boots; a second bootstrap
attempt in the same boot reuses it, matching the controller's same-key crash-recovery window), written
0600 (Linux) or with a protected DACL granting only SYSTEM, Administrators and optional `--key-reader` accounts (Windows, where
`C:\ProgramData` would otherwise let Users read it), never logged, and removed by `supervise` when it stops during an OS shutdown
(`systemctl is-system-running` = `stopping`; Windows `SM_SHUTTINGDOWN`). A plain restart of the agent keeps it, because `bb_worker`
re-reads it.

## Consequences
* One key-pair directory for bbconfig, the agent and hostd; no copy of the certificate in two places.
* The key's directory is not 0700 on Linux (the CA bundle in it is public); the file mode and the root-only `bb_worker` protect it.
* The root volume holding the key is deleted with the instance (EC2 workers never stop), so persistence beyond a boot is bounded
  by the instance lifetime even if the shutdown wipe does not run.
