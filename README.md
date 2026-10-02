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

This gets a control plane running on a local cluster and a first remote build going. It needs a Kubernetes cluster (k3s v1.36 or kind), `kubectl`, Helm 3 or 4,
and [Bazelisk](https://github.com/bazelbuild/bazelisk). Executing on EC2 or Mac pools needs more (an AWS VPC, worker images, hosts): the steps after the quickstart
point to those guides.

> **Status: pre-release.** Nothing is published yet. Until the first release the chart, the container images and `cucinactl` are built from this repository
> ([`docs/dev/getting-started.md`](docs/dev/getting-started.md)), and the OCI chart path `oci://ghcr.io/sloper-ai/charts/cucina` does not exist yet.

1. **Install the chart** from a checkout, with the `small` size profile:

   ```sh
   helm install cucina ./charts/cucina --namespace cucina --create-namespace \
     --set sizeProfile=small --set endpoints.client.host=<address clients will use>
   kubectl -n cucina rollout status deploy/cucina-frontend deploy/cucina-scheduler deploy/cucina-controller
   helm test cucina -n cucina        # a CAS and AC round trip through the client endpoint
   ```

   `helm status cucina -n cucina` prints the endpoints and the exact commands for the next steps.

2. **Log in** with the break-glass administrator key that `helm install` generated. Once OIDC administrators from `trustPolicies` exist, revoke the key (`cucinactl keys revoke`); turning it off in the chart values alone does not stop a key that was already issued:

   ```sh
   kubectl -n cucina get secret cucina-ca -o jsonpath='{.data.ca\.crt}' | base64 -d > cucina-ca.pem
   kubectl -n cucina get secret cucina-break-glass -o jsonpath='{.data.key}' | base64 -d |
     cucinactl login https://<sts address> --ca-file cucina-ca.pem --key -
   cucinactl status
   ```

3. **Print the Bazel configuration** for your platform and use it in your workspace:

   ```sh
   cucinactl bazelrc --platform linux --config-name cucina >> user.bazelrc
   ```

   It emits the endpoint, TLS, the host-scoped credential helper, the instance name, the platform definitions and the performance flags, as a `--config=cucina` section
   (without `--config-name` the lines apply to every build). Use `--platform windows` or `macos` on those clients, `--cache-only` for remote caching without remote execution,
   and `--cross --target <platform>` for cross-compilation.

4. **Build.** With an empty pool set the cache works immediately; with pools configured, the first action starts a worker and the pool returns to zero afterwards:

   ```sh
   bazelisk build //... --config=cucina
   cucinactl pools list          # desired and actual workers per pool
   ```

Next: [configure worker pools](docs/operations/chart.md), [build the Linux and Windows images](docs/operations/images.md), [enroll Mac minis](docs/macos/mac-mini-setup.md),
[give CI access](docs/security.md), [size it](docs/sizing.md), [run it on EKS](docs/operations/eks.md).

## Documentation

| Topic | Where |
| --- | --- |
| How it works | [`docs/architecture.md`](docs/architecture.md), [`docs/contracts.md`](docs/contracts.md), [ADRs](docs/adr/README.md) |
| Operating it | [Runbooks and operations index](docs/operations/README.md): [chart](docs/operations/chart.md), [exposure](docs/operations/exposure.md), [EKS](docs/operations/eks.md), [sizing](docs/sizing.md), [data transfer and cost](docs/operations/data-transfer.md) |
| Worker images | [Linux and Windows](docs/operations/images.md), [macOS](docs/operations/macos-images.md) |
| Mac fleet | [Mac mini setup through MDM](docs/macos/mac-mini-setup.md), [MDM guides](docs/mdm/) |
| Identity and security | [`docs/security.md`](docs/security.md) |
| The CLI | [`docs/cli.md`](docs/cli.md) |
| The acceptance environment | [`docs/operations/aws-e2e.md`](docs/operations/aws-e2e.md), [`docs/reports/`](docs/reports/) |
| Developing | [`docs/dev/getting-started.md`](docs/dev/getting-started.md), [Bazel](docs/dev/bazel.md), [`TESTING.md`](TESTING.md), [`AGENTS.md`](AGENTS.md) |
| Upstream patches | [`docs/upstream/`](docs/upstream/README.md) |
| Licences | [`LICENSE.md`](LICENSE.md), [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md) |

## Status

Cucina is under construction and has **not yet been proven on real infrastructure**: the acceptance campaign (a temporary k3s cluster on EC2, Linux and Windows pools,
a Mac host, cross-compilation, failure injection, scale-to-zero) has not run. "Built" below means the code and its tests are in this repository; "proven" means it passed the campaign.

| Component | State | Notes |
| --- | --- | --- |
| Contracts: CRDs, protobuf API, platform catalog, ports | built | [`docs/contracts.md`](docs/contracts.md) |
| Helm chart and Buildbarn configuration rendering | built | Rendering, schema and alert rules are tested, including against the pinned Buildbarn binaries; not yet installed on a production cluster |
| `cucina-controller` (autoscaler, reconcilers, STS, enrollment, management API) | built | Simulation and integration tests pass; no real-AWS run yet |
| EC2 provider and cost model | built | Real-AWS validation pending (the acceptance campaign) |
| Worker agent (`cucina-worker-agent`) | built | |
| Linux and Windows worker images (Packer) | in progress | Nothing is published, each operator builds their own ([images](docs/operations/images.md)) |
| macOS worker image and host agent (`cucina-hostd`) | in progress | The real root-daemon path needs a Mac mini ([manual check MT-001](docs/testing/manual/MT-001.md)) |
| macOS host package, MDM profiles, setup guide | in progress | Private signing certificate by default; Developer ID path scripted but off |
| `cucinactl` CLI | built | [`docs/cli.md`](docs/cli.md) |
| `cucinactl` terminal UI | in progress | `cucinactl tui`; manual check [MT-006](docs/testing/manual/MT-006.md) |
| Cross-platform Bazel routing (`@cucina_platforms`) | in progress | Windows tests from Linux or macOS clients need an upstream workaround ([ADR 0023](docs/adr/0023-windows-tests-from-non-windows-clients.md)) |
| AWS acceptance environment (OpenTofu) | in progress | Temporary; destroyed after the campaign |
| Testing infrastructure: tiers, fakes, simulation, scenario harness, git hooks | built | [`TESTING.md`](TESTING.md); the Test-Change range check and the quarantine check are not wired into CI yet |
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
