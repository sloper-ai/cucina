<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0404 — Exposure: one Service per endpoint and backend; management goes to the leader

* Status: accepted (2026-10-02)

## Context
R-CP-5 names one client and one worker endpoint, but each is served by several workloads (worker endpoint: frontend worker
listener, scheduler worker API, controller enrollment and host APIs). A Service selects one pod set. The autoscaler state
behind the management API exists only on the elected controller.

## Decision
Render one Service per (endpoint, backend): `client` (frontend), `api-sts`, `api-management` (selects the Pod the leader
labels `cucina.sloper.ai/leader=true`), `worker-storage`, `worker-scheduler`, `worker-controller`, each of the configured
type. On k3s ServiceLB they share the node addresses; on EKS each is an NLB unless the operator sets per-service hosts
(`endpoints.worker.*Host`). Ingress/Gateway (TLS at the proxy, h2c to the frontend) apply to the remote-execution endpoint
only; the worker endpoint stays L4 because mTLS needs pass-through.

## Consequences
More Services than "two endpoints" suggests; documented in docs/operations/exposure.md with the NLB and Traefik settings.
