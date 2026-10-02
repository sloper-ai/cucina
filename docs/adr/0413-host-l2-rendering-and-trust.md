<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0413 — Host L2: rendering, listener trust and compression

* Status: accepted (2026-10-02)

## Context
Each Mac host runs a `bb_storage` L2 that its VMs use as their slow backend (R-CACHE-4). It runs the pinned
bb_storage (NEW nested `keyLocationMap` schema), whose Go types cannot be linked next to the OLD ones that type-check
worker configurations (ADR 0001). VMs talk to it over the host-only vmnet bridge; it talks to the central worker
endpoint over the WAN.

## Decision
* `RenderHostL2(HostL2Settings)` executes a `text/template` with every string JSON-encoded. Unit tests rewrite the
  one `local` block to the old schema and parse the whole document strictly with the old Go types (identical
  outside `local`); the boot test runs the pinned bb_storage on it.
* CAS: `readCaching{slow: grpc(central, zstd), fast: local persistent on the host SSD (200 GiB default, block tiers
  of ADR 0411), replicator: deduplicating{concurrencyLimiting{local, 64}}}`. Writes, FindMissing, AC and FSAC pass
  through to the central endpoint with the host identity (docs/security.md admits hosts on the worker listener).
* Upstream hop: zstd `encoderLevel: 3` with a bounded pool, keepalive 60 s/20 s with `permitWithoutStream` for NAT
  paths. The frontend's worker listener must allow it (`keepaliveEnforcementPolicy.minTime` ≤ 60 s,
  `permitWithoutStream: true`, as R-CP-5 already sets for clients).
* VM listener: only the bridge address (never a wildcard — refused at render time), TLS with a server key pair
  hostd provides, `tlsClientCertificate` admitting `spiffe://cucina/worker/…/<serial>/…` of this host only.
  Authorizers are `allow` behind that authentication.
* Workers rendered with `Machine.StorageIsHostL2` never compress the VM→L2 hop, whatever
  `WorkerSettings.wan_compression` says; `Machine.StorageServerName` names the L2's certificate.

## Consequences
* hostd must issue/obtain the L2 server certificate (Cucina CA, SAN = bridge IP/name) and pass its name to VMs via
  `Machine.StorageServerName`; the enrollment API has no such profile yet (reported to the lead).
* A VM certificate revoked through the deny-list keeps L2 access until it expires (≤ 12 h): the L2 has no deny-list.
* Outputs written by one VM and read by another on the same host cross the WAN twice (up free, down once per host).
