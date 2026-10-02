<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0403 — TLS: per-component server certificates in a public and an internal group

* Status: accepted (2026-10-02)

## Context
R-CP-5 offers three TLS sources (chart-generated private CA, cert-manager, existing Secret). ACME can only certify public
names, while workers reach private addresses and verify them with Cucina's CA. `pki.CertSpec` (ADR 0552) issues one
certificate per component (`spiffe://cucina/server/<component>`), and `config.TLS` gives the controller a single
certificate for all its listeners.

## Decision
Five server certificates, two groups: public (`frontend` client listener, `sts`) and internal (`frontend-workers`,
`scheduler`, `controller`). Each group picks its source; chart-generated certificates are listed in `certs.json` for
`cucina-controller bootstrap/controller --certs` (issued by Cucina's CA, renewed by the leader), cert-manager ones are
`Certificate` objects with the same Secret names and SANs, an existing Secret replaces the whole group. The controller
serves the internal certificate (it carries the management and STS names too); the public STS endpoint is the STS
Deployment with the public certificate. The controller's BuildQueueState client certificate is always Cucina-issued.

## Consequences
With an ACME public group, `cucinactl` needs Cucina's CA bundle for the management API (served by the controller) but not
for the STS or remote execution.
