<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0015 — D5: Upstream Buildbarn, unmodified, no web UI

* Status: accepted (2026-10-02)

## Context
Cucina's value is the glue around Buildbarn, not a fork of it. bb-browser is deprecated upstream and
bb-portal was declined, and a second UI would double the surface to secure and document.

## Decision
Run the upstream release binaries of one matched set of date tags, with every configuration rendered
by the Helm chart (or, for workers and hosts, by Go code type-checked against the pinned protos).
There is no web UI: the Rust CLI and TUI `cucinactl` is the management interface; Grafana dashboards
are an optional extra. If an upstream bug blocks us we carry a minimal patch together with an
upstream-ready description in `docs/upstream/`.

## Consequences
* Buildbarn parses its configuration strictly, so each bump goes through the render check that boots
  every pinned binary against every rendered profile (`docs/operations/buildbarn-upgrade.md`).
* The pins currently span two `bb-storage` schemas: [ADR 0001](0001-buildbarn-dual-schema-pins.md).
  Platform queues are declared by the chart: [ADR 0002](0002-queue-declaration-from-values.md).
