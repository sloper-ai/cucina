<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0018 — D8: Platform identity is the REAPI lexicon plus `xcode-version`; sizes are size classes

* Status: accepted (2026-10-02)

## Context
Buildbarn matches an action's platform properties against a worker's *entire* property set exactly, so
every pool must advertise exactly one set. Machine sizes should not multiply platforms.

## Decision
Platforms use the REAPI platform lexicon: `OSFamily` in {`linux`, `windows`, `macos`} and `ISA` in {`x86-64`,
`arm-a64`, `arm-a32`, `rv64g`}, plus documented Cucina extensions (`s390x`, `cucina-emulation: qemu`,
`xcode-version`, optionally a toolchain version). Pools that differ only in machine size are size classes of one
platform, using Buildbarn's native mechanism. The catalog is `platforms/pools.json`.

## Consequences
* Adding a platform means a new queue, hence a scheduler restart (ADR 0002).
* Bazel `platform()` targets and the predeclared queues are generated from the same catalog, so they cannot disagree
  (ADR 0021).
