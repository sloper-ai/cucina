<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Runbook: Buildbarn upgrade

**Use when** you move to newer upstream Buildbarn releases. Cucina runs **unmodified upstream release binaries** from one pinned, matched set of date-tags, and renders their configuration to match that exact schema. A bump is a deliberate change that passes a gate (the config-render check), is rolled out in a fixed order,
and can be rolled back. **Severity:** Planned. **Time:** half a day including staging.

## Why this is careful

* Buildbarn parses configuration as **strict protojson**: one unknown field aborts startup. Releases rename fields (recently: the nested `keyLocationMap` of `local` storage, `portalUrl` for `browserUrl`, `grpc.client.address`).
* Workers and the control plane both embed Buildbarn: `bb_worker` and `bb_runner` are baked into worker images, `bb_storage` into the host package, the scheduler and storage into container images. A bump touches all of them.
* The pinned tags may span two `bb-storage` schemas at the moment ([ADR 0001](../adr/0001-buildbarn-dual-schema-pins.md)): worker configuration is rendered type-checked against one, `bb_storage` configuration against the other. A bump can remove that skew.

## Procedure

1. **Pick the release pair** and read what changed. Find the schema-relevant changes between your pinned tags and the new ones:

   ```sh
   gh api repos/buildbarn/bb-storage/compare/<old tag>...<new tag> --jq '.files[] | select(.filename | test("proto$")) | .filename'
   gh api repos/buildbarn/bb-remote-execution/compare/<old tag>...<new tag> --jq '.files[] | select(.filename | test("proto$")) | .filename'
   ```

   Read the proto diffs for renamed or removed fields, and the release notes. Do not take field names from memory: read the protos at the new tags.

2. **Find every pin** and change them together. The tags appear in more than one place:

   ```sh
   grep -rIEn '20[0-9]{6}T[0-9]{6}Z-[0-9a-f]{7}' --exclude-dir=.git --exclude-dir='bazel-*' --exclude='*.lock' --exclude=THIRD_PARTY_NOTICES.md . | cut -c1-140
   ```

   Expect: the pinned release binaries for tests and tools (`tools/pinned.bzl`, `@bb_release`), the Go modules (`go.mod` pins `bb-storage` and `bb-remote-execution` to the commits of the tags), the chart (`charts/cucina`: image tags and digests), the three worker images
   (`workers/linux/versions.json`, `workers/windows/versions.json`, `workers/macos/versions.json`), the host package (`macos/pkg/pins.env`, `macos/pkg/deps.MODULE.bazel`), and the docs ([`architecture.md`](../architecture.md), [`contracts.md`](../contracts.md), ADR 0001).
   Every binary is pinned by version **and SHA-256**: download the release's `sha256` asset (`gh release download <tag> -R buildbarn/bb-storage -p sha256 -p 'bb_storage.linux_amd64'`) and update the checksums; container images are pinned by digest.

3. **Let the compiler find schema changes.** Worker, runner and scheduler configuration is rendered from Go types of the pinned protos, so schema drift fails the build:

   ```sh
   go get github.com/buildbarn/bb-storage@<commit> github.com/buildbarn/bb-remote-execution@<commit>
   go build ./internal/bbconfig/...
   ```

   Fix the rendering code and its goldens (`Test-Change:` applies to regenerated goldens, see [`TESTING.md`](../../TESTING.md)). Chart templates and the host L2 template are text, so the next step covers them.

4. **Run the gate: the config-render check.** It renders every configuration profile (size profiles, TLS sources, storage modes, worker and host L2 configurations) and **starts each pinned release binary against it** with no Docker. A schema mistake aborts startup and fails the test:

   ```sh
   bazelisk test //charts/cucina/... //internal/bbconfig/... //internal/bbtest/...
   ```

   Nothing in the rollout below happens until this is green. It also proves one real action round trip against the pinned scheduler, storage and worker.

5. **Update ADR 0001.** If both repositories now agree on one `local` schema, delete the skew (render one schema everywhere); otherwise record the new pins and what still differs.

6. **Roll out to staging first, in this order:**

   1. **Build new worker images** that carry the new `bb_worker` and `bb_runner` (Linux and Windows AMIs, the macOS Tart image) and the new host package with `bb_storage`. Do not use them yet.
   2. **`helm upgrade` the control plane** ([Helm upgrade, rollback and uninstall](helm-upgrade-rollback-uninstall.md)). Storage shards roll one by one on their persistent volumes; the scheduler restarts (its in-memory queue is lost and clients retry); frontends roll behind it.
      Buildbarn discards persistent state it cannot read, so a storage upgrade that changes the on-disk format is a **cold-cache event** ([loss of storage](storage-loss.md)). In staging, confirm the cache survives: retention continues and a repeated build is above 99 % cache hits.
   3. **Roll the pools to the new images** ([AMI rollout and rollback](ami-rollout-rollback.md), [macOS images](macos-images.md)). Old-generation workers finish their actions and are replaced when idle. Check the release notes for any change of the scheduler-to-worker protocol; if there is one, the order above (scheduler first) matters.
   4. **Roll the Mac hosts** by publishing the new package through MDM ([macOS update](macos-update.md) for the host-side procedure).

7. **Verify** in staging, then production: `helm test`, `cucinactl status`, the cache canary, a cold remote build on every platform (`cucinactl bazelrc` per client), a warm rebuild (at least 99 % hits, no workers launched), and `cucinactl images` showing the new generation.

## Roll back

* **Control plane:** `helm rollback cucina <revision> -n cucina --wait`. The rolled-back storage may find the newer on-disk state unreadable and start cold; treat a failed upgrade of storage like a loss ([loss of storage](storage-loss.md)).
* **Workers:** point the pools back at the previous AMI or image ([AMI rollout and rollback](ami-rollout-rollback.md)); both are kept for exactly this.
* **Hosts:** publish the previous package version through MDM.
* **Code:** revert the pins and `go.mod` together; the gate in step 4 must pass again.

## Escalate

Attach the failing render-check output (it names the field), the old and new tags, and `cucinactl diag --include-logs`. If an upstream bug blocks you, carry a minimal patch with an upstream-ready description in [`docs/upstream/`](../upstream/README.md) rather than forking.
