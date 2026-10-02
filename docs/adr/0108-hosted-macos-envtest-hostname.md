<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0108 — Give hosted macOS envtest a valid hostname

* Status: accepted (2026-10-02); hosted regression validated

## Context

R-BUILD-6 runs the pinned envtest v1.36.2 control plane on GitHub's standard macOS runner.
Runs 37009866843 and 37013595135 reached the API server but failed its unchanged 20-second
shutdown deadline. Seventy uncached repetitions passed on the development Mac.

Captured control-plane output in run 37027489076 identified the difference: the hosted
runner's OS hostname exceeded Kubernetes' 63-byte label-value limit. Its API-server identity
Lease was repeatedly rejected, including after shutdown began; envtest then killed the server
at the deadline. No environment hostname or address is recorded here.

The pinned Kubernetes v1.36.2 code copies `os.Hostname()` directly into that Lease's hostname
label ([source](https://github.com/kubernetes/kubernetes/blob/v1.36.2/pkg/controlplane/apiserver/server.go#L307-L326))
and waits for the identity controller in a pre-shutdown hook
([source](https://github.com/kubernetes/kubernetes/blob/v1.36.2/pkg/controlplane/apiserver/server.go#L257-L281)).
The pinned binary's `--external-hostname` option controls generated URLs, not this label.

## Decision

Before Bazel starts, the macOS CI job gives its disposable runner the short hostname
`cucina-ci` and checks the exact read-back value. Both the workflow condition and the script
restrict this operation: `runner.os == 'macOS'`, plus `GITHUB_ACTIONS=true`,
`RUNNER_OS=macOS` and `RUNNER_ENVIRONMENT=github-hosted` at execution time.

Do not run this setup on a development Mac, self-hosted runner or cloud acceptance machine.
Keep `APIServerIdentity` enabled, retain the shutdown assertion and 20-second deadline, and
keep control-plane diagnostics. Do not add sleeps, retries, skips or a timeout waiver.

## Consequences and validation

The fixture now supplies a valid Kubernetes hostname instead of inheriting an arbitrary
provisioner-generated name. No Kubernetes binary, application behavior or runner size changes.

Controlled validation executes the actual workflow body with stateful fake `sudo` and
`hostname` commands. Local, wrong-OS and self-hosted contexts are rejected without mutation;
the allowed hosted-macOS context changes only fake state; incorrect read-back fails. All five
cases pass, as do actionlint and shellcheck. No real hostname command was executed locally.

Hosted run 37040910826 at `c91cd48` executed the guarded preparation and passed all 129 tests, with every test
executed rather than cached or skipped. Envtest completed under its unchanged 20-second shutdown deadline.
Remove the preparation when the pinned upstream handles long hostnames, then repeat that hosted check rather
than relaxing the test.
