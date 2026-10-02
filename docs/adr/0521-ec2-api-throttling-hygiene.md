<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0521 — EC2 API throttling: in-package token buckets and adaptive SDK retries

* Status: accepted (2026-10-02)

## Context
R-SCALE-6 asks for client-side token buckets (RunInstances: burst 5, refill 2/s), batched IDs, tag-filtered Describe calls and
respect for EC2 throttling; R-SCALE-4 forbids hot loops on capacity errors. `golang.org/x/time/rate` is not on the R-LIB-2 list,
and R-LIB-1 requires an ADR for every added library. EC2 reports `InsufficientInstanceCapacity` with an HTTP 5xx status, which the
SDK's standard retryer retries (three more attempts against the same type and subnet).

## Decision
* A ~60-line token bucket inside `internal/providers/ec2`, driven by `ports.Clock` so tests advance time instead of sleeping. One
  bucket per API class: RunInstances 5 / 2 s⁻¹, Describe* 50 / 10 s⁻¹, TerminateInstances 20 / 5 s⁻¹, other mutations
  (DeleteVolume, DeleteNetworkInterface, Enable/DisableFastLaunch) 20 / 5 s⁻¹, pricing:GetProducts 5 / 2 s⁻¹ (`Options.Limits`;
  the controller passes `autoscaler.runInstancesBurst/RefillPerSecond`). A throttle answer that survives the SDK's retries drains
  its bucket; a `Retry-After` header blocks it for that long.
* SDK retryer: adaptive mode (client-side rate reduction on throttles, exponential backoff with full jitter), 4 attempts, 20 s max
  backoff, no SDK retry quota (`ratelimit.None` — our buckets meter requests), and capacity, quota, idempotency, image and
  validation errors marked non-retryable.
* Remaining throttling surfaces as `*ec2.ThrottleError` (`errors.Is(err, ports.ErrThrottled)`, with `RetryAfter`).

## Consequences
No new dependency. ICE comes back in one round trip (≈ 0.6 s measured, ADR 0520); backoff stays with the controller.
