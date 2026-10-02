<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0580 — Management API: authorization enforced by the service, three access classes

* Status: accepted (2026-10-02)

## Context
R-AUTH-11/R-SEC-4 require a Cucina JWT with `admin` for mutating management calls; the proto comment lets any JWT with
`execute` or `admin` call read-only methods. Server-level interceptors are configured by whoever builds the `grpc.Server`
(the controller wiring), so a wiring mistake would expose the API, and a new RPC could ship without a rule. Some "read-only"
RPCs expose sensitive data: worker logs (action output of any tenant), host diagnostics, support bundles, and the
metadata of service keys, enrollment tokens and revocations.

## Decision
* `(*mgmt.Server).Register` installs a copy of the generated service descriptor whose every handler authenticates the bearer
  token (`mgmt.Authenticator`, backed by `keys.Verifier`) and applies the method→access table **before** the implementation
  runs, whatever interceptors the server has. A method without a rule is denied; `mgmt.New` refuses a table that does not
  cover the descriptor exactly (a test checks the same).
* Three classes: `Read` (any JWT with `execute` or `admin` on ≥ 1 instance name), `AdminRead` (sensitive reads: logs,
  diagnostics, support bundle, key/token/revocation listings) and `Mutate`. `AdminRead` and `Mutate` need **cluster admin**:
  `admin` on every configured instance name (the STS expands `"*"`); a tenant admin can read but not change shared fleet state.
* Operations are only visible on instance names the caller holds `execute`/`admin` on (`GetOperation` answers NOT_FOUND
  otherwise).
* Every call to an `AdminRead` or `Mutate` method writes one audit record, including denied and unauthenticated attempts
  (`mutating` distinguishes the classes); plain reads are not audited. Streams are recorded when they end.

## Consequences
Stricter than the proto comment for the six sensitive reads (an execute-only developer cannot tail logs or collect a bundle;
they use `cucinactl action inspect`, which reads CAS/AC with their own token). The optional `auth.Interceptor` on the same server
is redundant but harmless (both must pass). Adding an instance name invalidates existing admins' cluster-admin status until their
next token renewal (≤ 15 min).
