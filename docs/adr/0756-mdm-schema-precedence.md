<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->

# ADR 0756: Prefer Apple's published MDM schema to outdated key names

* Status: accepted (2026-10-02, agent `pkg`)
* Requirements: R-MAC-8, R-MAC-10, setup guide §11

## Context

The requirement prose uses `StealthMode` for the firewall and describes Migration Assistant as unskippable. Apple's
[device-management schema](https://github.com/apple/device-management) at commit
`09f249a06e7e3289930bf6d05f38fb562f748ebf` instead defines `EnableStealthMode`, and includes `Restore` among macOS
Setup Assistant skip keys. The schema also limits declarative package removal to installed files, not postinstall effects.

## Decision

Render `EnableStealthMode` and recommend skipping `Restore` only when the chosen MDM exposes it. Remote Management
itself remains mandatory. Document declarative uninstall's residual state explicitly and prefer the dedicated uninstaller
for full cleanup. Do not substitute undocumented keys to match the prose literally.

Apple Business UI support, assignment ordering and backend redirect handling remain explicitly unverified until MT-001.
A Foundation URLSession/curl download is not evidence about Apple Business's backend, and a manually written preferences
plist is not proof of managed/forced configuration or ADE enrollment.

## Consequences

The intended effects are preserved while payloads follow Apple's actual schema. Template lint and render tests are
necessary but insufficient: first-host MDM delivery, managed login items, pane skipping and power recovery require hardware
and an authenticated MDM organization. Recheck schemas and MT-001 when raising the minimum macOS version.
