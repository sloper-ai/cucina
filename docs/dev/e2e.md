<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# The scenario harness and the acceptance campaign

`test/e2e` implements the §10 campaign as Go `testing` scenarios (R-TEST-8d). **Offline tooling tests are not acceptance results.** The lead owns provisioning, images, Helm values, credentials, approval to run the campaign and final teardown. No cloud resource is created by `e2e env` or `e2e check`.

## Layout

| Path | Purpose |
| --- | --- |
| `test/e2e/harness` | Registry, typed descriptors, capability/dependency checks, budget governor, JSON results |
| `test/e2e/scenarios` | T0–T22, baselines, canaries and smoke scenarios |
| `test/e2e/remote` | SSM/local execution, detached jobs, cancellation, complete output and verified file transfer (ADR 1002) |
| `test/e2e/bazelrun` | Bazel invocations, Abseil overlays, per-host rc files, artifact collection |
| `test/e2e/collect/{bep,execlog,profile,prom,awsinv,spend}` | Bazel/Prometheus/lifecycle evidence and cost integration |
| `test/e2e/nfr`, `test/e2e/report` | Calculators, complete/partial NFR aggregation and report redaction |
| `test/e2e/cmd/e2e` | `list`, `env`, `check`, `report`, `redact-check` |
| `test/e2e/abseil` | Native/cross rc configurations, extra freestanding example, local baseline scripts |
| `deploy/aws-e2e/scripts/campaign` | Phase runner, port-forwards, mock issuer and baseline-host helpers |
| `internal/canary`, `slo` | Shared canaries and SLO queries; canary/chart lifecycle has a separate owner |

## Prerequisites and operator sequence

**Current user override: small functional runners only.** The lead selects `.large` worker/client/control-plane shapes (2 vCPU / 8 GiB) and AWS pool maxima of one. The descriptor generator labels results `measurementScope: "small-functional"`; NFR-P2 timings remain diagnostic and cannot qualify the original max-four large-worker benchmark. `baseline-host.sh` has no large-instance fallback: it requires the chosen worker type and rejects shapes above the small limit before launch. Do not infer permission for larger shapes from the original §10 campaign descriptions below.

The following sequence is for the **lead's live campaign**, not an offline test. Its command-line interfaces are checked by the tooling tests; infrastructure readiness still requires live verification.

1. Provision the tagged `default`/`us-west-1` environment using the AWS runbook. Prepare the worker images, controller image, kubeconfig, private Helm values and Prometheus deployment/scrapes. The harness does not install Prometheus. The local CLI binaries must be built for **darwin-arm64**, **linux-amd64** and **windows-amd64**; their descriptor paths are local files uploaded to clients when needed.
2. Prepare a private JSON settings file with deployment-specific overrides. Typical fields are `kubernetes.valuesFiles`, `endpoints.prometheus`, per-pool `workerSelectors`, `devMac` hostd/package inputs, `images`, the accessible pinned `cucina` commit, and the client baselines. The default Prometheus URL is port 9090; override it if the lead uses another port. Jobs such as `cucina-workers` can be selected with `job="cucina-workers",pool="linux-x86-64"` (separate Windows/macOS selectors).
3. Generate the descriptor. The file is atomically written under `~/.config/cucina/e2e/` with mode 0600. Existing descriptors require `--force`; preserve custom settings with `--settings`. Objects merge recursively and arrays replace. Unknown fields, wrong regions, mismatched campaign tags and unsafe descriptor names fail.

   ```sh
   export CUCINA_AGENT=e2e
   source .work/env.sh
   go run ./test/e2e/cmd/e2e env --name aws-e2e \
     --base "$CUCINA_SECRETS_DIR/aws-e2e/base-outputs.json" \
     --env "$CUCINA_SECRETS_DIR/aws-e2e/env-outputs.json" \
     --values "$CUCINA_SECRETS_DIR/aws-e2e/values-endpoints.json" \
     --values "$CUCINA_SECRETS_DIR/aws-e2e/values-campaign.yaml" \
     --settings "$CUCINA_SECRETS_DIR/aws-e2e/settings.json" \
     --cucinactl-darwin "$CLI_DARWIN" \
     --cucinactl-linux "$CLI_LINUX" \
     --cucinactl-windows "$CLI_WINDOWS" \
     --allow-destructive --with-idp
   ```

   Set the three `CLI_*` variables to actual build outputs. `--with-idp` only configures the HTTPS mock issuer; it does not deploy or trust it. `--allow-destructive` is an explicit opt-in, not the default. `run.sh all` refuses to start without eligible teardown.

