<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0650 — EC2 workers prove identity with the RSA-2048 PKCS#7 document signature, checked against the launch ledger, once per launch

* Status: accepted (2026-10-02)

## Context
R-SEC-3 lets EC2 workers enroll with the AWS-signed instance identity document. IMDS offers three signatures:
`/pkcs7` (DSA/SHA-1), `/signature` (base64 RSA **1024**-bit/SHA-256) and `/rsa2048` (PKCS#7 SignedData,
RSA-2048/SHA-256). `api/proto/cucina/v1/enrollment.proto` describes `EnrollWorkerRequest.signature` as an "RSA-2048
signature" but names the `/signature` path. The document and its signatures are readable by any process on the
instance, including build actions, and a rebooted instance keeps its `pendingTime`, so a reboot cannot be told apart
from a replay. "Consistent with the controller's own launch records" needs records that survive controller restarts.

## Decision
* Accept only the `/rsa2048` PKCS#7 form, verified with the AWS RSA-2048 certificate of the document's Region (the 34
  commercial Regions, embedded from the AWS documentation). A minimal stdlib parser (BER→DER normaliser, one signer,
  SHA-256, signed attributes with contentType and messageDigest) avoids a PKCS#7 dependency; anything else fails.
  The sent document must equal the signed content byte for byte.
* After account, Region and freshness (`pendingTime` ≤ 20 min old) checks, a cluster-tag-filtered `Describe` and the
  autoscaler's **launch ledger** (`internal/scaling` token prefix + WorkerPool ledger epoch + allocated sequence,
  `enroll.LedgerLaunches`) must confirm the instance; no separate launch log is kept.
* One enrollment per launch: a Lease per instance (atomic create, shared by all replicas) binds the first key; the same
  key may re-enroll within 10 min, at most 3 times; anything else is refused. A rebooted worker is replaced, not
  re-admitted (its dead-man switch terminates it).

## Consequences
* The worker agent must send the body of `/latest/dynamic/instance-identity/rsa2048` (and persist its per-boot key
  before enrolling, ADR 0571); the proto comment should name `/rsa2048` (lead).
* Instances launched under a lost or reset ledger cannot enroll and are replaced.
* New AWS Regions need their certificate added to `internal/enroll/awscerts.go`; other partitions can use
  `NewIdentityVerifierWithCerts`.
