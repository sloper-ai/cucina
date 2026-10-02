// SPDX-License-Identifier: FSL-1.1-ALv2

// Package pki is Cucina's private certificate authority for workload identity
// (R-SEC-2/3): EC2 workers, macOS VM workers, Mac hosts, the controller's
// BuildQueueState client and the in-cluster server certificates.
//
// The normative identity formats and verification rules are documented in
// docs/security.md §Workload identity; this package is their implementation:
//
//   - identity.go: SPIFFE-style URI SANs (spiffe://cucina/<role>/…), grammar and parsing.
//   - buildbarn.go: the tlsClientCertificate JMESPath expressions the chart renders.
//   - ca.go: the CA (ECDSA P-256 by default) and its trust bundle (two roots during rotation).
//   - csr.go: CSR validation; a CSR contributes only its public key.
//   - issuer.go: strict leaf profiles; the issuer decides every certificate field.
//   - verify.go, pin.go: peer verification for Cucina's own mTLS endpoints, SPKI pins.
//   - reload.go: zero-downtime certificate/bundle reload (fsnotify) for tls.Config callbacks.
//   - expiry.go: the cucina_cert_expiry_seconds{role} metric.
//   - secrets.go, rotator.go: Kubernetes Secrets (EnsureCA, EnsureServerCerts, CA rotation
//     phases) and the leader-only server-certificate Rotator.
package pki
