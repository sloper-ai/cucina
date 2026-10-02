<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->

# ADR 0753: Publishing the host package as immutable GitHub Release assets

* Status: accepted (2026-10-02, agent `pkg`)
* Requirements: R-MAC-8, R-OPS-7

## Context

MDMs need an unauthenticated HTTPS URL per package version plus its SHA-256 and bundle ID (Apple Business: URLs of
lowercase letters, digits and hyphens, one per version, no landing pages). R-OPS-7 makes GitHub Releases the production
location and asks whether Apple Business's downloader follows GitHub's redirect. Installer compares dotted numeric
versions, so SemVer pre-release suffixes would break upgrade ordering.

## Decision

* Package versions are `MAJOR.MINOR.PATCH`; published names are `cucina-host-<MAJOR>-<MINOR>-<PATCH>.{pkg,plist,json}`
  (+ `.pkg.sha256`, the uninstaller, the signer's public certificate).
* `make pkg-publish` uploads to the release `v<version>` without replacing existing assets, or to S3 with
  `If-None-Match: *`; it never changes bucket policies or Block Public Access. It then verifies the manifest and the
  package hash through the public URLs.
* Verified (2026-10-02): a release asset URL answers one `302` to a signed, short-lived (about an hour)
  `release-assets.githubusercontent.com` URL serving `application/octet-stream`. `verify-manifest.sh` (curl) and a
  Foundation `URLSession` probe on macOS 27 both follow it and receive the file; the device-side MDM download uses
  Foundation. **Not verified:** whether Apple Business's own package validation follows the redirect (needs an Apple
  Business account; part of MT-001). Fallback: the S3/CloudFront target.

## Consequences

Release workflows must not re-upload an existing version: fixing a package means a new patch version. The `.pkg`
extension is kept after the hyphenated base name; if Apple Business rejects it, publish the same file without extension.
