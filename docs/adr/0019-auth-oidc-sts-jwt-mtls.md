<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0019 — D9: OIDC to STS to short-lived JWT for clients; mTLS for workers and hosts

* Status: accepted (2026-10-02)

## Context
Laptops and CI must not hold long-lived secrets; builds must keep working while the control plane restarts;
offboarding someone at the identity provider has to take effect quickly; and Mac hosts sit behind NAT and
receive one identical MDM profile each, so a per-host secret cannot be pre-provisioned.

## Decision
* **Clients**: an OIDC identity (Google Workspace via the Desktop-app loopback flow with PKCE, GitHub Actions,
  any generic OIDC provider) is exchanged at the Cucina STS (RFC 8693) for a 15-minute JWT that Buildbarn validates locally
  against a JWKS file. Authorisation uses claims inside the JWT, per verb and instance name.
* **Workers and hosts**: short-lived mTLS certificates from Cucina's private CA. EC2 workers prove identity with the
  AWS-signed instance identity document; Mac hosts exchange a multi-use, expiring site token once, with admission by
  pre-registered or approved serial number.

## Consequences
* Offboarding takes effect within one token TTL; urgent revocation goes through a deny-list read by every authorizer,
  effective in about three minutes (`docs/operations/revocation.md`).
* Buildbarn caches JWT validation results, so a compromised signing key needs a frontend restart, not just a JWKS change.
* No secret is baked into an AMI, a VM image or the pkg.
