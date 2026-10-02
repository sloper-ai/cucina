<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Exposing Cucina's endpoints

Cucina has two endpoints (R-CP-5), kept apart for network segmentation:

* the **client endpoint** — remote execution and cache for Bazel (gRPC, TLS + Cucina JWT), plus the STS (HTTPS) and the
  management API (gRPC, TLS + JWT) for `cucinactl`; reachable from developer machines and CI;
* the **worker endpoint** — the frontend's worker/host listener (CAS/AC/FSAC), the scheduler's worker API, enrollment and
  the Mac host API, all mTLS with Cucina's CA except enrollment (TLS); reachable **only** from the VPC and the Mac sites.

Every externally reachable port is TLS; unauthenticated requests are rejected (UC22). In-cluster hops (frontend ↔
storage, frontend ↔ scheduler client port) never leave the cluster (ADR 0401).

## Services

A Kubernetes Service selects one set of pods, so each endpoint is several Services of the same type (ADR 0404):

| Service | Port (value) | Backend | Protocol | Clients |
| --- | --- | --- | --- | --- |
| `<release>-client` | 443 (`endpoints.client.port`) | frontend `:8980` | gRPC, TLS + JWT (h2c behind an Ingress/Gateway) | Bazel |
| `<release>-api-sts` | 8443 (`endpoints.sts.port`) | STS `:8443` | HTTPS | `cucinactl login`, credential helper |
| `<release>-api-management` | 8444 (`endpoints.management.port`) | **leader** controller `:8444` | gRPC, TLS + JWT | `cucinactl` |
| `<release>-worker-storage` | 8981 (`endpoints.worker.storagePort`) | frontend `:8981` | gRPC, mTLS | workers, Mac host L2 caches |
| `<release>-worker-scheduler` | 8983 (`endpoints.worker.schedulerPort`) | scheduler `:8983` | gRPC, mTLS | workers (Mac VMs through their host's relay) |
| `<release>-worker-controller` | 8445 / 8446 (`endpoints.worker.enrollmentPort`, `endpoints.hosts.port`) | **leader** controller `:8445`, `:8446` | gRPC TLS (enrollment), mTLS (hosts) | EC2 workers at boot, Mac hosts |
| `<release>-controller-leader` | 9090 (in-cluster only) | **leader** controller metrics listener | HTTP service discovery | Prometheus `/sd/workers`, `/sd/hosts` |

`exposure.client.type` (LoadBalancer, NodePort, ClusterIP, Ingress, Gateway) applies to `client`;
`exposure.api.type` and `exposure.worker.type` (LoadBalancer, NodePort, ClusterIP) to the others. The addresses the
controller hands to workers and clients come from `endpoints.*` (set them to the load balancers' names or IPs).

With multiple controllers, host/enrollment traffic and HTTP discovery select `cucina.sloper.ai/leader=true`: the
scaler's HostFleet sessions live in that process, not on standbys. A single controller needs no leader label for these
Services. The existing worker-controller load balancer is reused; controller-leader is ClusterIP only. The latter has
an endpoint label so the component ServiceMonitor does not scrape the same controller a second time.

## Mac sites outside the VPC

Keep `endpoints.worker.{host,storageHost,schedulerHost,enrollmentHost}` private: those are the addresses EC2 workers
receive. Mac hosts can use separate, already-routed hostname/IP aliases without changing EC2 traffic:

```yaml
endpoints:
  worker:
    host: worker.internal.example.com
  hosts:
    host: hosts.example.com
    # Optional when one hosts.host address routes all three ports:
    storageHost: storage.example.com
    schedulerHost: scheduler.example.com
```

`hosts.storageHost` and `hosts.schedulerHost` default to an explicitly set `hosts.host`, otherwise to their respective
worker hosts. They use `worker.storagePort` and `worker.schedulerPort`. The controller advertises these Mac-specific
addresses in the HostService welcome; the chart adds their DNS/IP SANs to the frontend-worker and scheduler
certificates. Existing-Secret TLS must carry those SANs too. EC2 storage/scheduler addresses are never replaced.

Aliases do not provision a network path. Publish the existing mTLS listener ports through the deployment's L4/NAT
routing, or provide VPN/private connectivity, and restrict sources to the Mac sites. Do not change EC2 endpoints to
public addresses to make Mac access work: that causes avoidable public-IP/NAT traffic.

## STS transport aliases

`endpoints.sts.url` remains the canonical JWT issuer. Set `endpoints.sts.aliases` to an explicit allowlist of alternative
HTTPS origins when clients inside the VPC must exchange tokens without a public-endpoint round trip:

```yaml
endpoints:
  sts:
    url: https://auth.example.com:8443
    aliases: [https://auth.internal.example.com:8443]
```

Only HTTPS origins are accepted (an optional trailing `/`, but no credentials, other path, query or fragment). The
chart adds each alias hostname/IP to STS TLS SANs, including the controller's STS listener; existing Secrets must
already cover them. Discovery contacted through an allowlisted inbound Host returns token/JWKS URLs on that origin;
the discovery issuer and signed JWT issuer do not change. Proxies must preserve Host: forwarded-host headers do not
select an alias. Empty aliases leave the previous configuration unchanged.

**Idle timeouts.** Workers long-poll the scheduler for up to 2 minutes and Bazel keeps Execute streams open for the
whole action; every load balancer, NAT and proxy on these paths must allow idle connections longer than 2 minutes (AWS NLB:
350 s default; Traefik: see below). Bazel 9 pings every 60 s; the frontend and scheduler accept pings every 20 s
(`keepaliveEnforcementPolicy`), so Bazel never gets GOAWAY.

## k3s: ServiceLB (the test environment)

`type: LoadBalancer` with k3s' built-in ServiceLB (klipper-lb) publishes every Service port on the node's addresses
(`ci/values-small.yaml`). Workers must use the node's **private** IP (`endpoints.worker.host`), clients the public name
or Elastic IP (`endpoints.client.host`). Restrict sources with security groups and `loadBalancerSourceRanges`
(ServiceLB enforces them). `exposure.worker.externalTrafficPolicy: Local` keeps worker traffic on the node it arrives at.

ServiceLB binds each port once per node: k3s' bundled Traefik already holds 80/443. Either install k3s with
`--disable traefik` (the acceptance environment does; nothing in Cucina needs an ingress there) or move the client
endpoint off 443 (`endpoints.client.port: 8980`).

## AWS: Network Load Balancers (AWS Load Balancer Controller)

```yaml
exposure:
  client:
    type: LoadBalancer
    loadBalancerClass: service.k8s.aws/nlb
    annotations:
      service.beta.kubernetes.io/aws-load-balancer-scheme: internet-facing
      service.beta.kubernetes.io/aws-load-balancer-nlb-target-type: ip
    loadBalancerSourceRanges: [203.0.113.0/24]          # office / CI egress ranges
  api: { … same as client … }
  worker:
    type: LoadBalancer
    loadBalancerClass: service.k8s.aws/nlb
    externalTrafficPolicy: Local
    annotations:
      service.beta.kubernetes.io/aws-load-balancer-scheme: internal
      service.beta.kubernetes.io/aws-load-balancer-nlb-target-type: ip
      service.beta.kubernetes.io/aws-load-balancer-attributes: load_balancing.cross_zone.enabled=false
```

* Each Service becomes its own NLB; set `endpoints.worker.storageHost`, `schedulerHost` and `enrollmentHost` to their DNS
  names (and `endpoints.worker.serverName` to one name the internal certificate carries, so workers verify a stable name).
* Keep NLBs, nodes and workers in **one AZ** with cross-zone load balancing off (R-DATA-4): in-AZ traffic is free.
* The NLB TCP idle timeout (350 s) exceeds the 2-minute long-poll; if you lower it (listener attribute
  `tcp.idle_timeout.seconds`), keep it above 150 s.
* Workers reach the internal NLB over private IPs only; never point them at an internet-facing load balancer (R-DATA-4).

## NodePort

`type: NodePort` with fixed `nodePorts` (`ci/values-medium.yaml` does this for the worker endpoint) when an external L4
load balancer or a firewall in front of the nodes forwards the ports. TLS still terminates in the pods.

## Ingress (remote-execution endpoint only)

With `exposure.client.type: Ingress` the Ingress terminates TLS (its certificate: `exposure.client.ingress.tlsSecretName`,
e.g. from cert-manager) and forwards HTTP/2 cleartext (h2c) to the frontend, whose client listener then serves without
TLS (JWT authentication is unchanged). The frontend Service port carries `appProtocol: kubernetes.io/h2c`.

**Traefik** (k3s' default ingress) needs two settings, or it breaks gRPC:

1. h2c to the backend — annotate the backend Service:

   ```yaml
   exposure:
     client:
       type: Ingress
       ingress: {className: traefik, tlsSecretName: cucina-ingress-tls}
       backendServiceAnnotations:
         traefik.ingress.kubernetes.io/service.serversscheme: h2c
   ```

2. no read timeout on the entrypoint — Traefik's default `respondingTimeouts.readTimeout` (60 s) kills long Execute
   streams. On k3s, configure the bundled Traefik with a `HelmChartConfig`:

   ```yaml
   apiVersion: helm.cattle.io/v1
   kind: HelmChartConfig
   metadata: {name: traefik, namespace: kube-system}
   spec:
     valuesContent: |-
       ports:
         websecure:
           transport:
             respondingTimeouts: {readTimeout: 0, writeTimeout: 0, idleTimeout: 600s}
   ```

**ingress-nginx**: `nginx.ingress.kubernetes.io/backend-protocol: GRPC` plus long `proxy-read-timeout` /
`proxy-send-timeout` (≥ 3600 s; Bazel's longest actions) and `client-body-timeout`.

The STS and the management API keep their own Services (`exposure.api`): they terminate TLS in the pods.

## Gateway API

`exposure.client.type: Gateway` renders a `GRPCRoute` attached to `exposure.client.gateway.parentRefs` for the host
`endpoints.client.host`; the Gateway's HTTPS listener terminates TLS and reaches the frontend over h2c (the Service port's
`appProtocol: kubernetes.io/h2c`). Make sure the Gateway implementation's stream/idle timeouts exceed the longest action.

## What must never be exposed

The storage shards (`<release>-storage`), the scheduler's client port (`:8982`) and BuildQueueState (`:8984`) are
in-cluster only; the chart's NetworkPolicies (on by default) admit them only from the frontend, scheduler and controller.
The Pushgateway (optional) is ClusterIP; expose it only behind authentication.
