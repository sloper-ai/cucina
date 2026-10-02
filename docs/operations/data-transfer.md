<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Data transfer, cost and client guidance

A remote build moves a lot of bytes: inputs to workers, outputs back, toolchains, images. Where they flow decides most of the network bill and much of the speed. This page explains the topology that keeps bulk traffic free, what each path costs, what clients should set, and where to place CI.
The prices are AWS's for us-west-1 as verified on 2026-10-01; prices change, so re-check the AWS pricing pages before you decide something on them.

## Principles

1. **Never move a byte twice.** Content addressing, deduplication at every tier, and read-through caches that merge concurrent fetches of the same blob.
2. **Move only the bytes that are read.** Clients use "build without the bytes" and the remote repository contents cache; workers use virtual build directories that fetch only what an action opens.
3. **Keep bulk traffic free.** One Availability Zone, private addresses, no NAT gateway, no public-IP hairpins, no cross-zone load balancing.
4. **Compress only where bytes cost money or bandwidth is scarce:** between clients and the cluster, and over the Mac WAN link. Never inside the AZ, where it would only cost CPU.
5. **Measure every path in bytes and dollars.**

## The topology that makes this work

This is the production default, and the test environment follows it.

* **One AZ** for the workers, the storage and the control plane's worker-facing endpoints. In-AZ traffic between private addresses is free.
* **Workers live in a private subnet of a dual-stack VPC.** They reach AWS's management endpoints (SSM, ECR) over IPv6 through an **egress-only internet gateway** (free), and reach S3 (ECR image layers) through an **S3 gateway endpoint** (free). There is **no NAT gateway**.
* **Workers reach the control plane over private IPs only:** never the node's public IP, never an internet-facing load balancer, and no cross-zone load balancing. The worker endpoint is reachable only from the VPC and from the Mac sites.
* **Fallback when an IPv6 path is missing** (some endpoints, or an IPv6-only Windows limitation): give those workers an auto-assigned public IPv4 address, which costs about $0.005 per running worker-hour and is released at termination. A NAT gateway ($0.048 per hour plus $0.048 per GB processed) only beats that above roughly ten workers running on average, so use it only past that break-even,
  and record the decision in an ADR.

## What each path carries and costs

| Path | What flows | How bytes are kept down | Cost |
| --- | --- | --- | --- |
| **P1** Client to cluster | Sources and generated inputs; local results from writers only | `FindMissingBlobs` dedupe per command; ByteStream with zstd (about level 3, blobs of 100 bytes and up). **Toolchains are not uploaded per client:** the remote repository contents cache holds them. Read-only clients set `--remote_upload_local_results=false` | Ingress is free |
| **P2** Cluster to client | The outputs the user needs | Build without the bytes (`minimal` on CI; `toplevel` plus `--remote_download_regex` for developers); zstd; a local disk cache with garbage collection; the repository contents cache means clients fetch only trees and `.bzl` files, not toolchain bytes | Internet egress $0.09 per GB (the first 100 GB per month are free); **free for CI in the same AZ** |
| **P3** Frontend to storage | All CAS and AC traffic | In-cluster, uncompressed; `existenceCaching` of about a minute (never longer than retention) cuts `FindMissingBlobs` fan-out; `completenessChecking` at the lowest layer | Free |
| **P4** Storage to EC2 workers | Action inputs | Virtual build directories fetch only the bytes read (measured: about 5 % of a 2.8 GB toolchain directory per Abseil compile); prefetching from the file system access cache; L1 `readCaching` with a deduplicating replicator. Private IPs, same AZ, uncompressed | Free. Going through a public IP would cost $0.01 per GB each way and is forbidden |
| **P5** EC2 workers to storage | Outputs | Written once to the central CAS | Free |
| **P6** Central to Mac sites (the WAN) | Inputs to macOS VMs, outputs back | A per-host L2 cache (optionally a site tier) with a deduplicating replicator; zstd on the WAN hop at encoder level 3 (about zstd 7 to 8; measured to save 5 to 12 % more than level 2 on C++ artifacts); each blob crosses the WAN once per site | AWS to site $0.09 per GB; site to AWS free |
| **P7** Worker images | AMIs, Tart images | AMIs stay in their region. Tart images are pulled once per site (an optional registry mirror) and only changed 512 MiB chunks download; stacked images make updates incremental | The private GHCR package is free today; see below |
| **P8** Worker management traffic | SSM, ECR API, AWS APIs | The egress-only gateway and the S3 endpoint above | About zero; the public-IPv4 fallback costs $0.005 per running worker-hour |
| **P9** Metrics and logs | Small | In-VPC scraping; hostd relays Mac metrics | Negligible |

