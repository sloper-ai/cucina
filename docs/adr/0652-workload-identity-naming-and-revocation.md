<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0652 — Workload identities are SPIFFE-style URI SANs; certificates are revoked by deny-list, not CRL

* Status: accepted (2026-10-02)

## Context
R-AUTH-4 asks for SPIFFE-style URI validation in Buildbarn's `tlsClientCertificate`. Buildbarn exposes only
`dnsNames`, `emailAddresses` and `uris` to standard JMESPath (go-jmespath v0.4.0: no `split`, no regex), has no CRL or
OCSP support, and its authorizers are shared by all listeners of a process, so certificate holders and JWT clients
meet the same authorizer expressions. The VM identity needs a host segment, and the scheduler's `node` label must
name the same VM.

## Decision
* Identities: `spiffe://cucina/worker/<pool>/<instance-id>`, `spiffe://cucina/worker/<pool>/<serial>/<vm>`,
  `spiffe://cucina/host/<serial>`, `spiffe://cucina/controller`, `spiffe://cucina/server/<component>`; fixed trust
  domain `cucina` (one CA per installation), segments restricted to `[A-Za-z0-9._-]` so Buildbarn's `url.URL.String()`
  view is byte-identical; `<serial>` is the canonical upper-case serial from the caller's host certificate; the VM
  `node` label is `<serial>/<vm>`.
* Buildbarn rules match prefixes (`starts_with`), and certificate metadata reuses the JWT's `private` keys (explicit
  per-verb instance-name lists) plus `sub` = URI SAN, so the same authorizers and the same deny-list serve both.
* No CRL/OCSP: short lifetimes, no renewal for removed hosts, and urgent revocation through the deny-list `sub`
  entries (≤ 2–3 min, docs/security.md §Deny-list).

## Consequences
* Buildbarn cannot bind a worker certificate to the platform it registers for; the threat model documents it and the
  chart can bind pools to instance names.
* The host L2 (ADR 0413) has no deny-list: a revoked VM keeps L2 access until its ≤ 12 h certificate expires.
* Renaming the trust domain or a role segment is a breaking change for the chart's rendered expressions.
