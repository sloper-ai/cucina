<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0401 — In-cluster Buildbarn hops are plaintext, guarded by NetworkPolicies

* Status: accepted (2026-10-02)

## Context
Frontend → storage shards carries all CAS/AC traffic (P3: in-cluster, uncompressed); frontend → scheduler client port
and scheduler → storage are in-cluster too. UC22 requires TLS and authentication on every endpoint reachable from outside
the cluster; these ports are not. mTLS on P3 would add a client certificate per frontend, CPU on the hottest path and more
rotation surface.

## Decision
Storage shards and the scheduler's client port listen in plaintext; storage authorizers are `allow` (the frontend already
authorized the caller), the scheduler re-validates the forwarded JWT (R-AUTH-4). NetworkPolicies (on by default) admit the
storage gRPC port only from frontend and scheduler pods, the scheduler client port only from frontends and BuildQueueState
only from the controller. BuildQueueState itself is mTLS with the controller identity (R-SEC-4).

## Consequences
On clusters that do not enforce NetworkPolicies, any pod can read and write the storage shards directly. The upgrade path
is mTLS with `pki.BuildbarnStorageFromFrontendValidation` (already specified) when a deployment needs it.
