<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0151 — Release pipeline: per-runner Bazel builds, one assembly, publish from verified bytes

* Status: accepted (2026-10-02)

## Context
R-OPS-7 and R-BUILD-3: a tag-triggered workflow builds everything with Bazel and publishes public
GHCR images and the chart (GITHUB_TOKEN), a GitHub Release (CLI, macOS host package + manifest,
SHA-256 sums, notes) and the Homebrew formula — and nothing may be published during development.
macOS targets compile only on macOS exec (R-BUILD-1, R-XPLAT-8); MDM needs a *signed* package
(R-MAC-9) whose private key belongs in the CI secret store; Bazel output names cannot depend on
the stamped version.

## Decision
* `//release` (rules in `//bazel/release`, tool `//bazel/release/tool`) is the release graph:
  `:macos` (cucinactl darwin/arm64, worker agent darwin, `cucina-hostd` with cgo, the unsigned
  host package + manifests) on a macOS runner, `:linux` (cucinactl Linux musl and Windows,
  worker agents, both images, the chart) on a Linux runner, `:all` (+ SHA256SUMS, formula) on a
  Mac. Windows `cucinactl` stays on the validated `x86_64-pc-windows-gnullvm` release ABI
  (ADR 0103); the newer MSVC foundation fix (ADR 0107) does not silently change that pin.
  Validation reads PE import descriptor names, including ordinal-only dependencies:
  Go's `debug/pe.ImportedLibraries` is unimplemented and cannot enforce this contract.
  Every artifact target is `manual`; `release/build.sh` adds `-c opt --strip=always`.
* Dists carry final asset names (`cucinactl-<v>-<os>-<arch>.tar.gz|zip`, `cucina-<v>.tgz`,
  `cucina-host-<M-m-p>.*` per ADR 0753, …) plus `meta/buildinfo.json`. `cucina-release finalize`
  merges dists only if their build info is identical, then writes SHA256SUMS and the formula.
* CLI archives are deterministic; `cucina-credential-helper` is a hard link in tar.gz and a copy
  in zip (R-AUTH-8). Images: `cucina-controller` and `cucina-sts` (same binary, entrypoint
  `cucina-controller`, the chart passes `sts` as an argument), distroless static nonroot by
  digest, licence notices in `/usr/share/doc/cucina`, one immutable version tag (no `latest`).
  The packaged chart pins `images.controller.{repository,tag,digest}` to the built index (source untouched).
  Package companions include a structural `.manifest.json` (`items[].assets`/metadata), separate
  from the Apple Business form-data `.json`.
* Workflow (`release.yml`): plan → build-linux ‖ build-macos → sign-pkg (tags or explicit signed dry run, `release`
  environment: p12 from secrets into a throwaway keychain, `release/sign-pkg.sh`) → assemble
  (`release/assemble.sh`: verify all, notes) → publish-images (crane push of the *verified* OCI
  layouts, digest must match) → publish-chart (helm push) → github-release → homebrew (final
  versions; `HOMEBREW_TAP_TOKEN`) and a macOS check that the published manifest URL serves the
  package through GitHub's redirect. Provenance via `actions/attest-build-provenance` for images,
  chart and every asset (SHA256SUMS as subject list). Pull requests and manual runs execute the
  same build/assembly scripts as a dry run and skip every publish job; `release/dry-run.sh` runs
  them locally on macOS. Dry-run paths never invoke the publishing entrypoint, even in its
  dry-run mode. The entrypoint additionally requires a clean stamped build matching the
  tag-push workflow's repository and commit; its guard test runs with a synthetic publisher,
  an empty credential environment and a network-blocked sandbox, never on the host.
* Private signing has two certificate roles: a code-signing application leaf and Apple's
  no-EKU installer leaf. The ephemeral CI runner temporarily trusts the installer certificate,
  removes that trust on exit and deletes its keychain even after a partial import failure.
  Workstation signing does not change system trust through release scripts.
* Without a signing identity the tag build fails, unless `vars.CUCINA_RELEASE_WITHOUT_PKG` is
  true (then no host package is published; `make -C macos/pkg pkg-publish` adds it later).
* macOS VM images: only the manual `macos-worker-image.yml` on a self-hosted Mac, protected by
  the `macos-images` environment. The package must already exist and a read-only visibility
  check must positively confirm `private` before any push. Hosts pull with ADR 0702's read-only
  credential. Bootstrapping the package and enabling publication are operator tasks, not part
  of development validation.

## Consequences
A complete release needs a Mac runner; cold cross-toolchain builds are much slower than cached
builds. New GHCR packages start private: the first release needs a one-time visibility change
(the publish step warns). Treat published bytes as immutable; fixing one means a new patch version.
Unsigned Apple packages and example MDM command UUIDs can contain tool-generated timestamps or
random values; reproducible tar/zip/chart/image metadata does not claim bit-reproducible signatures.
