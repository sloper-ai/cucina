<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0803 — `action inspect`: uncached results through `HistoricalExecuteResponse`

* Status: accepted (2026-10-02)

## Context
UC13/T20 inspect failed actions. Failed (non-zero exit) results are never written to the AC; `bb_worker` stores them as a
`buildbarn.cas.HistoricalExecuteResponse` in the CAS and prints `Action details (uncached result): <portalUrl>/<instance>/
blobs/sha256/historical_execute_response/<hash>-<size>/`. The management API's `GetOperation` returns only an
`OperationSummary` (no result), and Cucina has no bb-browser/bb-portal.

## Decision
* Vendor `pkg/proto/cas/cas.proto` from bb-remote-execution `1a3be957` (Apache-2.0) and generate it with the REAPI protos.
* `cucinactl action inspect` accepts an action digest, the portal-style `action`/`historical_execute_response` links Buildbarn
  prints (instance taken from the link), or an operation name (management API → action digest). Results come from the AC, or
  from the historical execute response for uncached ones; blobs are read through ByteStream with the user's JWT.

## Consequences
Failed actions are inspectable from the link Bazel prints, provided the chart sets a `portalUrl` (any URL; only the path is
used). Contract gap reported to the lead: `GetOperationResponse` could carry the completed `ExecuteResponse` (BuildQueueState
keeps it briefly) so `action inspect <operation>` also shows a just-failed result.
