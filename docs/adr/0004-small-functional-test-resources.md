<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0004 — Use small resources for functional acceptance

* Status: accepted (2026-10-02, operator decision)

## Context

The initial acceptance plan specified large compute instances for throughput benchmarks. Carrying those sizes into a one-action smoke test was unnecessary. The operator explicitly requested small test runners instead.

## Decision

Use two-vCPU instances for the temporary cloud environment: `m7i.large` for the control plane, clients and x86_64 test workers, and `m7g.large` for arm64 workers. Keep functional worker pools bounded at one instance each, zero when idle. Image builders and baseline helpers must also select small instances; a benchmark-sized launch requires a new explicit operator request.

Keep production pool configuration flexible. This decision limits the development campaign, not the distribution's ability to manage larger fleets.

## Consequences

Continue validating installation, authentication, worker lifecycle, execution, caching, routing and cleanup on the small topology. Record the actual instance types, capacity and measurements in the report. A local baseline on the chosen small worker remains useful diagnostic evidence.

Do not present these runs as qualification of the original multi-worker throughput target. Mark that performance comparison as not run for the small-functional scope rather than weakening its threshold or fabricating a pass. Other missing evidence remains missing evidence, not success.
