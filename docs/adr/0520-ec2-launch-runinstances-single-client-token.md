<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0520 — EC2 launches: sequential RunInstances with one client token per launch

* Status: accepted (2026-10-02)

## Context
R-POOL-2 allows `CreateFleet` (`type: instant`) or `RunInstances` for the ordered walk over instance types × subnets.
`ports.Compute.Launch` must be idempotent per launch token ("a second Launch with the same token returns the instance the
first created", R-SCALE-5), tag the instance, its volumes and ENIs, and return capacity errors fast so the controller owns backoff.

Measured on 2026-10-02 in us-west-1 (t4g.nano, Amazon Linux 2023 minimal arm64, private subnet, 3 rounds, ephemeral program):

| Call | RunInstances | CreateFleet (instant) |
| --- | --- | --- |
| Successful launch, API latency | 1.04 / 1.14 / 1.16 s | 1.59 / 1.60 / 1.74 s |
| Refused combination (type not offered), API latency | 0.61 / 0.62 / 0.64 s | 1.16 / 1.29 / 1.34 s |
| API return → `running` (one sample each) | 3.1 s | 2.6 s |

Client-token semantics, verified on real EC2 with `RunInstances`: a call that fails (`Unsupported`) does **not** consume the
token — the same token then launches a different type; the same token and parameters return the same instance (also after it
terminated); the same token with other parameters returns `IdempotentParameterMismatch`. Idempotency is zonal (per AZ) when a
subnet is given.

`CreateFleet` still requires a launch template: overrides carry type, subnet, AMI, block devices, profile and metadata options,
but not user data, security groups, the public-IP flag or the shutdown behaviour. An instant fleet that launches nothing
*completes* (HTTP 200 with `Errors`) and burns its client token, so "same token ⇒ same instance" would need a new token per
retry — which brings duplicates back.

## Decision
`Launch` walks `InstanceTypes` (outer) × `SubnetIDs` (inner) with sequential `RunInstances` calls that all carry
`ClientToken = hex(sha256(cluster ‖ token))` (64 characters; per-cluster so installations sharing an account never collide).
Before the walk it looks the token up (`DescribeInstances` with the `client-token` filter plus the owner tags) and returns a
found instance. Capacity and quota errors (`InsufficientInstanceCapacity`, `Unsupported`, `VcpuLimitExceeded`, …) move to the
next combination; `IdempotentParameterMismatch` means an earlier combination succeeded, and the instance found by client token
(bounded wait for eventual consistency) is returned; any other error stops the walk. When every combination fails,
`*CapacityError{Tried}` is returned at once (`errors.Is` `ErrInsufficientCapacity` and/or `ErrQuotaExceeded`).
Spot (SHOULD) uses one-time spot `RunInstances` (interruption = terminate): every spot combination first, then on-demand if
`FallbackOnDemand`.

## Consequences
* ~0.5 s faster per launch and per refused combination; no launch templates to version, garbage-collect or authorise.
* Exactly once per Availability Zone. Across AZs a retry that races EC2's eventual consistency could launch twice (zonal
  idempotency); the client-token pre-check makes it unlikely, the provider logs it, and the `cucina:launch-token` tag lets the
  `NoDuplicateLaunchPerToken` invariant see it. Single-AZ pools (the default, R-DATA-4) are not affected.
* Capacity-optimized spot allocation is a fleet feature and is not available; the spot walk is "prioritized". Revisit (fleet +
  per-generation launch template) if spot becomes a default.
