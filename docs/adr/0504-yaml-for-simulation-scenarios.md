<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0504 — `go.yaml.in/yaml/v3` for simulation scenarios

* Status: accepted (2026-10-02)

## Context
R-TEST-8c specifies scenario YAML (fleet, workload, faults, seed, expectations). Scenarios are hand-written and the sweep writes
minimised regressions back as files. No Go YAML library is in the R-LIB-2 table.

## Decision
Use `go.yaml.in/yaml/v3` (the maintained continuation of `gopkg.in/yaml.v3` under the YAML organisation; already in the module graph
through `k8s.io/apimachinery`, well above the star bar) with `KnownFields(true)` (unknown fields are errors). It is used only by
`sim/` (tests and the `simctl` developer tool), never in a production binary; production configuration stays JSON.

## Consequences
No new module; strict decoding catches typos in scenario files. Traces stay JSONL (`encoding/json`).
