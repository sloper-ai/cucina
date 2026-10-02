<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0653 — One private CA per installation, rotated with a two-root bundle in three phases

* Status: accepted (2026-10-02)

## Context
§13 makes a Cucina-generated private CA the production default (cert-manager and existing Secrets stay possible).
The CA signs workload certificates (workers, VMs, hosts, controller) and, by default, the in-cluster server
certificates. Buildbarn takes client CAs as inline PEM read at start-up; workers trust what enrollment gave them at
boot; hosts trust their MDM profile until their first renewal. A rotation must never leave a verifier without the root
that signed what it is shown.

## Decision
* One ECDSA P-256 root, 10 years, `pathLen 0`, in the Secret `config.pki.caSecret` (`ca.crt` bundle, `ca.key` active
  key). The active signer is the bundle certificate matching `ca.key`, so the bundle may hold two roots in any order.
* Rotation phases `introduce` (add CA2 to the bundle, keep signing with CA1), `activate` (sign with CA2; the leader-only
  Rotator re-issues managed server/controller certificates) and `retire` (drop CA1 after the longest leaf lifetime),
  exposed as `pki.RotateCA`; the documented waits come from Buildbarn restarts, the 12 h worker uptime, the ~4.7-day
  host renewal period and MDM rollout.
* Managed server/controller certificates: 90 days, renewed at 2/3 of their lifetime; `tls.crt` carries the issuing CA
  so clients can verify by SPKI pin before they hold the bundle.

## Consequences
* A single CA compromise exposes every identity; the CA Secret's RBAC is the main control, and recovery is a new CA
  plus fleet re-enrollment.
* Operators who source server certificates elsewhere keep the workload CA (Cucina must sign workload leaves itself).
* MDM profiles must carry both CA certificates or pins during a rotation.
