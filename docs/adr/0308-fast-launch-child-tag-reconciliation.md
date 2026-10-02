<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0308 — Reconcile replacement Fast Launch snapshot tags

* Status: accepted (2026-10-02)

## Context
R-POOL-2 permits a standing Windows Fast Launch snapshot pool, and campaign safety requires every resource to carry
its ownership/expiry tags. Real replenishment snapshots retained only AWS's `CreatedBy`, `CreatedByLaunchTemplateId`
and version tags. Launch-template instance/volume tag specifications and a one-time snapshot tag command therefore
do not provide continuing coverage. The service-linked role must remain untouched.

## Decision
The controller periodically invokes explicit, idempotent Fast Launch enable/reconciliation for its current image,
even when already enabled. Each image has at most one attempt per minute, including errors; successful observation
and attempt/error bookkeeping are separate. Read-only describe/inventory never writes tags.

The provider requires nonempty configured `ExtraTags` on an owned AMI and prep launch template. Only self-owned
snapshots with `CreatedBy=EC2 Fast Launch`, the exact parent template ID and the exact service AMI description may
inherit those tags. Pagination is exhausted before writing, existing conflicting values are rejected, and tags are
batched. The campaign IAM grant permits only its three tag keys/values on this exact prep template's children;
conflicting existing values also fail the IAM condition. No account-level setting or service-linked role changes.

Disable and teardown wait for both the disabled configuration and disappearance of its child snapshots. Untagged
children cannot be hidden by a campaign-tag filter. AMI/root-snapshot deletion still requires all three campaign
tags, and the old image must no longer be desired by a live reconciler before cleanup starts.

## Consequences
Tag coverage is **eventually reconciled**, not atomic at AWS-managed snapshot creation: there is a bounded normal
reconciliation window, and failures can extend it while remaining visible. This is the closest safe alternative to
inheritance that the observed service does not provide. Parent-lineage inventory keeps missing-tag children visible
during that window. Offline fake-clock, SDK-fake, CLI-fake and plan-policy tests guard the mechanism; deploying the
controller and the scoped IAM update, then verifying a real replenishment, remains an operator step.
