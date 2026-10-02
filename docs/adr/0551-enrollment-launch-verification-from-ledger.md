<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0551 — EnrollWorker verifies launches against the pool's launch ledger

* Status: accepted (2026-10-02)

## Context
R-SEC-3: an EC2 worker gets a certificate only after the controller verified its signed identity document, a
tag-filtered `DescribeInstances`, and that the instance carries the controller's own launch records
(`enroll.LaunchRecords`). `internal/enroll` ships `LaunchTokens`, stateless HMAC tokens minted per launch. The autoscaler
(`internal/scaling`, ADR 05xx of corea) instead derives the idempotency token — which is also the `cucina:launch-token`
tag — from the pool's launch ledger (`cuc-<hash(cluster,pool)>-<epoch>-<seq>`), so that a restarted controller reuses
in-flight tokens and EC2 deduplicates them (R-SCALE-5). A token cannot be both: an HMAC token is random per launch and
breaks the restart reuse; the ledger token is predictable.

## Decision
`controller.LedgerLaunches` implements `enroll.LaunchRecords`: the instance's token must parse, its prefix must be
`scaling.TokenPrefix(cluster, pool)`, its epoch the pool's current ledger epoch (WorkerPool annotation
`cucina.sloper.ai/launch-ledger`, read uncached) and its sequence number below the persisted `Next` (the ledger is
written ahead of every launch). Instances of a deleted pool or of a lost ledger never enroll; they fail their startup
timeout and are replaced.

## Consequences
Restart idempotency and enrollment use one token. The tag is not a secret: anyone who can launch instances with
arbitrary `cucina:*` tags in the account could guess a valid token. The real gates remain the AWS-signed identity
document, the tag-filtered Describe, and the IAM policy that lets only the controller's role create instances with
`cucina:*` request tags (deploy/aws-e2e, ADR 0202). Hardening option, if wanted: an additional HMAC tag
(`cucina:launch-mac`, key in a Secret) checked with `enroll.LaunchTokens`, keeping the ledger token for idempotency.
