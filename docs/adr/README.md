<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Architecture decision records

An ADR records one decision that is expensive to reverse, or one deliberate deviation
from the plan, together with the reason. Code and docs say *what* Cucina does; ADRs say
*why*, and what we gave up.

## When to write one

* A requirement or contract could not be met as written, and you chose the closest
  alternative that keeps its intent. Write the ADR in the same change.
* You add a library or tool that is not on the pinned lists, or you pick one of several
  reasonable designs (a protocol, a storage layout, a privilege model).
* You measured something and the result decides a default (a build-directory mode, an
  instance type, an Xcode or Visual Studio version).

Do not write one for choices that are cheap to change, or that the code explains by itself.

## Format

One file, `docs/adr/NNNN-short-slug.md`, short (a screen or two):

```markdown
# NNNN — Title in the imperative or as a statement of fact

* Status: proposed | accepted (YYYY-MM-DD) | superseded by NNNN | deprecated

## Context
What forced the decision: the requirement, the constraint, the measurement. Name the
alternatives that were considered and why they lost.

## Decision
What we do, precisely enough to check.

## Consequences
What gets easier, what gets harder, what to watch, when to revisit.
```

Rules: never rewrite history. To change a decision, add a new ADR and mark the old one
`superseded by NNNN`; a typo or a missing link may be fixed in place. Keep the one-line
title meaningful, because the index below is built from it.

## Numbering

ADR numbers are allocated by range, so parallel work never collides. Pick the next free
number in your range.

| Range | Area |
| --- | --- |
| 0001–0099 | architecture and cross-cutting policy (0011–0024 record the baseline decisions D1–D14) |
| 01xx | build and Bazel |
| 02xx | AWS infrastructure and the e2e environment |
| 03xx | worker images (Linux, Windows, macOS) |
| 04xx | Helm chart and Buildbarn configuration |
| 05xx | controller, scaling, EC2 provider, worker agent |
| 06xx | authentication, STS, PKI, enrollment |
| 07xx | macOS host agent, packaging, MDM |
| 08xx | `cucinactl` |
| 09xx | cross-platform builds |
| 10xx | testing, e2e harness, canaries |

## Index

The table is generated; do not edit it by hand. After adding or retitling an ADR run
`tools/ci/adr-index.sh` (CI runs it with `--check`).

