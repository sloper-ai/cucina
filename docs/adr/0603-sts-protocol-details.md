<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0603 — STS protocol details: subjects, sessions, errors, service keys, break-glass

* Status: accepted (2026-10-02)

## Context
Contracts §5.1 fixes the token-exchange shape and the JWT claims but leaves details open that clients, the chart and the
deny-list depend on.

## Decision
* **Subjects** are `<scheme>:<id>` (scheme `[a-z][a-z0-9-]{0,31}`); the STS percent-encodes every byte outside
  `[A-Za-z0-9._~:@/+=-]` (and `%`) of the mapped value, injectively, and rejects results over 512 bytes. A scheme keeps
  subjects of different issuers apart.
* **`sid`** is random per OIDC exchange (revoking it kills one token) and `SessionForKey(id)` for service keys (revoking a
  key deny-lists all its outstanding tokens).
* **Errors**: `invalid_request` and `unsupported_grant_type` 400, `invalid_grant` 400 (the token itself is unacceptable),
  `access_denied` 403 (valid token, policy says no), `slow_down` 429 with `Retry-After` (rate limit), `server_error` 500
  (including unreachable IdP keys and failed group lookups: fail closed). Parameters only in the body, each once.
* **Service keys** use `subject_token_type=urn:cucina:params:oauth:token-type:service-key` (the access-token URI is
  accepted too) and are recognised by their `cuc_sk_` prefix.
* **Break-glass** has a built-in policy (every verb, every instance name) so the STS works with zero TrustPolicies; a valid
  `serviceAccount.breakGlass` TrustPolicy replaces it. The reserved account `break-glass` cannot get regular keys.
* **Policy union**: all matching policies of an issuer apply; they must map the same subject (else `access_denied`); the
  TTL is the smallest applied `maxTTL`; a token without any applicable grant is rejected.
* **jti replay cache** is per replica and fails closed when full (the STS is stateless by design).
* **Management API** interceptor: `execute`/`admin` anywhere for reads, `admin` on every instance name for mutations and
  the sensitive reads of ADR 0580 (`auth.Requirement` instead of a verb per method, since reads accept two verbs).

## Consequences
The CLI must send the service-key token type and treat 403/429 distinctly; operators see percent-encoded subjects for
unusual workflow names; documented in docs/security.md.
