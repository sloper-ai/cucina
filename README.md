<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Cucina

Cucina is a self-hosted, batteries-included distribution of [Buildbarn](https://github.com/buildbarn) that gives Bazel remote execution and
remote caching (REAPI v2). You install it with **one Helm chart**, declare worker pools, and Cucina runs the rest: it generates the Buildbarn
configuration, launches workers when a build needs them, and shuts them down when it does not.

* **Zero idle cost on EC2.** Linux (x86_64 and Graviton) and Windows worker pools scale to zero: with nothing queued, no instances, volumes, IPs or
  network interfaces exist.
* **macOS pools on your own Mac minis.** Tart VMs on always-on Apple-silicon hosts, enrolled through MDM, with Xcode versions side by side.
* **One chart, unmodified Buildbarn.** Upstream release binaries, one pinned matched set, configuration rendered from values. You never write jsonnet.
* **Cross-compilation with tests on the target.** From any client, build every supported target and run its tests on a worker of the target OS and architecture.
* **No web UI.** `cucinactl`, a Rust CLI and TUI, manages pools, workers, hosts, queues, operations, keys and costs. Grafana dashboards are optional.
* **Short-lived credentials everywhere.** OIDC identities (Google, GitHub Actions, any OIDC provider) are exchanged for 15-minute tokens; workers and hosts use short-lived mTLS.

| Pool type | Runs on | Idle state ("scale to zero") |
| --- | --- | --- |
| EC2 Linux x86_64 and arm64 (Graviton) | EC2 instances in your VPC | No instances exist |
| EC2 Windows (x86_64) | EC2 instances | No instances exist |
| macOS (arm64) | Tart VMs on your always-on Mac minis | VMs shut down (disk kept); hosts stay on |

## Architecture

```text
 Bazel clients ──TLS + JWT──► frontend (bb_storage) ──────► storage shards (bb_storage `local`, persistent PVCs)  = L3
                                │ Execute
                                ▼
                              scheduler (bb_scheduler, predeclared queues) ◄── BuildQueueState ── cucina-controller ──► EC2 API
                                ▲ worker API (mTLS)                                                    ▲ outbound gRPC (mTLS)
         ┌──────────────────────┴──────────────────────┐                                              │
 EC2 Linux x86_64/arm64 + Windows workers (bb_worker +    Mac host: cucina-hostd ── tart ──► ≤ 2 macOS VMs (bb_worker + bb_runner, L1;
 bb_runner; qemu runners; L1 on NVMe / ephemeral EBS)              │                          Xcode + generic arm64 runners)
                                                                   └─ L2 bb_storage cache on host SSD
```

The control plane (frontend, storage, scheduler, controller) runs in Kubernetes and is always on. Workers are virtual machines that the controller launches from
the scheduler's queue and terminates when idle. [`docs/architecture.md`](docs/architecture.md) explains the components, data flows, cache tiers, identity model,
failure model and cost model.

## Quickstart

Start with a local **cache-only** control plane; remote execution needs a configured VM pool. You need `kubectl`, Helm 3 or 4, [Bazelisk](https://github.com/bazelbuild/bazelisk), and a local kind cluster with enough disk for the `small` storage profile. For k3s or EKS use the [chart](docs/operations/chart.md) and [exposure](docs/operations/exposure.md) guides instead of these localhost endpoints.

> **Pre-release:** the default Cucina image and OCI chart are not published. First [build and load the local controller image and CLI](docs/dev/getting-started.md#local-controller-image-for-kind-pre-release). That recipe creates the dedicated `cucina-dev` kind cluster and loads `cucina-controller:dev`; installing this checkout alone cannot pull a nonexistent release image.

1. **Install from the checkout.** Save local settings outside the public repository. The worker address below is a placeholder for this workerless local setup, not an address to give real VMs.

   ```sh
   umask 077
   mkdir -p -m 700 "$HOME/.config/cucina"
   cat > "$HOME/.config/cucina/kind-values.yaml" <<'YAML'
   sizeProfile: small
   images:
     controller: {repository: cucina-controller, tag: dev, digest: ""}
   endpoints:
     client: {host: localhost, port: 8980}
     worker: {host: localhost}
   exposure:
     client: {type: ClusterIP}
     api: {type: ClusterIP}
     worker: {type: ClusterIP}
   storage: {mode: file, storageClassName: standard}
   controller:
     aws: {enabled: false}
   pools: []
   YAML
   helm install cucina ./charts/cucina -n cucina --create-namespace \
     -f "$HOME/.config/cucina/kind-values.yaml" --wait --timeout 10m
   helm test cucina -n cucina --logs       # STS exchange plus CAS and AC round trips
   ```

2. **Expose the local Services.** Run each command in its own terminal and leave it running; all bind localhost by default. Start them only after checking that your kubectl context is `kind-cucina-dev`.

   ```sh
   kubectl -n cucina port-forward svc/cucina-client 8980:8980
   kubectl -n cucina port-forward svc/cucina-api-sts 8443:8443
   kubectl -n cucina port-forward svc/cucina-api-management 8444:8444
   ```

3. **Log in**, keeping the CA outside the checkout and piping the key without displaying it:

   ```sh
   kubectl -n cucina get secret cucina-ca -o jsonpath='{.data.ca\.crt}' | base64 -d > "$HOME/.config/cucina/cucina-ca.pem"
   kubectl -n cucina get secret cucina-break-glass -o jsonpath='{.data.key}' | base64 -d |
     cucinactl login https://localhost:8443 --ca-file "$HOME/.config/cucina/cucina-ca.pem" --key -
   cucinactl status
   ```

   For a shared deployment, configure OIDC administrators, verify their login and **revoke** the break-glass key. Disabling chart generation alone does not revoke an issued key. Give the canaries their own restricted credential first ([credential rotation](docs/operations/rotate-ca-credentials.md#3-other-credentials)).

4. **Configure Bazel and build.** Use `macos` or `windows` instead of `linux` for those clients. This example executes locally and uses Cucina's remote cache because there are no workers yet:

   ```sh
   cucinactl bazelrc --platform linux --cache-only --config-name cucina >> user.bazelrc
   bazelisk build //... --config=cucina
   ```

   The workspace's `.bazelrc` must contain `try-import %workspace%/user.bazelrc` (this repository already does). Generated configuration includes TLS and the credential helper. Repeating the same action from a fresh output base can reuse its remote results.

5. **Enable remote execution.** [Build the Linux/Windows images](docs/operations/images.md) or [enroll Mac hosts](docs/macos/mac-mini-setup.md), set reachable worker endpoints and a pool in your Helm values, then `helm upgrade`. Generate a fresh configuration **without** `--cache-only`. The first uncached action can then start a worker; confirm with `cucinactl pools list` and `cucinactl workers list`. This fleet path is not proven by the local cache smoke test.

Next: [configure worker pools](docs/operations/chart.md), [give CI access](docs/security.md), [size it](docs/sizing.md), [run it on EKS](docs/operations/eks.md).

## Documentation

| Topic | Where |
| --- | --- |
| How it works | [`docs/architecture.md`](docs/architecture.md), [`docs/contracts.md`](docs/contracts.md), [ADRs](docs/adr/README.md) |
| Operating it | [Runbooks and operations index](docs/operations/README.md): [chart](docs/operations/chart.md), [exposure](docs/operations/exposure.md), [EKS](docs/operations/eks.md), [sizing](docs/sizing.md), [data transfer and cost](docs/operations/data-transfer.md) |
| Worker images | [Linux and Windows](docs/operations/images.md), [macOS](docs/operations/macos-images.md) |
| Mac fleet | [Mac mini setup through MDM](docs/macos/mac-mini-setup.md), [MDM guides](docs/mdm/) |
| Identity and security | [`docs/security.md`](docs/security.md) |
| The CLI and terminal UI | [`docs/cli.md`](docs/cli.md), [`docs/tui.md`](docs/tui.md) |
| Releases and artifact signing | [`docs/operations/releasing.md`](docs/operations/releasing.md) |
| The acceptance environment | [`docs/operations/aws-e2e.md`](docs/operations/aws-e2e.md), [`docs/reports/`](docs/reports/) |
| Developing | [`docs/dev/getting-started.md`](docs/dev/getting-started.md), [Bazel](docs/dev/bazel.md), [`TESTING.md`](TESTING.md), [`AGENTS.md`](AGENTS.md) |
| Upstream patches | [`docs/upstream/`](docs/upstream/README.md) |
| Licences | [`LICENSE.md`](LICENSE.md), [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md) |

## Status

Cucina is pre-release. The **full AWS/Mac acceptance campaign is not yet complete**: local component tests and the kind install/upgrade/rollback/uninstall smoke do not prove fleet behavior or performance targets. "Built" below means implementation and tests exist, not that every release gate passed. Manual checks remain unsigned until a human records evidence.

| Component | State | Notes |
| --- | --- | --- |
| Contracts: CRDs, protobuf API, platform catalog, ports | built | [`docs/contracts.md`](docs/contracts.md) |
| Helm chart and Buildbarn configuration rendering | built; locally validated | Local kind lifecycle passed; pinned-binary render checks exist. EKS production validation is still pending |
| `cucina-controller` (autoscaler, reconcilers, STS, enrollment, management API) | built; fleet validation pending | Simulation/integration suites exist; the five-minute cache canary runs in-process. Operator-triggered CA rotation remains planned |
| EC2 provider and cost model | built | Real-AWS validation pending (the acceptance campaign) |
| Worker agent (`cucina-worker-agent`) | built | |
| Linux and Windows worker images (Packer) | in progress | Nothing is published, each operator builds their own ([images](docs/operations/images.md)) |
| macOS worker image and host agent (`cucina-hostd`) | in progress | The real root-daemon path needs a Mac mini ([manual check MT-001](docs/testing/manual/MT-001.md)) |
| macOS host package, MDM profiles, setup guide | in progress | Private signing certificate by default; Developer ID path scripted but off |
| `cucinactl` CLI | built | [`docs/cli.md`](docs/cli.md) |
| `cucinactl` terminal UI | in progress | `cucinactl tui`; manual check [MT-006](docs/testing/manual/MT-006.md) |
| Cross-platform Bazel routing (`@cucina_platforms`) | in progress | Windows tests from Linux or macOS clients need an upstream workaround ([ADR 0023](docs/adr/0023-windows-tests-from-non-windows-clients.md)) |
| AWS acceptance environment (OpenTofu) | in progress | Temporary; destroyed after the campaign |
| Testing infrastructure: tiers, fakes, simulation, scenario harness, git hooks | built; policy CI partial | [`TESTING.md`](TESTING.md); Test-Change range, quarantine and notices checks are runnable locally but not yet wired into the foundation workflow. The system-tier CI lane remains planned |
| Release automation: signed packages, GHCR images, chart, Homebrew tap | in progress | Nothing is published yet |
| Web UI | out of scope | By design |

## Limitations

* **Kubernetes-pod workers** are not supported: all workers are VMs. The provider interface leaves room for them.
* **The scheduler is a single replica** with in-memory state, as upstream. A restart loses the queue; Bazel retries ride it out.
* **macOS x86_64 and Windows arm64** are not supported, as targets or as execution platforms. EC2 Mac instances are not used.
* **Mac hosts need FileVault off** (auto-login and Tart's keychain requirement) and run at most two macOS VMs each (Apple's limit). See the setup guide for the trade-off.
* **macOS images cannot be published** (Xcode's licence); they live in a private registry package. Worker AMIs are built per AWS account.
* **Windows workers** have no upstream privilege separation; a low-privilege account limits the damage. Use one pool per trust level.
* **Production on EKS** is documented, not yet deployed.

## Repository layout

```text
api/                 CRDs and the protobuf API              charts/cucina/   the Helm chart
cmd/, internal/      Go: controller, host agent, worker agent  workers/        Packer templates for worker images
cli/                 Rust: cucinactl                         macos/          host package and MDM profiles
platforms/, bazel/   platform catalog, Bazel macros          deploy/aws-e2e/  OpenTofu for the acceptance environment
sim/, invariants/, slo/, test/e2e/   simulation, invariants, SLOs, scenario harness
docs/                architecture, runbooks, ADRs, manual checklists
```

## Contributing

Read [`TESTING.md`](TESTING.md) first: tests here exist to prove requirements, not to maximise coverage, and weakening one needs a `Test-Change:` line in the commit message.
[`docs/dev/getting-started.md`](docs/dev/getting-started.md) sets up a development machine. Enable the git hooks once per clone with `git config core.hooksPath .githooks`
(they check the `Test-Change` rule and scan pushes for secrets). This repository is public: never commit secrets, state files or environment identifiers.
Decisions that deviate from the plan are recorded as [ADRs](docs/adr/README.md). Coding agents: see [`AGENTS.md`](AGENTS.md).

## License

Cucina is licensed under the [Functional Source License, Version 1.1, ALv2 Future License](LICENSE.md) (FSL-1.1-ALv2). The licensor is Brwse Co.; each version converts to the Apache License 2.0
two years after its release. Third-party components keep their own licences: [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md).
