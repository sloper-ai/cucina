<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0552 — Server certificates come from a `--certs` list; fixed names for controller-owned Secrets

* Status: accepted (2026-10-02)

## Context
The bootstrap hook must create the in-cluster server certificates (frontend, storage, scheduler, controller, STS) and
the leader must renew them before expiry (`pki.EnsureServerCerts`, `pki.Rotator`, agent enroll). Their SAN lists are
chart knowledge (Service DNS names, public hostnames). `internal/config.Controller` (lead-owned) has no field for them,
and `pki.CertSpec` cannot live in `internal/config` (pki imports config). The site enrollment tokens also need a Secret
whose name the configuration does not carry.

## Decision
`cucina-controller bootstrap --certs FILE` and `cucina-controller controller --certs FILE` read a JSON array of
`pki.CertSpec` (strict parsing). Without `--certs`, bootstrap only creates the CA (cert-manager or existing Secrets
supply the server certificates) and no rotator runs. Enrollment tokens live in the Secret `<releaseName>-enroll-tokens`.
The chart renders the certificate list next to `controller.json` and grants the Secret access (docs/dev/controller.md).

## Consequences
No contract change was needed to unblock the chart. If the lead prefers the list inside `controller.json`, add
`pki.serverCerts` to `config.PKI` with its own type and drop the flag; the rest of the wiring stays.