4. Bind the local CLI to this deployment. **T0 needs authenticated management access after installation.** Either preconfigure the matching profile or provide `kubernetes.bootstrapCLI: ["/absolute/private/bootstrap-script"]`. T0 executes that lead-owned script after `helm install`/`helm test`, suppresses its output, then verifies the selected profile's STS, REAPI, management and CA paths against the descriptor. The script exports credentials to private files and configures the local CLI; the harness neither invents keys nor selects an unrelated ambient cluster. Example CLI shape is `cucinactl login <STS-URL> --ca-file <CA-FILE> --key <KEY-FILE>`: `--key` is a **filename**, not the key text.
5. Establish the configured Prometheus port-forward, then check and run T0:

   ```sh
   go run ./test/e2e/cmd/e2e check --env aws-e2e --id T0
   bazelisk run //test/e2e:scenario -- --env=aws-e2e --id=T0
   ```

   `check` examines local files, declared capabilities, prior results and (without a bootstrap hook) local CLI configuration. It does not call AWS/Kubernetes or certify live readiness. A pre-existing release makes T0 SKIP; manual preflight of an existing release is not an official T0 pass.
6. Prepare **both client-type and worker-type baselines** for Linux/Windows. Add `clients.linux-baseline` / `clients.windows-baseline` with the corresponding worker instance type, then run `baseline-linux` / `baseline-windows` (`baseline-macos` for the Mac). NFR-P2 cannot pass with only the small client baseline. T1/T4 require positive baseline measurements before spending on a cold run. Run T1–T22 phases only after their inputs and independent review are ready.
7. Run teardown even after failures, generate the report, then run `redact-check` before any report commit. Phase `all` attempts teardown/report on normal completion and interruption; any FAIL, ERROR or SKIP makes its exit nonzero. Single phases leave lifecycle control to the lead; do not omit T15.

   ```sh
   deploy/aws-e2e/scripts/campaign/run.sh --env aws-e2e preflight
   deploy/aws-e2e/scripts/campaign/run.sh --env aws-e2e baselines linux
   deploy/aws-e2e/scripts/campaign/run.sh --env aws-e2e teardown report
   go run ./test/e2e/cmd/e2e redact-check docs/reports/e2e-*.md
   ```

For a compiler-independent run, the lead may freeze `go build -o <bin>/e2e ./test/e2e/cmd/e2e` and `go test -c -o <bin>/scenario.test ./test/e2e`; invoke the latter with `-test.run '^TestScenario$' -test.timeout 24h -env aws-e2e -id T0`. A no-environment invocation deliberately skips, not passes acceptance.

## Descriptor details and safety

* Files contain identifiers, not embedded secrets. `secrets.*` are private file paths. Temporary client keys use `remote.PutPrivate`, never SSM inline parameters; login supplies `--key FILE --ca-file FILE --credential-store=file`. The temporary key is deleted, while the CA remains for the profile and Bazel.
* Client STS, `remoteExecution` and `management` fields use the control plane's **private address**. After login, the harness overrides the CLI's REAPI/management profile fields before generating rc files and installing a matching host-scoped helper. Discovery may still advertise a public **token endpoint**: the lead must verify authentication routing separately before claiming zero public-IP traffic.
* Optional `devMac` client settings do not imply an enrolled Mac host. `mac-host` is enabled only with hostd binary/config inputs; packaging needs signed initial/upgrade packages, signer certificate, site approval and free VM slots. No system-wide installation is performed on the dev Mac.
* T9 needs isolation SG/tags; T10 needs the HTTPS mock CA/trust setup, signing-key rotation command, read-only key and valid worker/host certificates for positive mTLS controls. T11 needs upgrade values, T12 distinct images, and T22 a commit actually reachable by the clients. T20 destructive command coverage requires explicit disposable `cli` fixtures, not production-like names guessed by the harness.
* `safety.maxSpendUSD`, `maxInstances`, `allowDestructive` and `allowOverBudget` constrain execution. Only the user may authorize exceeding the budget. Raw artifacts are outside the repository; the descriptor is on the encrypted internal disk.

