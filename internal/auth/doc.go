// SPDX-License-Identifier: FSL-1.1-ALv2

// Package auth is Cucina's trust-policy engine (R-AUTH-2) and the management API's
// authentication helper (R-AUTH-11).
//
// An external token (OIDC ID token, GitHub Actions JWT) is verified by a
// ports.IdentityProvider (OIDCVerifier: go-oidc discovery + JWKS caching, exact iss,
// aud, exp, signature, asymmetric algorithms only), then every TrustPolicy of its issuer
// is evaluated: all claimValidationRules must be true, claimMappings produce the
// subject, display name and groups, and the union of the grants whose condition holds
// becomes the Principal. A token matching no policy is rejected. All CEL is compiled at
// load time, must have the right result type, runs under a cost limit, interrupt checks
// and a per-evaluation timeout, and every error fails closed.
//
// Service-account keys (internal/keys) are evaluated against TrustPolicies of type
// serviceAccount; the break-glass key has a built-in all-verbs policy.
package auth