The one number to remember: **bytes inside the AZ are free, and bytes that leave AWS cost $0.09 per GB** (after the free tier), so the way to a small bill is to keep consumers of large outputs inside the AZ and to stop clients downloading what they never read.

## Client settings

`cucinactl bazelrc` emits these (Bazel 9.2 flag names); do not hand-edit them away without knowing which path they protect.

| Setting | Why |
| --- | --- |
| `startup --experimental_remote_repo_contents_cache` | Toolchains and external repositories are stored once as trees; clients overlay them instead of downloading or extracting them (P1, P2) |
| `--remote_cache_compression` | zstd on the client link (the frontend advertises it; Bazel fails without it) |
| `--remote_download_outputs=minimal` for CI; `toplevel` plus `--remote_download_regex` for developers | Build without the bytes (P2) |
| `--disk_cache=<dir>` with `--experimental_disk_cache_gc_max_size` | A bounded local cache |
| `--experimental_remote_cache_eviction_retries=5` and `--rewind_lost_inputs` | Recovery when retention is exceeded ([storage full or retention too short](storage-full-retention.md)) |
| `--remote_build_event_upload=minimal`, `--nolegacy_important_outputs` | Less metadata and fewer needless transfers |
| `--remote_upload_local_results=false` for read-only principals | Avoids deny and refresh loops and useless uploads |
| `--remote_max_connections` and `--remote_max_concurrency_per_connection` | Bazel 9.2 uses ByteStream for every blob (never the batch RPCs), so concurrency comes from these two, not from batch sizes |

Keep `--experimental_remote_cache_ttl` (default three hours) below the storage retention, and never set `--experimental_remote_cache_chunking`: Buildbarn answers Unimplemented. The repository contents cache needs a **trusted writer** that seeds each toolchain version once (CI on `main`, holding `ac-write` for a dedicated scope); everyone else only reads. Keys include the client host's
OS and CPU, so seed once per client platform (Linux x86_64, macOS arm64, Windows x86_64); the blobs still deduplicate in the CAS.

## Where to run CI

* **Self-hosted runners in the cluster's AZ make P2 free.** They pull outputs over private addresses.
* **GitHub-hosted runners pay internet egress** for everything they download. Use `--remote_download_outputs=minimal` and compression, and let only the artifacts you really need leave the cluster.
* Authenticate CI with GitHub's OIDC ([security](../security.md)); no secrets are stored in GitHub.

## Optional: CloudFront in front of the client endpoint

Not enabled by default. A distribution can lower client egress cost: the first terabyte per month is free and bandwidth beyond it is about 5.6 % cheaper than direct egress. It also has costs: uploads through it are billed (about $0.02 per GB), and gRPC responses are not cacheable, so it is only a cheaper pipe, not a cache.
Before recommending it for your deployment, **verify that Execute's streaming updates (about every 60 seconds) survive the origin and idle timeouts**. Measure before you adopt it.

## Optional: per-AZ L2 caches for multi-AZ pools

If you run pools in more than one AZ, workers in the second AZ would otherwise fetch from central storage across the AZ boundary and pay inter-AZ transfer. An L2 cache in each additional AZ avoids that: each blob crosses the boundary once per AZ. It is a **standing cost** (an always-on node and volume per AZ), so it is off by default; adopt it
only when measured inter-AZ bytes cost more than the cache does. The default and the cheapest design is one AZ.

## GHCR is free today, and may not stay free

Cucina's macOS images live in a private GitHub Container Registry package, and the chart and Cucina's public images are planned for GHCR as well. GHCR storage and bandwidth are currently free, even for private images, but GitHub may begin charging with a month's notice. Treat registry bandwidth as a possible future line item: Tart images are large, which is why hosts pull
each image once per site (optionally through a mirror) and only changed chunks download.

## Measuring

* **Bytes per path**: Buildbarn's blob-store metrics by backend label, hostd's WAN counters, and the controller's estimate of worker traffic.
* **Dollars**: the controller converts bytes to dollars with the AWS price list (egress, inter-AZ, NAT, public IPv4, ECR and S3 storage); see the data-transfer lines of `cucinactl cost` and the TUI's cost view. An egress anomaly raises an alert.
* **A standing check** that the design holds: no NAT gateway exists (`aws ec2 describe-nat-gateways`), workers on the private path have no public IPv4, and cross-AZ bytes are about zero ([cost leak](cost-leak.md)).
