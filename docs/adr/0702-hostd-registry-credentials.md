<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0702 — Tart image pull credentials: static read-only package token, short exposure

* Status: accepted (2026-10-02)

## Context
R-MAC-5 / R-OPS-7: hosts pre-pull golden images from the private GHCR package `ghcr.io/sloper-ai/cucina-worker-macos`
with short-lived credentials passed through `TART_REGISTRY_*` environment variables, never the keychain. GHCR accepts
classic PATs or GitHub App installation tokens (1 h) for `read:packages`; a GitHub App needs an app, its private key
and installation management in the controller.

## Decision
`HostService.GetRegistryCredentials` is served through a `RegistryCreds` interface. v1 ships `StaticRegistry`: one
dedicated read-only (`read:packages`) credential from the Secret named by `config.Hosts.RegistrySecret`, returned with an
advisory one-hour `expires_at`. hostd requests it right before each pull, passes it only in the `tart pull` environment
(`TART_REGISTRY_HOSTNAME/USERNAME/PASSWORD`), never stores or logs it, and redacts it from diagnostics. GitHub App
installation tokens (`config.Registry.Mode = "github-app"`) are the upgrade path behind the same interface.

## Consequences
* A leaked host can read the package (the image contains no secrets, UC23); rotate the token in the Secret to revoke.
* Sites with a zot mirror (R-DATA-5) can return mirror credentials from the same interface.
