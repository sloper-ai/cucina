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

**Current campaign scope: small-functional, with economical runners by default.** The lead has selected `.large` worker/client/control-plane shapes (2 vCPU / 8 GiB) and AWS pool maxima of one. The descriptor generator labels results `measurementScope: "small-functional"`; NFR-P2 remains `pass: false`, `unqualified` and `waivedBy: "ADR0004"`. Otherwise-valid scenarios get a distinct **FUNCTIONAL PASS**, accepted by dependencies only in the same small scope; full-scope P2 and every other required NFR/check remain gates. A worker-type P2 baseline is optional diagnostic work in this scope, not a reason to launch extra instances. The user's later 2026-10-02 clarification permits a larger runner when genuinely needed, while minimizing cost: try cheaper storage/cache corrections first, verify pricing, use the smallest sufficient size and bound its runtime. That permission does not retroactively qualify this campaign for the original benchmark topology. `baseline-host.sh` still deliberately has no automatic large-instance fallback; changes to its size guard or a campaign's declared scope require an explicit, reviewed configuration change.

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
6. Run the **native client baselines** needed for NFR-X2 (`baseline-linux`, `baseline-windows`, `baseline-macos`). In the approved small-functional campaign, do **not** launch extra worker-type baseline hosts merely to qualify waived P2: those measurements are optional diagnostics. Outside that explicit waiver, original-scope T1/T4 still require successful client and worker-type baselines before spending on the cold run. A compiler/invocation failure or missing test outcomes cannot become a baseline PASS. Run later phases only after their inputs and independent review are ready.
7. Run teardown even after failures, generate the report, then run `redact-check` before any report commit. Phase `all` attempts teardown/report on normal completion and interruption; any FAIL, ERROR or SKIP makes its exit nonzero. Single phases leave lifecycle control to the lead; do not omit T15.

   ```sh
   deploy/aws-e2e/scripts/campaign/run.sh --env aws-e2e preflight
   deploy/aws-e2e/scripts/campaign/run.sh --env aws-e2e baselines linux
   deploy/aws-e2e/scripts/campaign/run.sh --env aws-e2e teardown report
   go run ./test/e2e/cmd/e2e redact-check docs/reports/e2e-*.md
   ```

For a compiler-independent run, the lead may freeze `go build -o <bin>/e2e ./test/e2e/cmd/e2e` and `go test -c -o <bin>/scenario.test ./test/e2e`; invoke the latter with `-test.run '^TestScenario$' -test.timeout 24h -env aws-e2e -id T0`. A no-environment invocation deliberately skips, not passes acceptance.

## Single-writer execution policy

**Run one standalone scenario process at a time for a campaign.** `Governor` has an in-memory mutex for one instance, not a cross-process lock. `OpenGovernor` reads `budget.json` once; `Record`/`Refresh` later replace that snapshot through a shared `.tmp` filename and rename. The rename is not a transaction: concurrent governors can lose each other's entries, race on the temporary file, and admit work using stale spend. Even two governor objects in one process are not coordinated.

* Keep standalone baselines, T1 and other runner invocations **serial**. A comma-separated `-id` list runs sequentially in one runner. Use the intended concurrency **inside T7/T16**, whose single runner owns the budget journal.
* Do not run a separate `Refresh`/ledger writer while a runner holds its loaded ledger. Do not use separate results directories to bypass the shared campaign budget; that only hides the aggregate spend and can also collide on client workspaces.
* The governor persists no in-flight reservation/lease. After interruption, preserve the attempt and reconcile known resource costs before retrying; never delete an entry or silently merge estimates to manufacture headroom. A run-wide planning allowance is separate evidence, not verified billed spend or an automatically merged journal.
* A future narrow guard should acquire a **nonblocking, OS-held exclusive lock on a separate stable lock file** before reading the ledger and hold it through the last write/runner shutdown. Contention or inability to establish the lock must abort before any scenario side effect. Every writer must participate; locking only the final rename would not protect budget admission. The guard must not automatically steal a lock, merge ledgers, or reset estimates. This is **not implemented in the current frozen runner**, and requires its own two-process regression before use.

This policy does not block a serial T1 run and does not require modifying an active budget file.

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

## Metric-query contract

CAS byte queries use the pinned Buildbarn labels: lowercase `storage_type="cas"`, `backend_type="grpc"` for upstream reads, and `local_block_device|local_in_memory` for local reads. Operations (`Get`, `Put`, `GetFromComposite`) are case-sensitive. Scenarios use `slo.CASBytesIncrease` / `slo.CASBytesRate`; the standalone and chart rules provide `cucina:blob_bytes:rate5m`. These histogram sums describe logical blob sizes, not compressed wire bytes. All-layer frontend/rate totals are diagnostics and can count the same logical bytes more than once.

