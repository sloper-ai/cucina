<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0651 — Mac host enrollment binds the first key; the bound key may recover; tokens are SHA-256 hashed

* Status: accepted (2026-10-02)

## Context
R-SEC-3: one multi-use site token per MDM profile; each host exchanges it once (token + serial + CSR); only
pre-registered or approved serials are admitted; re-enrolling an existing serial needs an admin; token revocation
leaves enrolled hosts alone; the token is stored hashed. Open points: what "same request" means while a host polls in
`PENDING`, what happens when a host's certificate expired during a long outage (UC21: unattended return to service),
how to hash tokens, and what the maximum host count counts.

## Decision
* The **first key** a serial presents is bound to it (SHA-256 of the SubjectPublicKeyInfo, so re-signed CSRs for the
  same key are the same request). Polling with that key is idempotent; another key is `DENIED` until an admin removes
  the host. Approval therefore approves the key that was pending.
* An enrolled serial presenting its **bound key** with a valid token gets a fresh certificate (audited `recovered`):
  this proves possession of the same key as `RenewCertificate` would, and lets a host whose 7-day certificate expired
  during an outage return without an admin. A different key is a re-enrollment: `DENIED`.
* Tokens are `cuc_et_<id>_<secret>` with a 256-bit random secret; only SHA-256 of the secret is stored and compared
  in constant time. A slow password hash (argon2id) buys nothing for 256-bit random secrets.
* Every serial that first contacted Cucina with a token counts towards its host count (pending or enrolled), so a
  leaked token can create at most that many Pending MacHosts; removing a host frees its slot.

## Consequences
* hostd must keep its key in the System keychain across restarts (otherwise a restart while pending is `DENIED`).
* Anyone holding the bound private key can obtain certificates while the MacHost exists — the same exposure as
  `RenewCertificate`; `cucinactl hosts remove` ends both.
* If hostd rotates its key at renewal, the renewal must update the bound key (`enroll.Server.RenewCertificate` does;
  hostlink's own renewal path should call it or update the annotation).
