<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0014 — D4: macOS hosts run a custom agent that shells out to Tart

* Status: accepted (2026-10-02)

## Context
Orchard is the obvious orchestrator for Tart, but its VMs are ephemeral (a stopped VM is a terminal
state), it has a single non-HA controller, no queue-driven scaling, and leaves you to build your own
image. Cucina needs persistent VMs (their L1 cache survives shutdown), scale driven by the Buildbarn
queue, and one management plane for EC2 and Mac pools.

## Decision
`cucina-hostd` (Go, a root LaunchDaemon on each Mac mini) dials out to the controller (so it works
behind NAT), runs `tart` as a dedicated non-root user, hosts the L2 cache, and executes the
controller's VM commands. We reuse Orchard's patterns (outbound connection, bootstrap token,
privilege drop, the `tart` lifecycle code) but not its server.

## Consequences
* We own the host agent, its enrollment (site token plus serial approval), its packaging and
  signing, and the MDM profiles.
* The root-daemon to `cucina`-user Tart path cannot be exercised on the dev Mac (it runs hostd in
  user mode, and VMs cannot nest macOS VMs): manual check MT-001 covers it.
* At most two macOS VMs run per host (Apple's limit); hostd enforces it as an invariant.