## Cross matrix (T16–T19)

See ADR 1006 and `docs/cross-compilation.md`. The runner consumes real `platforms/targets.json`/`pools.json` and generates each configuration through `cucinactl bazelrc --cross --target <P> [--exec-pool <pool>]`. It first runs the offline `xplatcheck -exec-pools` pre-check (17 target rows and three additional exec-pool rows at the current catalog). This check does not execute remote actions.

* Full rows build then test `//absl/...`. qemu rows build all of Abseil, then test the smoke subset with the catalog's timeout multiplier (currently `600,3000,9000,36000`). wasm/BPF build the freestanding C example only and explicitly record **test step: not applicable**.
* Compile/test routing is evaluated from REAPI properties, not platform names. macOS tests expect Xcode runner properties; generic macOS properties do not count. T17 moves each allowed non-default compile pool first without moving tests.
* T16 and the Mac's T18 Windows tests first run `tools/xplat/windows-test-overlay.sh`. Windows clients need no overlay; T19 compiles Linux and MinGW targets on Linux and tests on their target runners. Cross Windows invocations unset `BAZEL_SH`.
* Each configuration has a separate, expunged output base. Forced failure/re-execution scenarios additionally disable remote cache acceptance/test caching and require executed remote spawns. Cancellation stops the detached job, not just the poller.

## Evidence and remaining acceptance gaps

A missing declared NFR fails the scenario. An unavailable required check has an explicit reason and prevents PASS; known failures win over unavailable checks. Missing metric sources, logs and zero denominators are not measurements. The report marks incomplete catalog coverage **PARTIAL** rather than passing a Linux-only measurement for a multi-OS requirement.

These MUST clauses remain unavailable without additional evidence and must not be signed off from the current diagnostics:

* **T8 per-VM idle→actual termination latency:** management history records drain/terminate API-call completion, not idle transitions or EC2 disappearance. T8 currently records an unavailable timing check when the requisite pairs are absent. Live `idle_seconds` sampling correlated with provider disappearance is still needed; zero final residue alone proves only the final state.
* **NFR-X5 per-toolchain-version upload attribution:** total BEP upload counters do not identify LLVM/SDK/CRT blobs. T16 reports totals but marks this check unavailable; it cannot claim a full T16 pass from totals alone. Dependent cache scenarios remain blocked by a nonpassing T16 unless the missing evidence is implemented.
* **Campaign cost qualification:** the worker subtotal is not complete campaign spend. Baseline hosts, standing infrastructure, image builds, snapshots, Fast Launch and transfer charges require complete accounting; NFR-C3 remains unqualified without it. The original static standing-topology table does not qualify NFR-C2 for the new small topology. Unknown instance rates are explicitly incomplete, not zero-cost; the governor keeps the conservative reservation.
* **Cold-start timing:** controller polling can miss a short action. A null first-action latency, including a partial set of launch samples, cannot establish NFR-P1's maximum. The controller/REAPI evidence must supply it; the harness will not substitute zero.
* Hardware/MDM behavior that cannot be exercised inside the packaging VM still requires its manual checklist. A missing signed upgrade package, valid denial control, client lane, remote-execution log or isolated CLI fixture is an unmet prerequisite, not a successful test.

Other NFR methods and their bounds are documented in ADR 1005. Review recorded queries/results and limitations before signing off; registered scenarios alone are not proof of implementation coverage.

## Tooling verification

```sh
go test ./test/e2e/... ./slo/...
bazelisk test //test/e2e/... //slo/... --runs_per_test=20
bazelisk run //tools/xplat/cmd/xplatcheck -- -workdir /tmp/xplatcheck -exec-pools
```

Tests cover the parsers, NFR/budget/status logic, descriptor generation, private client setup, real local job cancellation, CLI boundary failures and matrix planning. They make no AWS calls. Canary implementation/tests and kind lifecycle are maintained separately; do not conflate their offline success with live campaign evidence.
