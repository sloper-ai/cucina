<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0405 — Size profiles as chart data; storage layout derived from one size per store

* Status: accepted (2026-10-02)

## Context
R-CP-7 wants `small`, `medium`, `large` profiles with per-component overrides; R-CP-3 wants block and key-location-map
sizing derived from one size value per store.

## Decision
Profiles live in `files/profiles.yaml`; component values default to null/empty and override the profile when set
(`mergeOverwrite`). Per store: blocks old 8 / current 24 / new 3 (CAS) or 1 (others) / spare 3; key-location map =
`keyLocationMapFactor` (4) × size / `averageObjectSize` (CAS 32 KiB, AC 2 KiB, FSAC 1 KiB, ISCC 512 B) entries of 66 bytes
(the pinned record size). The CAS must be at least 19 GiB so a block (size / 38) holds 512 MiB blobs.

## Consequences
The averages are estimates; `CucinaKeyLocationMapTooSmall` alerts when entries are displaced, and docs/sizing.md records
measured values once the campaign provides them.
