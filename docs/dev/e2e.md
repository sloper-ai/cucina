<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# The scenario harness and the acceptance campaign

`test/e2e` is Cucina's one scenario harness (R-TEST-8d): the §10 campaign (T0–T22), local baselines, read-only production
smoke checks and the canary scenarios run on it. It is plain Go `testing`, with no framework.

## Layout

| Path | What |
| --- | --- |
| `test/e2e/harness` | Scenario registry, environment descriptor, SKIP logic, budget governor, JSON results |
| `test/e2e/scenarios` | T0–T22, `baseline-*`, `canary-*`, `kind-t0`, `cli-smoke`, `pods-ready`, `zero-scale` |
| `test/e2e/remote` | Hosts: SSM Run Command (Linux/Windows VMs), local (dev Mac); jobs, chunked output, transfers (ADR 1002) |
| `test/e2e/bazelrun` | Bazel invocations with the §10.2 and §10.4 flags; Abseil checkout, overlay and per-host `.bazelrc` |
| `test/e2e/collect/{bep,execlog,profile}` | Parsers: build event stream (JSON/binary), compact execution log, `--profile` trace |
| `test/e2e/collect/{prom,awsinv,spend}` | Prometheus snapshotter, tag-filtered EC2 inventory + sampler, cost (via `internal/cost`) |
| `test/e2e/nfr` | §8 catalogue and calculators: every NFR is computed, never eyeballed |
| `test/e2e/report`, `test/e2e/cmd/e2e` | Report generator with redaction; `e2e list|env|report|redact-check` |
| `test/e2e/abseil` | Abseil overlay (MODULE addition, `.bazelrc`) and local-baseline scripts |
| `test/e2e/third_party/bazel` | Vendored `spawn.proto` and trimmed `build_event_stream.proto` (ADR 1001) |
| `internal/canary` | Cache/exec canaries, `cucina_canary_*` metrics, `cucina-controller canary` command (ADR 1004) |
| `slo/` | Recording rules and SLOs shared by scenarios, canaries and alerts (`rules.json`, `sloth.json`) |
| `deploy/aws-e2e/scripts/campaign` | `run.sh` (phases), `port-forwards.sh`, `mock-oauth2-server.sh`, `baseline-host.sh` |

## Writing a scenario

A scenario declares what it needs and what it proves; the harness does the rest:

```go
&harness.Scenario{
    ID: "T42", Title: "…", Requires: []harness.Requirement{harness.RequiresAWS, harness.RequiresLinuxClient},
    Cost: harness.CostLow, EstimateUSD: 1, Timeout: time.Hour, NFRs: []string{"NFR-P3"},
    Post: append([]harness.Check{ZeroResidueCheck("nothing left")}, Guards...),
    Run: func(c *harness.Context) error {
        lr, err := openLane(c, LinuxLane)          // Abseil + overlay + cucinactl bazelrc on the client
        …
        o, err := lr.bazel("warm", BuildOpts{Command: "build"})
        c.NFR(nfr.CacheHits("NFR-P3", "linux", o.ExecLog.RemoteCacheHitRatio()))
        return nil
    },
}
```

* `Requires`: an unmet one is a SKIP with a reason (`aws`, `mac-host`, `idp`, `destructive`, `linux-client`, `windows-client`,
  `kubernetes`, `prometheus`, `cucinactl`, `cross-matrix`, `hostd-pkg`). `Envs` limits the environment kinds
  (`kind` ≤ 2 min per scenario, `prod-smoke` read-only only).
* Return `harness.Fail(…)` for a product verdict, `harness.Skip(…)` for a prerequisite found at run time; anything else is an
  environment error. Missed NFRs fail the scenario automatically.
* `Post` checks run after `Run` (even when it failed): PromQL thresholds from `slo/`, tag-filtered AWS describes, CLI JSON.
* Record numbers with `c.Metric`, structured outputs with `c.Record`, timelines with `c.Event`; every PromQL query is recorded
  by the client `svc.Prom(c)` returns.
