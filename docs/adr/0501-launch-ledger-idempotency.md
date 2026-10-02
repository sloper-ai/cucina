<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0501 — Deterministic launch tokens from a write-ahead launch ledger

* Status: accepted (2026-10-02)

## Context
R-SCALE-5: a restarted controller must never double-launch. EC2 `ClientToken` deduplicates launches, but only if the new leader
re-derives the *same* tokens for launches that may still be in flight, while never reusing tokens of old, terminated instances
(EC2 returns the old instance). `Describe` is eventually consistent: a fresh instance can be invisible for seconds, longer than a
leader failover, and workers may register with the scheduler before `Describe` lists them.

## Decision
Tokens are `cuc-<hash(cluster,pool)>-<epoch>-<seq>` (≤ 64 chars, also the `cucina:launch-token` tag). The per-pool ledger
`{Epoch, Committed, Next, At}` is persisted by the executor (WorkerPool annotation) **before** executing a decision's launches whenever
`Decision.LedgerChanged`. `Epoch` is random at ledger creation (a lost/recreated ledger never collides with older tokens). A seq stays
below `Committed` only once its outcome is known **and** its instance has been listed by `Describe` (or `ConsistencyGrace`, 2 min,
passed). `At` is refreshed by every launch attempt — fresh, reused or a retry of an ambiguous launch — so it bounds the age of the most
recent attempt. After a restart, unlisted seqs in `[Committed, Next)` are reused first (EC2 deduplicates them) and count as pending
capacity (and against `max`) until reused or `At + ConsistencyGrace`. Nothing is committed and nothing launches before the first successful
provider observation. Capacity accounting uses only provider listings, launch results, intents and ledger ghosts — never worker threads
of nodes the provider does not list. Ambiguous launch errors keep the token for a retry; throttling and `ErrSkipped` release it; ICE,
quota, invalid and stale-token responses consume it.

## Consequences
The deterministic simulation and the rapid state machine found four double-launch paths that this design closes (scenarios
`controller-restart`, `regressions/controller-restart-seed162-invisible-launch`, and restarts after a throttled or ambiguous launch was
retried long after its seq was first allocated). Costs: one annotation write before a launch batch and one after its
results; after a restart, scale-out may be held for up to `ConsistencyGrace` for launches that really failed.