<!-- BEGIN ADR INDEX -->
| ADR | Title | Status | Area |
| --- | --- | --- | --- |
| [0001](0001-buildbarn-dual-schema-pins.md) | Buildbarn version pins: worker and storage schemas differ | accepted | architecture |
| [0002](0002-queue-declaration-from-values.md) | Platform queues are declared by the chart, not at runtime | accepted | architecture |
| [0003](0003-rust-rpc-stack-connect-rust-buffa.md) | Rust RPC and protobuf stack: connect-rust + buffa (not tonic/prost) | accepted — user decision, 2026-10-02 | architecture |
| [0004](0004-small-functional-test-resources.md) | Use small resources for functional acceptance | accepted | architecture |
| [0011](0011-workers-are-vms.md) | D1: Workers are VMs running bb_worker and bb_runner natively | accepted | architecture |
| [0012](0012-custom-controller-queue-driven.md) | D2: One Go controller drives pools from the scheduler's queue state | accepted | architecture |
| [0013](0013-ec2-launch-on-demand-no-stop.md) | D3: EC2 pools launch on demand and terminate on scale-in | accepted | architecture |
| [0014](0014-macos-custom-hostd-tart.md) | D4: macOS hosts run a custom agent that shells out to Tart | accepted | architecture |
| [0015](0015-upstream-buildbarn-unmodified.md) | D5: Upstream Buildbarn, unmodified, no web UI | accepted | architecture |
| [0016](0016-cache-tiers-no-object-storage.md) | D6: Three cache tiers on local storage, no object-storage backend | accepted | architecture |
| [0017](0017-virtual-build-directories.md) | D7: Virtual build directories on every OS where they work | accepted | architecture |
| [0018](0018-platform-identity-size-classes.md) | D8: Platform identity is the REAPI lexicon plus `xcode-version`; sizes are size classes | accepted | architecture |
| [0019](0019-auth-oidc-sts-jwt-mtls.md) | D9: OIDC to STS to short-lived JWT for clients; mTLS for workers and hosts | accepted | architecture |
| [0020](0020-bazel-monorepo-go-rust.md) | D10: A Bazel monorepo with Go and Rust, one protobuf API | accepted | architecture |
| [0021](0021-cross-platform-routing-generated-platforms.md) | D11: Cross-platform routing is generated Bazel platforms plus exact runner properties | accepted | architecture |
| [0022](0022-qemu-user-for-foreign-architectures.md) | D12: riscv64, s390x and armv7 tests run under qemu-user | accepted | architecture |
| [0023](0023-windows-tests-from-non-windows-clients.md) | D13: Windows tests from Linux and macOS clients via a `@bazel_tools` overlay, else a patched Bazel | accepted | architecture |
| [0024](0024-testing-policy.md) | D14: Tiered, admission-gated tests; fakes behind owned ports; production guards | accepted | architecture |
| [0025](0025-test-change-enforcement.md) | Weakening a test needs a `Test-Change:` trailer, enforced in layers by cheap heuristics | accepted | architecture |
| [0026](0026-flake-policy-and-quarantine.md) | Quarantine is a set of BUILD tags with an expiry that CI enforces | accepted | architecture |
| [0027](0027-third-party-notices-generation.md) | Third-party notices are generated from `go list` and cargo-about, not from a licence-scanning service | accepted | architecture |
| [0100](0100-bazel-monorepo-foundation.md) | Bazel monorepo foundation | accepted | build (bazel) |
| [0101](0101-aws-lc-rs-overlay.md) | aws-lc-rs 1.18.1 on the rules_rs aws-lc-sys overlay (BCR AWS-LC 5.1.0) | accepted | build (bazel) |
| [0102](0102-macos-sdk-acquisition.md) | How hermetic-llvm acquires the macOS SDK today (input to R-XPLAT-8) | accepted as the interim state | build (bazel) |
| [0103](0103-windows-rust-gnullvm.md) | Windows `cucinactl` targets x86_64-pc-windows-gnullvm (MSVC fallback) | accepted | build (bazel) |
| [0104](0104-tier-guard-aspect.md) | The tier-tag guard is an aspect, not a `bazel query` test | accepted | build (bazel) |
| [0105](0105-protoc-version-normalisation.md) | Checked-in Go protobuf code is identical from Bazel and `buf generate` | accepted | build (bazel) |
| [0106](0106-rust-codegen-buffa-connect.md) | Rust protobuf/RPC code generation in Bazel: protoc + buffa/connect-rust plugins | accepted | build (bazel) |
| [0107](0107-windows-native-toolchain-compatibility.md) | Enable native Windows Rust builds with narrow toolchain patches | accepted | build (bazel) |
| [0108](0108-hosted-macos-envtest-hostname.md) | Give hosted macOS envtest a valid hostname | accepted | build (bazel) |
| [0109](0109-windows-gawk-gnu-abi.md) | Build the Windows awk tool with its supported GNU ABI | accepted | build (bazel) |
| [0110](0110-build-oci-images-with-rules-img.md) | Build OCI images with rules_img | accepted | build (bazel) |
| [0150](0150-release-versioning-and-stamping.md) | One release version, stamped by Bazel from the VERSION file | accepted | build (bazel) |
| [0151](0151-release-pipeline-and-publishing.md) | Release pipeline: per-runner Bazel builds, one assembly, publish from verified bytes | accepted | build (bazel) |
| [0152](0152-nightly-and-report-lanes.md) | Nightly, system and mutation lanes outside the gating CI | accepted | build (bazel) |
| [0200](0200-e2e-vpc-raw-resources.md) | The e2e VPC is built from raw resources, not `terraform-aws-modules/vpc` | accepted | aws (infra) |
| [0201](0201-e2e-ipv6-egress-and-ssm.md) | Private-subnet workers reach SSM over IPv6 only with dual-stack endpoints enabled | accepted | aws (infra) |
| [0202](0202-e2e-iam-least-privilege.md) | Least-privilege IAM for the e2e environment: tag-gated controller, protected nodes, confined SSM | accepted | aws (infra) |
| [0301](0301-windows-service-wrapper-shawl.md) | shawl (not WinSW) wraps bb_worker/bb_runner as Windows services | accepted | images |
| [0302](0302-windows-boot-orchestration-and-accounts.md) | Windows worker boot orchestration and service accounts | accepted | images |
| [0303](0303-windows-defender-exclusions.md) | Microsoft Defender — path/process exclusions by default, Dev Drive as an option | accepted | images |
| [0304](0304-visual-studio-build-tools-pinning.md) | Pinning Visual Studio 2026 Build Tools | accepted | images |
| [0305](0305-qemu-cross-runtimes.md) | qemu-user and cross glibc runtimes on the x86_64 Linux image | accepted | images |
| [0306](0306-linux-worker-boot-trims.md) | Linux worker boot trims (no SSH, reduced cloud-init, volatile journal) | accepted | images |
| [0307](0307-private-source-image-pins.md) | Private source-image pins and build attestations | accepted | images |
| [0308](0308-fast-launch-child-tag-reconciliation.md) | Reconcile replacement Fast Launch snapshot tags | accepted | images |
| [0350](0350-macos-worker-image.md) | macOS worker image: Cirrus base, unprivileged auto-login build user, hostd-started services | accepted | images |
| [0351](0351-macos-build-directory.md) | macOS VMs use NFSv4 virtual build directories | accepted | images |
| [0352](0352-xcode-source-for-worker-images.md) | Where a worker image's Xcode comes from | accepted | images |
| [0353](0353-macos-runner-concurrency.md) | macOS VMs: Xcode and generic runners each offer vCPU slots | accepted | images |
| [0400](0400-buildbarn-config-rendering.md) | Buildbarn configuration: Helm-rendered protojson, `importstr` for CA bundles, startup self-checks | accepted | chart and buildbarn |
| [0401](0401-in-cluster-hops.md) | In-cluster Buildbarn hops are plaintext, guarded by NetworkPolicies | accepted | chart and buildbarn |
| [0402](0402-storage-volumes.md) | Storage volumes: one filesystem PVC (file mode) or raw Block for the CAS plus a small filesystem PVC | accepted | chart and buildbarn |
| [0403](0403-tls-certificates.md) | TLS: per-component server certificates in a public and an internal group | accepted | chart and buildbarn |
| [0404](0404-exposure-services.md) | Exposure: one Service per endpoint and backend; management goes to the leader | accepted | chart and buildbarn |
| [0405](0405-size-profiles-and-storage-sizing.md) | Size profiles as chart data; storage layout derived from one size per store | accepted | chart and buildbarn |
| [0406](0406-crds-directory-and-apply-hook.md) | CRDs in crds/, upgraded by a `crds apply` hook | accepted | chart and buildbarn |
| [0410](0410-one-bb-runner-per-worker.md) | One bb_runner per worker; per-runner settings live in bb_worker | accepted | chart and buildbarn |
| [0411](0411-l1-placement-and-block-sizing.md) | Worker L1: placement order and block sizing | accepted | chart and buildbarn |
| [0412](0412-config-boot-tests-on-the-dev-mac.md) | Booting every rendered profile with darwin binaries | accepted | chart and buildbarn |
| [0413](0413-host-l2-rendering-and-trust.md) | Host L2: rendering, listener trust and compression | accepted | chart and buildbarn |
| [0414](0414-buildqueue-adapter-semantics.md) | BuildQueue adapter: counts, errors and retries | accepted | chart and buildbarn |
| [0500](0500-autoscaler-core-observation-decision-loop.md) | Autoscaler core: one pure decision function over raw observations | accepted | controller and scaling |
| [0501](0501-launch-ledger-idempotency.md) | Deterministic launch tokens from a write-ahead launch ledger | accepted | controller and scaling |
| [0502](0502-startup-failure-circuit-breaker.md) | Startup failures: probe one VM at a time, back off per event | accepted | controller and scaling |
| [0503](0503-idle-semantics-and-drains.md) | Idle means "idle at every poll"; blind polls restart timers; drain ownership by pattern | accepted | controller and scaling |
| [0504](0504-yaml-for-simulation-scenarios.md) | `go.yaml.in/yaml/v3` for simulation scenarios | accepted | controller and scaling |
| [0520](0520-ec2-launch-runinstances-single-client-token.md) | EC2 launches: sequential RunInstances with one client token per launch | accepted | controller and scaling |
| [0521](0521-ec2-api-throttling-hygiene.md) | EC2 API throttling: in-package token buckets and adaptive SDK retries | accepted | controller and scaling |
| [0522](0522-ec2-price-list-filters-and-fallback.md) | Instance prices: Price List filtered by regionCode and operation, dated embedded fallback | accepted | controller and scaling |
| [0550](0550-controller-component-registry.md) | cucina-controller is assembled from build-time optional components | accepted | controller and scaling |
| [0551](0551-enrollment-launch-verification-from-ledger.md) | EnrollWorker verifies launches against the pool's launch ledger | accepted | controller and scaling |
| [0552](0552-certificate-list-and-config-gaps.md) | Server certificates come from a `--certs` list; fixed names for controller-owned Secrets | accepted | controller and scaling |
| [0570](0570-worker-agent-l1-device-vs-sizing.md) | Worker agent picks and mounts the L1 volume; internal/bbconfig sizes it | accepted | controller and scaling |
| [0571](0571-worker-key-next-to-certificate.md) | The per-boot worker key lives next to its certificate in /etc/cucina/pki | accepted | controller and scaling |
| [0572](0572-deadman-idleness-from-bb-worker-metrics.md) | Dead-man idleness from bb_worker's file-pool and executor metrics | accepted | controller and scaling |
| [0573](0573-boot-data-user-data-format.md) | EC2 boot data: versioned JSON in user data, no TOFU, tolerant of new fields | accepted | controller and scaling |
| [0574](0574-spot-drain-and-bootstrap-failure-policy.md) | Spot notice drains with a service stop; every bootstrap failure powers off | accepted | controller and scaling |
| [0580](0580-management-api-guard-and-access-classes.md) | Management API: authorization enforced by the service, three access classes | accepted | controller and scaling |
| [0581](0581-worker-logs-ssm-polling.md) | Worker logs: SSM Run Command polling for EC2, host diagnostics for Tart | accepted | controller and scaling |
| [0582](0582-operator-drain-kill-and-floor-semantics.md) | Operator drains, kills and floors through the management API | accepted | controller and scaling |
| [0600](0600-deny-list-exact-substring-encoding.md) | Deny-list: quoted match tokens tested with `contains()` on the raw file | accepted | auth, STS, PKI |
| [0601](0601-service-key-hmac-pepper.md) | Service-account keys: 256-bit random secrets hashed with HMAC-SHA-256 and a pepper | accepted | auth, STS, PKI |
| [0602](0602-in-process-oidc-test-issuer.md) | Go STS tests use an in-process OIDC issuer; mock-oauth2-server stays for CLI and e2e | accepted | auth, STS, PKI |
| [0603](0603-sts-protocol-details.md) | STS protocol details: subjects, sessions, errors, service keys, break-glass | accepted | auth, STS, PKI |
| [0650](0650-ec2-identity-rsa2048-ledger-and-lease-replay.md) | EC2 workers prove identity with the RSA-2048 PKCS#7 document signature, checked against the launch ledger, once per launch | accepted | auth, STS, PKI |
| [0651](0651-mac-host-enrollment-semantics.md) | Mac host enrollment binds the first key; the bound key may recover; tokens are SHA-256 hashed | accepted | auth, STS, PKI |
| [0652](0652-workload-identity-naming-and-revocation.md) | Workload identities are SPIFFE-style URI SANs; certificates are revoked by deny-list, not CRL | accepted | auth, STS, PKI |
| [0653](0653-single-private-ca-and-two-root-rotation.md) | One private CA per installation, rotated with a two-root bundle in three phases | accepted | auth, STS, PKI |
| [0700](0700-hostd-vm-network-relay.md) | Mac VMs reach the control plane only through hostd: L4 relay + per-VM certificates | accepted | macOS, hostd, pkg |
| [0701](0701-hostd-privilege-drop.md) | hostd runs `tart` as `cucina` via `launchctl asuser` + a setuid trampoline | accepted | macOS, hostd, pkg |
| [0702](0702-hostd-registry-credentials.md) | Tart image pull credentials: static read-only package token, short exposure | accepted | macOS, hostd, pkg |
| [0703](0703-hostd-in-vm-configuration.md) | hostd configures each VM at every boot and starts its Buildbarn jobs; dead-man from outside | accepted | macOS, hostd, pkg |
| [0750](0750-host-pkg-layout-and-build.md) | Host package layout, setup helper and AppleDouble-free payloads | accepted | macOS, hostd, pkg |
| [0751](0751-autologin-by-postinstall.md) | Auto-login of the `cucina` user is set up by the package by default | accepted | macOS, hostd, pkg |
| [0752](0752-private-signing-certificate-profile.md) | Separate private application and installer signing identities | accepted | macOS, hostd, pkg |
| [0753](0753-pkg-publishing-github-releases.md) | Publishing the host package as immutable GitHub Release assets | accepted | macOS, hostd, pkg |
| [0754](0754-install-settings-domain.md) | Separate preference domain for the package's install settings | accepted | macOS, hostd, pkg |
| [0755](0755-headless-autologin-credential.md) | Prepare auto-login credentials and the login keychain without a GUI | accepted | macOS, hostd, pkg |
| [0756](0756-mdm-schema-precedence.md) | Prefer Apple's published MDM schema to outdated key names | accepted | macOS, hostd, pkg |
| [0801](0801-cli-token-cache-locking-and-dependencies.md) | cucinactl: token-cache locking and the few dependencies beyond R-LIB-3 | accepted | cli |
| [0802](0802-cli-bazelrc-platforms-and-targets-schema.md) | `cucinactl bazelrc`: flag scope, platform labels and the targets schema | accepted | cli |
| [0803](0803-cli-action-inspection-of-uncached-results.md) | `action inspect`: uncached results through `HistoricalExecuteResponse` | accepted | cli |
| [0804](0804-cli-json-output-contract.md) | `--output json` is a hand-written, schema-checked contract | accepted | cli |
| [0805](0805-cli-credential-helper-and-login-details.md) | Credential helper and login details | accepted | cli |
| [0806](0806-cli-macos-browser-without-appkit.md) | cucinactl opens the browser with `open -u` on macOS (no AppKit) | accepted | cli |
| [0807](0807-cli-bazelrc-build-lines-and-platform-targets.md) | `cucinactl bazelrc` emits `build` lines and reads `platforms/targets.json` | accepted | cli |
| [0808](0808-cli-private-ca-and-http-proxies.md) | Private CA bundles and HTTP proxies for every cucinactl connection | accepted | cli |
| [0850](0850-tui-architecture.md) | `cucinactl tui`: reducer architecture, data refresh and safety rules | accepted | cli |
| [0851](0851-tui-vhs-goldens.md) | TUI flows as VHS goldens: deterministic demo data, manual tier | accepted | cli |
| [0900](0900-cucina-platforms-generated-module.md) | `@cucina_platforms`: a generated module, `targets.json` schema v1, labels and configs | accepted | cross-platform |
| [0901](0901-apple-sdk-on-the-exec-machine.md) | The Apple SDK is resolved on the macOS exec machine (R-XPLAT-8) | accepted | cross-platform |
| [0902](0902-supported-hermetic-llvm-toolchain-pairs.md) | Register only the supported hermetic-llvm toolchain pairs (macOS targets on macOS exec) | accepted | cross-platform |
| [0903](0903-test-placement-and-macos-test-runner.md) | Where test actions land; macOS-target tests use the Xcode runner | accepted | cross-platform |
| [0904](0904-windows-tests-bazel-tools-overlay.md) | Windows tests from Linux/macOS clients: a `@bazel_tools` overlay (no patched Bazel) | accepted | cross-platform |
| [1001](1001-vendored-trimmed-bazel-protos.md) | e2e collectors decode Bazel's BEP and compact execution log with vendored protos | accepted | testing and e2e |
| [1002](1002-e2e-remote-plumbing-ssm.md) | e2e remote plumbing: SSM Run Command scripts, background jobs, port-forward transfers | accepted | testing and e2e |
| [1003](1003-mock-idp-topology.md) | T10 runs navikt/mock-oauth2-server in-cluster over HTTPS, reached from the dev Mac by port-forward | accepted | testing and e2e |
| [1004](1004-canary-result-export.md) | Canary results reach Prometheus through the controller, not a Pushgateway | accepted | testing and e2e |
| [1005](1005-nfr-measurement-methods.md) | How the campaign computes the NFRs that need a definition | accepted | testing and e2e |
| [1006](1006-cross-campaign-evidence.md) | Cross-campaign setup and incomplete evidence | Accepted | testing and e2e |
<!-- END ADR INDEX -->

## The baseline decisions

[`docs/architecture.md`](../architecture.md#13-decisions) maps the fourteen baseline
decisions (D1–D14) to ADRs 0011–0024 and to the code and docs that implement them.