* Register it in `scenarios/registry.go`; `TestRegistryCoversTheCampaign` checks IDs and NFR cross-references.

## Environment descriptor

`~/.config/cucina/e2e/<name>.json` (0600; it holds instance IDs and endpoints, never committed). Generate the AWS one from the
OpenTofu outputs, then fill in what OpenTofu cannot know (cucinactl binaries per OS, the Cucina commit for T22, T12 image IDs,
the T11 upgrade values file, the service-key files):

```sh
go run ./test/e2e/cmd/e2e env --name aws-e2e \
  --base ~/.config/cucina/aws-e2e/base-outputs.json --env ~/.config/cucina/aws-e2e/env-outputs.json
```

Secrets are referenced by path (`secrets.serviceKeyFile`, `readOnlyKeyFile`, `adminKeyFile`) and reach client VMs only over the
SSM port-forwarding path (`remote.PutPrivate`). `safety` holds `maxSpendUSD`, `maxInstances`, `allowDestructive` and
`allowOverBudget` (set only after the user approved an overrun).

## Running

```sh
source .work/env.sh                                     # profile default, us-west-1, run tags
deploy/aws-e2e/scripts/campaign/run.sh preflight        # descriptor, AWS session, cluster, port-forwards
deploy/aws-e2e/scripts/campaign/run.sh install linux    # T0, then T1–T3
go test ./test/e2e -run TestScenario -args -env aws-e2e -id T9c     # one scenario
bazel run //test/e2e:scenario -- --env=aws-e2e --id=T9c             # the same through Bazel
go run ./test/e2e/cmd/e2e report --env aws-e2e          # docs/reports/e2e-<date>.md (redacted)
go run ./test/e2e/cmd/e2e redact-check docs/reports/e2e-*.md        # gate before every commit of a report
```

Results: `<artifactsDir>/<runId>/results/result-<ID>.json` (+ `budget.json`, the governor's ledger); raw BEP, execution logs and
profiles under `<artifactsDir>/<runId>/<ID>/` on the dev-storage volume. Bazel runs on the hosts with a minimal environment so
the raw artifacts carry no credentials.

The budget governor admits a scenario only if spent (max of the AWS-measured actual and the scenario ledger) + reserve + its
estimate stays within $300; non-essential scenarios are skipped, essential ones are held until the user approves.

## Collectors (§10.4)

* **Bazel client**: BEP (wall, critical path, runner counts, NetworkMetrics, heap with `--memory_profile`), compact execution
  log (per-spawn queue/setup/execution/output-upload time, input/output bytes, platform, action digest), profile (critical path,
  slowest actions, phases, Bazel memory, network counters; Perfetto `trace_processor` SQL when `PERFETTO_TRACE_PROCESSOR` or
  `trace_processor_shell` exists), Bazel server peak RSS (VmHWM / PeakWorkingSet64 after `bazel shutdown` + build).
* **Control plane / workers**: PromQL over `slo/` recording rules and Buildbarn metrics (worker selectors per lane in
  `workerSelectors`; default `job=~".*worker.*"`).
* **AWS**: tag-filtered describes sampled every 10–15 s → instance lifecycles, instance-seconds by type, EBS GiB-hours,
  public-IPv4 hours, duplicate launch tokens; priced by `internal/cost`.
* Measurement definitions that needed a decision are in ADR 1005.

## Tests of the tooling

`go test ./test/e2e/... ./internal/canary/... ./slo/...` — parsers against real Bazel 9.2.0 fixtures (≤ 50 lines each, scrubbed),
calculators and redaction tables, governor and SKIP logic (unit); the POSIX job/transfer scripts, the Bazel collection pipeline
with a fake `bazel`, and both canaries against the pinned `bb_storage`/`bb_scheduler` (integration, localhost only). Nothing
needs AWS. Regenerate the vendored protos with `cd test/e2e/third_party/bazel && buf generate` (protoc-gen-go v1.36.12) and the
SLO files with `go run ./slo/cmd/slogen`.
