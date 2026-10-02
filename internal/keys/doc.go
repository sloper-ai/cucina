// SPDX-License-Identifier: FSL-1.1-ALv2

// Package keys owns Cucina's token key material and revocation state (R-AUTH-3,
// R-AUTH-9, R-AUTH-10, R-AUTH-12):
//
//   - the Cucina JWT claim contract (docs/contracts.md §5.1), minting with ES256 and
//     a `kid`, and local verification (management API, tests);
//   - the signing key set in a Kubernetes Secret, the JWKS ConfigMap the controller
//     writes directly, and the rotation state machine (publish K2 → wait ≥ lead and
//     verify the frontends loaded it → sign with K2 → keep K1 ≥ max TTL + skew →
//     remove), plus the compromised-key path;
//   - the deny-list ConfigMap read by every Buildbarn JMESPath authorizer
//     (docs/security.md §Deny-list);
//   - opaque service-account keys (`cuc_sk_<id>_<secret>`), stored as HMAC-SHA-256
//     with a server pepper (ADR 0601), and the Helm-install break-glass key;
//   - idempotent bootstrap entry points for the Helm pre-install hook.
//
// The package never logs or returns token or key material in errors.
package keys
