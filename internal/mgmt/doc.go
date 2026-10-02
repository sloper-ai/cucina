// SPDX-License-Identifier: FSL-1.1-ALv2

// Package mgmt implements the controller's ManagementService, the single gRPC
// endpoint used by cucinactl (R-CLI-2, UC13–UC17), and its audit log.
//
// Security is enforced by construction (R-AUTH-11, R-SEC-4):
//
//   - Register installs a wrapped copy of the generated service descriptor in
//     which every handler first authenticates the caller's Cucina JWT (through the
//     Authenticator the auth package provides) and checks the method→access table
//     (MethodAccess). A method without an entry is denied, and New refuses to build
//     a server whose table does not cover every RPC of the descriptor, so a new RPC
//     cannot ship unauthenticated, whatever interceptors the gRPC server has.
//   - Mutating methods and sensitive reads (logs, diagnostics, credential
//     metadata, support bundles) need cluster admin: the `admin` verb on every
//     configured instance name. Other reads need `execute` or `admin` on at least
//     one instance name; operations are only visible on instance names the caller
//     holds `execute` or `admin` on.
//   - Every call to an audited method writes exactly one structured JSON record
//     (Auditor): time, principal, display name, method, request summary with
//     secrets redacted, result code and duration. Responses are never audited.
//
// The server never talks to Kubernetes or AWS itself: it reads and changes the
// fleet through narrow consumer-owned interfaces (sources.go) that cucina-controller
// wires to the reconciler, the enrollment and key stores, the scheduler's
// BuildQueueState API and SSM. docs/dev/mgmt.md lists them with the adapters the
// controller provides. Streams (WatchOverview, WatchOperations) are served from
// shared, single-flight snapshots so that the number of subscribers never
// multiplies the load on the scheduler or the cloud.
package mgmt
