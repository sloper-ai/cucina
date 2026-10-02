<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Releasing Cucina

One SemVer version (the `VERSION` file) covers the chart, images, `cucinactl`, the macOS host
package and the agents (R-OPS-7, ADR 0150). A tag `v<VERSION>` runs
[`.github/workflows/release.yml`](../../.github/workflows/release.yml), which builds every artifact
with Bazel (R-BUILD-3), checks that they all carry that version, and publishes them (ADR 0151).
Pull requests run the same workflow as a **dry run** that publishes nothing.

> Nothing has been published yet. The first release is the maintainers' call: see
> [First release checklist](#first-release-checklist). Publishing, tap bootstrap and live
> attestation verification below are operator-only procedures and still need first-release
> validation. Local validation builds/checks artifacts without calling a publisher.

## What a release contains

**GitHub Release** `v<version>` (assets are immutable; a fix is a new patch version):

| Asset | What it is |
| --- | --- |
| `cucinactl-<v>-darwin-arm64.tar.gz` | CLI/TUI for macOS on Apple silicon (built on macOS exec) |
| `cucinactl-<v>-linux-amd64.tar.gz`, `…-linux-arm64.tar.gz` | static musl builds |
| `cucinactl-<v>-windows-amd64.zip` | Windows x86_64, MinGW/UCRT ABI (ADR 0103) |
| `cucina-worker-agent-<v>-<os>-<arch>[.exe]` | worker agent for Linux x86_64/arm64, Windows x86_64, macOS arm64 (input of the image builds: `make -C workers/linux WORKER_AGENT=…`) |
| `cucina-hostd-<v>-darwin-arm64` | the host agent binary (unsigned; install hosts with the package instead) |
| `cucina-<v>.tgz` | the Helm chart package (also in GHCR, below) |
| `cucina-host-<M>-<m>-<p>.pkg` | the **signed** macOS host package (R-MAC-8/-9); version = numeric core of `<v>` |
| `cucina-host-<M>-<m>-<p>.plist` | its MDM manifest (ManifestURL for `InstallEnterpriseApplication` and `com.apple.configuration.package`) |
| `cucina-host-<M>-<m>-<p>.manifest.json` | structural JSON manifest, equivalent to the plist (`items[].assets` and bundle metadata) |
| `cucina-host-<M>-<m>-<p>.json` | the values Apple Business's package form asks for (URL, SHA-256, bundle ID, version); not the structural manifest |
| `cucina-host-<M>-<m>-<p>.pkg.sha256`, `….install-enterprise-application.plist`, `….ddm-package.json` | checksum and example MDM command/declaration |
| `cucina-host-signer-application.pem`, `cucina-host-signer-installer.pem` | public certificates for the two signing roles (both go in the MDM trust profile) |
| `SHA256SUMS` | SHA-256 of every asset above (`sha256sum -c` format) |

Each CLI archive holds `cucinactl`, `cucina-credential-helper` (a hard link — a copy in the zip —
that Bazel runs as its credential helper, R-AUTH-8), `LICENSE.md` and `THIRD_PARTY_NOTICES.md`.

**GHCR (public):** `ghcr.io/sloper-ai/cucina-controller:<v>` and `ghcr.io/sloper-ai/cucina-sts:<v>`
(linux/amd64 + linux/arm64, distroless static nonroot, notices in `/usr/share/doc/cucina`,
`org.opencontainers.image.*` labels; the STS image is the controller binary, run as
`cucina-controller sts`), and the chart `oci://ghcr.io/sloper-ai/charts/cucina` `<v>`, which pins
the controller image by digest. Tags are never moved; there is no `latest`.

**Homebrew:** `brew install sloper-ai/tap/cucinactl` (formula committed to `sloper-ai/homebrew-tap`;
final versions only).

**Provenance:** GitHub build-provenance attestations for every asset, both images and the chart.

macOS **worker VM images** are not released: they go to the *private* package
`ghcr.io/sloper-ai/cucina-worker-macos` (Xcode's licence forbids public redistribution) through the
manual [`macos-worker-image.yml`](../../.github/workflows/macos-worker-image.yml) on a self-hosted
Mac (labels `self-hosted, macOS, ARM64`; Tart, Packer, `gh` and the image's Xcode installed). Protect
its `macos-images` environment with approved refs/reviewers. Before enabling pushes, the operator
must bootstrap the package as **private** and grant the workflow access: every push performs an
API visibility check and refuses a missing, inaccessible or non-private package. Hosts pull
it with a separate read-only package credential that the controller hands to hostd at pull time,
never via profiles or images ([ADR 0702](../adr/0702-hostd-registry-credentials.md)). Linux and
Windows AMIs are built per AWS account with Packer and never published.

## How the workflow runs

```
plan ─┬─ build-linux (ubuntu: //release:linux)  ─┐
      └─ build-macos (macOS: //release:macos) ─ sign-pkg (tags, env `release`) ─┤
                                                              assemble + verify ┘
   tags only: publish-images → publish-chart → github-release → homebrew, verify-published-pkg
```

* **plan** takes the version from the tag (it must equal `VERSION`) or, for dry runs, `0.0.0-dryrun`
  (`workflow_dispatch` accepts another). A pre-release version (`0.2.0-rc.1`) is published as a
  GitHub pre-release and does not update the Homebrew tap.
* **build-*** run `release/build.sh <macos|linux>`: `bazel build --stamp
  --workspace_status_command=release/workspace-status.sh -c opt --strip=always //release:<part>`
  and the release-lane check `//release:verify_<part>_test`.
* **sign-pkg** imports the signing identity from the `release` environment into a throwaway
  keychain (`macos/pkg/scripts/ci-keychain.sh`) and runs `release/sign-pkg.sh` on the exact inputs
  built by `build-macos` (signature checked by `check-pkg.sh`, manifests written for the release URL).
* **assemble** runs `release/assemble.sh`: merges the dists only if they come from the same stamped
  build, writes `SHA256SUMS` and the formula, verifies everything (versions, archive contents,
  binary formats, chart pin, image labels and notices, package version, signer and manifests) and
  generates the notes (`release/notes.sh`: commit subjects since the previous tag and new ADRs,
  both anchored to the built commit). PR and local dry runs never invoke `release/publish.sh`.
* **publish-*** push exactly the verified bytes: `crane push` of the OCI layouts (the pushed digest
  must match), `helm push`, `gh release create --verify-tag`, a commit to the tap. They need
  `CUCINA_PUBLISH=1`, clean stamped source metadata, and a tag-push GitHub Actions context whose
  repository/commit match that metadata. Dirty, unstamped, local and manual-dispatch builds are refused.
* **verify-published-pkg** downloads the published manifest and package the way a Mac does
  (`macos/pkg/scripts/verify-manifest.sh`: HTTPS, following GitHub's redirect) and compares the SHA-256.

Permissions are per job (`contents: read` everywhere; `packages: write` + `id-token`/`attestations:
write` only in the publish jobs; `contents: write` only for the release). Actions are pinned by SHA
(`pinact run`); secrets reach only the jobs that use them, through stdin or the environment.

## Cutting a release

1. On a branch: set `VERSION` to the new version, merge it (CI and the release dry run green).
2. Tag the merge commit and push the tag:
   ```sh
   git tag -s v0.1.0 -m "Cucina 0.1.0" && git push origin v0.1.0
   ```
3. Approve the `release` environment if it has reviewers; watch the run; check the summary.
4. Bump `VERSION` to the next planned version in the next change.

Dry run locally (a complete release requires a macOS host; nothing is published and no
credentials are needed). On Linux, run `release/build.sh linux --out DIR` for that lane only;
`dry-run.sh` refuses an incomplete full release before building anything:

```sh
release/dry-run.sh                    # all parts + assembly + checks, version 0.0.0-dryrun; no publisher calls
release/dry-run.sh --version 0.3.0-rc.1 --out /tmp/rel --lint
BAZEL_STARTUP_ARGS=--output_base=/path BAZEL_ARGS=--jobs=4 release/dry-run.sh   # shared machine
bazel build //release:all             # unstamped (0.0.0-dev), fastbuild: quick check of the graph
```

Local dirty builds are marked in `meta/buildinfo.json` and the image label
`ai.sloper.cucina.source-dirty`; they cannot be published. When validating while others edit,
freeze the source files (not a symlink to a live `.git`) and provide `CUCINA_SOURCE_COMMIT`,
`CUCINA_SOURCE_DIRTY=true`, `SOURCE_DATE_EPOCH` and `CUCINA_SOURCE_REPO` (read-only history for
notes). Every lane must use that same snapshot. A moving commit is deliberately rejected during
assembly.

Sign a package by hand (local keychain; R-MAC-9; both private certificates already trusted
according to docs/mdm/signing.md):

```sh
bazel run --stamp --workspace_status_command=release/workspace-status.sh //release:pkg_sign -- \
  --identity "Cucina Host Application Signing" \
  --installer-identity "Cucina Host Package Signing" --out /tmp/signed
```

## Verifying a release

```sh
gh release download v0.1.0 --repo sloper-ai/cucina --dir rel && cd rel
sha256sum -c SHA256SUMS                          # macOS: shasum -a 256 -c SHA256SUMS
gh attestation verify cucinactl-0.1.0-linux-amd64.tar.gz --repo sloper-ai/cucina
gh attestation verify oci://ghcr.io/sloper-ai/cucina-controller:0.1.0 --repo sloper-ai/cucina
gh attestation verify oci://ghcr.io/sloper-ai/charts/cucina:0.1.0 --repo sloper-ai/cucina
pkgutil --check-signature cucina-host-0-1-0.pkg  # signer = cucina-host-signer-installer.pem
crane config ghcr.io/sloper-ai/cucina-controller:0.1.0 | jq .config.Labels
```

### The package URL and Apple Business

Apple Business needs an unauthenticated HTTPS URL per version, its SHA-256 and the bundle ID
(`ai.sloper.cucina.host`); the values are in `cucina-host-<M>-<m>-<p>.json`. A release asset URL
answers one `302` to a short-lived signed URL on `release-assets.githubusercontent.com`; macOS's
MDM client (Foundation) follows it, and `verify-published-pkg` proves it for every release
(ADR 0753). Whether **Apple Business's own validation** follows the redirect is not documented;
verify it once (part of MT-001):

1. `curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' -o /dev/null --write-out 'status=%{http_code} redirects=%{num_redirects}\n' https://github.com/sloper-ai/cucina/releases/download/v0.1.0/cucina-host-0-1-0.pkg`
   must report status `200` after at least one redirect. Do not print the signed redirect URL's query string.
2. In Apple Business, add the package with that URL, SHA-256 and bundle ID; it must reach a
   valid/ready state without errors.
3. Assign it to a test Mac and confirm the install (`/var/log/install.log`, `pkgutil --pkg-info
   ai.sloper.cucina.host`).
4. If Apple Business rejects the URL, publish the same files to S3/CloudFront instead
   (`make -C macos/pkg pkg-publish TARGET=s3 …`, docs/mdm/pkg.md) and use those URLs.

## One-time setup (operator)

**Signing the host package (R-MAC-9).** Create two private identities with
`macos/pkg/scripts/make-signing-cert.sh`: `--purpose application` (codeSigning EKU) and
`--purpose installer` (Apple's no-EKU recipe). Follow docs/mdm/signing.md and export each with
its key as a password-protected PKCS#12. A codeSigning-only certificate is rejected by
`productbuild`; do not reuse the application identity as the installer identity.

Create the `release` environment with required reviewers and approved refs, then add:

| Secret | Content |
| --- | --- |
| `CUCINA_PKG_SIGNING_P12` | base64 application PKCS#12 |
| `CUCINA_PKG_SIGNING_P12_PASSWORD` | application PKCS#12 password |
| `CUCINA_PKG_INSTALLER_P12` | base64 installer PKCS#12 |
| `CUCINA_PKG_INSTALLER_P12_PASSWORD` | installer PKCS#12 password |

Set repository variable `CUCINA_PKG_SIGNING=true`. The GitHub-hosted runner imports both into
one throwaway keychain, temporarily trusts the installer certificate for `productbuild`,
removes the trust on exit, and deletes the keychain (also after partial import failures).
This trust-changing mode is rejected outside hosted CI; it has not been run on the dev Mac.
MDM must distribute **both public certificates** before the package.

Test the configured signing path without publishing: *Actions → release → Run workflow* with
`sign` checked. Without configured signing a tag build fails, unless
`CUCINA_RELEASE_WITHOUT_PKG=true` (then no host package is published; a later manual upload is
possible only when immutable releases are off and checksums/provenance are regenerated).
Anyone holding these private keys can ship silently trusted code: rotate them per
[rotate-pkg-signing-cert.md](rotate-pkg-signing-cert.md).

**Homebrew tap.** Create the public repository `sloper-ai/homebrew-tap` with a `Formula/`
directory (`gh repo create sloper-ai/homebrew-tap --public --add-readme`). Create a fine-grained
token (or GitHub App token) limited to that repository with *Contents: read and write*, and store
it as `HOMEBREW_TAP_TOKEN` in the `release` environment. The workflow then commits
`Formula/cucinactl.rb` for each final release; users run `brew install sloper-ai/tap/cucinactl`.

**GHCR.** Packages created by the workflow start **private**. After the first release set
`cucina-controller`, `cucina-sts` and `charts/cucina` to *Public* (Organization → Packages →
package → Package settings → Change visibility); the publish jobs warn until anonymous pulls work.
Keep `cucina-worker-macos` private and give hosts only the read-only credential (ADR 0702).
*Cost:* GHCR storage and bandwidth are currently free for public and private packages, but GitHub
may start charging with one month's notice; watch the org's billing page (also docs/sizing.md).

**Repository settings.** Protect `v*` tags (ruleset: only maintainers create, nobody deletes or
moves them) and consider enabling immutable releases.

## Rollback and yanking

* Never delete or move a published tag, release asset or image tag: MDM manifests, the tap and
  `helm` users point at them. Fix forward with a new patch version.
* **Yank a release:** edit the GitHub Release (mark it pre-release, start the notes with a
  warning and the replacement version), and revert the tap's `Formula/cucinactl.rb` commit to the
  previous version. Delete a GHCR package version only if it is harmful (`gh api -X DELETE
  /orgs/sloper-ai/packages/container/<package>/versions/<id>`); the chart pins digests, so a
  deleted image breaks installs of that chart version.
* **Roll back a deployment:** `helm rollback <release> <revision>` or `helm upgrade --version
  <previous>` (docs/operations/helm-upgrade-rollback-uninstall.md); `cucinactl`: install the
  previous archive or `brew` formula; Mac hosts: point MDM at the previous version's manifest URL
  (downgrades work, Tart included; ADR 0750).

## First release checklist

- [ ] `main` green in CI; the release workflow's dry run green on the release PR, and
      `release/dry-run.sh` green on a Mac.
- [ ] Required manual checks signed off for this release (docs/testing/manual, R-TEST-9; MT-001
      covers the Apple Business URL check above).
- [ ] `VERSION` is `0.1.0` on the commit to tag; `THIRD_PARTY_NOTICES.md` regenerated
      (tools/notices/generate.sh).
- [ ] Environment `release` with both application/installer PKCS#12 secrets and passwords
      listed above, plus `HOMEBREW_TAP_TOKEN`; variable `CUCINA_PKG_SIGNING=true`.
- [ ] Signed manual dry run passes, both public signer certificates installed through MDM.
- [ ] `sloper-ai/homebrew-tap` exists with `Formula/`; org settings allow public packages;
      `v*` tag ruleset in place.
- [ ] `git tag -s v0.1.0 -m "Cucina 0.1.0" && git push origin v0.1.0`; approve `release`.
- [ ] After the run: make the three GHCR packages public; `helm pull
      oci://ghcr.io/sloper-ai/charts/cucina --version 0.1.0`; `docker pull` both images without
      login; `brew install sloper-ai/tap/cucinactl && cucinactl --version`; verify checksums and
      attestations (above); add the package to Apple Business and check the URL (above).
- [ ] Bump `VERSION` to the next version.
