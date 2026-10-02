<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0307 — Private source-image pins and build attestations

* Status: accepted (2026-10-02)

## Context
R-VER requires an identifiable input set. Selecting the newest matching Linux AMI on every build silently changes
the base OS, while checking provider-scoped image identifiers into this public repository violates its environment
hygiene rule. A version label alone also does not prove which binaries or legal documents an image contains.

## Decision
* Linux builds require an exact `source_ami` from the operator's private `source-amis.json`, selected and updated
  deliberately. There is no latest-matching fallback. `SOURCE_AMIS_FILE` selects the lock file; it records region,
  source name/ID, architecture, selection time and the previously observed package versions. Per-family Packer
  variable files prevent concurrent x86/ARM builds from overwriting each other's source selection.
* Windows worker builds select the exact Windows base AMI in the private `amis.json`, rather than silently choosing
  the latest matching image. Rebuilding the Windows base remains a deliberate OS/toolchain update operation.
* AMI tags and the private registry preserve each build's resolved source. Fresh-instance verification additionally
  writes a private attestation containing AMI metadata, installed versions, every shipped legal document's SHA-256
  and byte size, the worker-agent/Buildbarn binary hashes and sizes, and fatal selftest/legal-payload results.
* Both Windows stages and every Linux variant must copy `LICENSE.md`, `THIRD_PARTY_NOTICES.md` and the maintained
  redistributed license texts into the image. Copy verification is byte-for-byte, and missing inputs fail the build.
  Package-manager and vendor-installed license files are retained as well. Pre-fix AMIs are not retrospectively
  marked compliant merely because their recipes were fixed.

## Consequences
Rebuilding a pinned base does not silently move to a new source AMI. Source-ID locks and attestations remain private
under `~/.config/cucina/`; only the selection mechanism and version information belong in the repository.
This is **not complete distro-package reproducibility**: Ubuntu package repositories can change or remove versions,
and the recorded package list is evidence, not an apt snapshot lock. A repository-snapshot or equivalent package
lock is planned; unavailable old AMIs also require an explicit source-pin update rather than a silent fallback.