`cucina:cas_retention_seconds` selects only persistent L3 (`cucina_component="storage",storage_type="cas"`), not worker L1 or host L2. The pinned location-map gauge starts at construction/restart time; removing an old block replaces it with that block's old-queue entry timestamp. Before removal, retention is finite observed map age, not infinity or a missing-until-first-eviction series. Fresh storage cannot yet prove the unchanged four-hour acceptance margin above Bazel's three-hour TTL. Empty L3 results and query failures still fail the required check, even with a healthy controller scrape. See [contracts §6](../contracts.md#6-metrics-contract-names-are-api-dashboards-alerts-slos-scenarios-use-them).

The original logged T1 failure remains a failure. Correcting these selectors or re-querying historical data is diagnostic only: do not rewrite its result, replace its captured evidence, or erase independent failures (including the worker RSS limit miss).

## Evidence and remaining acceptance gaps

A missing declared NFR fails the scenario. An unavailable required check has an explicit reason and prevents PASS; known failures win over unavailable checks. Missing metric sources, logs and zero denominators are not measurements. The report marks incomplete catalog coverage **PARTIAL** rather than passing a Linux-only measurement for a multi-OS requirement.

The following checks now have concrete collectors, with explicit evidence requirements rather than permanent placeholders:

* **T10i complete exposure evidence:** the operator first binds `endpoints.publicHost` to one tag-owned numeric IPv4. Its public TLS REAPI endpoint must use the same address and supplies a real positive control. Nmap is required; failure never selects another scanner. The scan still covers TCP 1–65535 with the default ten-retransmission allowance, bounded to 24 minutes; the cancellation-aware TLS loop has four minutes inside the unchanged 30-minute scenario. A shorter parent deadline can still interrupt it and cannot qualify exposure. Success requires complete XML, exactly the intended host, successful completion, no scanner diagnostics, exact explicit-plus-aggregate port accounting, and TLS evidence on every open port. TLS-speaking is **not peer identity/certificate trust**; an error string alone cannot establish it. Raw XML, progress, stderr, tool version, argv, execution status and TLS errors are retained on success and failure under private `~/.config/cucina/e2e/t10i-*` directories (0700/0600). Only an allow-listed summary with raw-file hashes/sizes enters bulk artifacts. The original cancelled T10i remains ERROR; live validation of this repair is still pending.
* **T8 per-VM idle→actual termination:** T7 starts live collection before submitting its workloads and continues through natural scale-in. It records bracketed daemon `idle_seconds`, pool queue counts, acknowledged drains, and tag-filtered EC2 state observations including `terminated`. T8 evaluates every live-observed cohort node against the resolved idle timeout plus the controller's two-minute idle-drain grace, preserving sampling uncertainty, then independently verifies zero instance/volume/ENI/IP residue. Missing queue/idle/drain/terminal evidence or an API gap cannot pass; a resource leak wins over incomplete timing. Old T7 results without the collector must be rerun—bounded post-hoc management history is not retroactive evidence.
* **NFR-X5 attribution:** set `crossInventoryFile` before the first remote toolchain use. `e2e inventory --rules <export> --out <private-json>` creates an explicit canonical-repository map from **`bazel mod show_repo --all_repos`** in the prepared workspace. It records source-attribute versions where available and immutable resolved-rule revisions otherwise; pins include the owning module's rule and patch/source definition. Unused catalog variants are not claimed as exercised. Common build/test invocations then capture `--remote_grpc_log` under each client's private `.config/cucina/e2e/<run>/rpc` tree and transfer it only into the dev Mac's private `~/.config/cucina/e2e/<run>/rpc-collected`. Only allow-listed sanitized JSON is registered in bulk artifacts. Raw RPC logs may contain action/request data and are **not** bulk-safe merely because Bazel's interceptor omits Authorization headers.
* **Actual-client source binding:** before remote work, each lane exports its own resolved rules with remote cache/executor disabled into private storage. Every exercised repository pin is compared with the supplied inventory before sanitized evidence is registered. A central inventory is a setup seed, not proof of what the client built; local-path or source mismatches fail qualification. The capture records a hash of the actual export. T16 only reuses earlier captures whose registered artifact hashes still match the same run.
* **X5 qualification:** repository-labelled full `FindMissingBlobs` manifests plus the final marker/Tree AC record establish complete membership. Execution-log file identities prove which pinned variants each configuration used. Every offered ByteStream payload is joined by CAS scope/hash/size, including ordinary-action uploads; shared digests receive one logical credit. Failed/retried/already-present offered bytes remain counted; QueryWriteStatus completion and compressed finish offsets are checked separately. Synthetic Action/Command/marker/Tree data is housekeeping, not initial compiler-content evidence. T16 forces complete manifest generation with fresh output bases, disabled local repo-contents cache and remote cache reads on the seed builds, then replays the configurations with normal cache reads. This is an explicit cache-writing test, not a read-only probe. It can replace upstream intermediate repo-cache AC alternatives but does not delete CAS content. Hash-verified same-run T1 captures can supply first-upload evidence. Warm CAS with no observed initial content upload remains unqualified for that assertion; neither a zero log nor a newly forced manifest invents historical upload evidence.

These remaining constraints must still be met before sign-off:
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
