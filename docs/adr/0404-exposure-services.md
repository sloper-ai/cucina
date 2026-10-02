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

HostService sessions and their HostFleet state are also process-local, so `worker-controller` selects the leader when
multiple replicas run (both enrollment and host ports; no new load balancer). The internal metrics-only
`controller-leader` Service uses the same selector for `/sd/workers` and `/sd/hosts`. It carries the endpoint label that
excludes it from ordinary component ServiceMonitors; host SD targets retain their owning Pod address and distinct
scrape path. A single replica needs no election label for these routes.

Mac-facing `endpoints.hosts.{storageHost,schedulerHost}` aliases are independent of EC2's private worker addresses.
They default to an explicit `hosts.host`, then the respective worker host; the matching server certificates include
these aliases. Reachability still comes from existing L4/NAT routing or private connectivity, not a new chart-created
load balancer.

## Consequences
More Services than "two endpoints" suggests; documented in docs/operations/exposure.md with the NLB and Traefik settings.
