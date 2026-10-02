<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0700 — Mac VMs reach the control plane only through hostd: L4 relay + per-VM certificates

* Status: accepted (2026-10-02)

## Context
R-MAC-4: VMs talk only to their host — hostd's L2 for CAS/AC and a hostd-provided path to the scheduler — "either a proxy
or per-VM short-lived certificates (your choice)". R-MAC-10: Local Network privacy auto-allows the root daemon but not the
privilege-dropped `tart` child, so connections to VM addresses must be made by hostd. The vmnet bridge (`bridge100`,
192.168.64.1 by default) exists only while a VM runs, so a listener cannot be bound to it at hostd start. The L2
`bb_storage` renderer (`internal/bbconfig.RenderHostL2`) serves TLS, verifies `spiffe://cucina/worker/…` client
certificates and refuses wildcard listen addresses.

## Decision
Both: an **L4 relay in hostd** and **per-VM short-lived certificates**.
* hostd listens on `:8981` (→ L2 `bb_storage` on `127.0.0.1:8991`) and `:8983` (→ the controller's scheduler worker
  endpoint from `HostSettings.scheduler_endpoint`). A connection is admitted only if its source address belongs to a VM
  hostd started and it arrived on the interface whose network contains that address (not the LAN); at most 64
  connections per VM. Bytes are copied unchanged, so TLS is end-to-end.
* Each VM gets its own ≤ 12 h certificate (`IssueVMIdentity`, URI SAN `spiffe://cucina/worker/<pool>/<serial>/<vm>`),
  generated on the host, pushed over `tart exec` stdin. The scheduler authenticates the VM itself; the controller only
  issues identities for VMs it asked this host to run.
* The L2's VM-facing certificate comes from a **host-local CA** (key in the System keychain, item `host-l2-ca-key`,
  name `cucina-host-l2`); hostd appends that CA to the VM's `ca.crt`. The L2 verifies the VM certificates against the
  Cucina CA and only admits this host's serial.
* The L2's upstream uses the host identity; `bb_storage` needs it as files, so hostd writes the host certificate and key
  to `/var/db/cucina/hostd/l2/` (root, 0700/0600) at every start.

## Consequences
* VMs never need a route beyond the host; the dead-man "scheduler unreachable" input comes from the relay (dial outcomes
  and live connections).
* The frontend's worker listener must let **host** identities write the AC/CAS: the L2 forwards the VMs' writes with
  the host certificate (chart/auth: authorize `spiffe://cucina/host/*` for worker-scope writes).
* A copy of the host key exists on disk for `bb_storage` (root-only); the keychain item stays the source of truth.
* With the application firewall on, `cucina-hostd` must be an allowed app (pkg postinstall); `bb_storage` binds loopback only.
