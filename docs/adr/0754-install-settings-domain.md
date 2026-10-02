<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->

# ADR 0754: Separate preference domain for the package's install settings

* Status: accepted (2026-10-02, agent `pkg`)
* Requirements: R-MAC-10

## Context

hostd validates its managed preferences (`ai.sloper.cucina.hostd`) strictly: an unknown key makes it exit with code 2.
The package's postinstall needs a few switches of its own (create the user, manage auto-login, restart after the first
install, Local Network escape hatch) that hostd must never see.

## Decision

The package reads its switches from the preference domain **`ai.sloper.cucina.host`** (the package identifier), managed
(`/Library/Managed Preferences/ai.sloper.cucina.host.plist`) or local, with defaults that need no configuration:
`CreateUser` (true), `ManageAutoLogin` (true), `RestartAfterFirstInstall` (true), `LocalNetworkAllowedEthernetAddresses`
(unset). Profile 02 carries both domains in one `com.apple.ManagedClient.preferences` payload. The only hostd key the
package reads is `RunAsUser` (read-only).

## Consequences

hostd's schema stays strict and owned by hostd; the package can evolve its switches independently. Two domains to know
about, documented in docs/mdm/pkg.md and docs/mdm/profiles.md.
