<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0013 — D3: EC2 pools launch on demand and terminate on scale-in

* Status: accepted (2026-10-02)

## Context
Zero idle cost for EC2 pools is a hard requirement. A stopped or hibernated instance still pays for
its EBS volumes, so stop/start pools and ASG warm pools are ruled out as the default. Measured
references: a fresh Linux launch is ready in about 35–40 seconds; a fresh Windows launch from a
custom AMI takes about four minutes, and about 85 seconds with EC2 Fast Launch.

## Decision
Instances are launched from the AMI when work arrives and terminated when idle; no stopped
instances, warm pools or hibernation by default. Speed is bought per launch instead: small AMIs
with everything preinstalled, trimmed boot, ordered instance-type and subnet lists, an optional
per-launch EBS volume-initialization rate. Windows pools enable EC2 Fast Launch, whose
pre-provisioned snapshots are the one approved standing cost besides AMI storage. Fast Launch is
enabled on every new Windows AMI and disabled (and the disabled state awaited) before the AMI is
deregistered.

## Consequences
* With a pool at zero nothing exists to pay for except AMI storage and, for Windows, Fast Launch
  snapshots (`docs/architecture.md`, "Cost model").
* Cold-start budgets are explicit and measured; every other standing cost (`minRunning > 0`,
  Fast Snapshot Restore, warm pools) is opt-in.
* The Fast Launch ordering matters in image rollouts and teardown: see
  `docs/operations/ami-rollout-rollback.md`.
