<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0602 — Go STS tests use an in-process OIDC issuer; mock-oauth2-server stays for CLI and e2e

* Status: accepted (2026-10-02)

## Context
R-TEST-6 names `navikt/mock-oauth2-server` 6.0.4 for the STS integration tests. It is a JVM program: Maven Central has
only the thin jar (dependencies would have to be resolved by Maven/Gradle, neither is installed), GitHub releases have no
assets, and the container image would need Docker, which the integration tier must not use (R-TEST-2: Docker only in the
`system` lane). The STS table also needs tokens a conforming IdP never issues: `alg: none`, tampered signatures, unknown
`kid`, `nbf`/`iat` in the future, arbitrary GitHub-shaped claims.

## Decision
`internal/auth/oidctest` is a minimal in-process issuer built on go-jose: several issuers on one HTTPS test server, one
per path (`/google`, `/github`, like mock-oauth2-server), discovery and JWKS, RS256 keys generated at test time (never
written to disk), key rotation, arbitrary claims, deliberately broken tokens, and an offline kid-aware key set for the
unit tier. Google- and GitHub-shaped claim builders provide valid defaults (R-TEST-8e).

## Consequences
* Go tests are hermetic and run on RBE (no JVM, no Docker, localhost only).
* Interoperability with mock-oauth2-server itself is exercised by the CLI login tests and the e2e campaign (T10), which
  run the real server; an ephemeral cross-check of the STS against the 6.0.4 image is recorded in the auth report.
