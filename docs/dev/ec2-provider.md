<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# EC2 provider and cost model

`internal/providers/ec2` implements `ports.Compute` on EC2 (R-POOL-2) with `aws-sdk-go-v2` (config v1.33.6, service/ec2
v1.338.1, service/pricing v1.49.1). `internal/cost` is the pure cost model behind `cucinactl cost` and the cost metrics (R-OBS-5).
Decisions: [ADR 0520](../adr/0520-ec2-launch-runinstances-single-client-token.md) (launch API and idempotency),
[ADR 0521](../adr/0521-ec2-api-throttling-hygiene.md) (throttling), [ADR 0522](../adr/0522-ec2-price-list-filters-and-fallback.md)
(prices).

## Wiring

```go
cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(c.AWS.Region)) // IRSA / Pod Identity / instance profile
compute, err := ec2.New(cfg, ec2.Options{
	ExtraTags:         c.AWS.ExtraTags,         // campaign tags; added to every instance, volume and ENI
	PricingRegionCode: c.AWS.PricingRegionCode, // default: the region
	Limits:            ec2.Limits{RunInstances: ec2.Limit{Burst: c.Autoscaler.RunInstancesBurst, RefillPerSecond: c.Autoscaler.RunInstancesRefill}},
	Clock:             clock, Logger: log,
	OnAPIError:        func(op, code string) { apiErrors.WithLabelValues(op, code).Inc() }, // cucina_ec2_api_errors_total
})
defer compute.Close()
```

`LaunchRequest.Tags` must carry `cucina:cluster` (the installation ID); the provider adds `cucina:managed-by`, `cucina:pool`,
`cucina:generation`, `cucina:launch-token` and (if absent) `cucina:role=worker`, and refuses requests whose tags contradict them.
Tag keys must be usable as instance-metadata tags (`[A-Za-z0-9+-=._:@]`, no `aws:` prefix).

## Behaviour

| Method | Behaviour |
| --- | --- |
| `Launch` | Walks `InstanceTypes` (outer) × `SubnetIDs` with sequential `RunInstances`, one per combination, all with `ClientToken = hex(sha256(cluster ‖ token))`; looks the token up first (`client-token` filter). Capacity/quota errors (incl. `Unsupported`) move on, `IdempotentParameterMismatch` returns the instance created earlier, anything else stops. All combinations failed → `*CapacityError{Tried}` (`errors.Is` `ErrInsufficientCapacity` / `ErrQuotaExceeded`), immediately. Spot: all spot combinations first, then on-demand if `FallbackOnDemand`. |
| | Request: IMDSv2 required, hop limit 1, instance-metadata tags enabled; `InstanceInitiatedShutdownBehavior=terminate`; explicit primary ENI with `AssociatePublicIpAddress = req.AssociatePublicIP` (never inherited from the subnet) and `DeleteOnTermination`; `TagSpecifications` on instance, volume, network-interface (+ spot request); root volume from `RootVolume` (device from the AMI if empty, gp3 default, IOPS/throughput, `VolumeInitializationRate` 100–300 MiB/s); `ExtraVolumes` (auto device names `/dev/sdf`…); **every EBS mapping of the AMI is overridden to `DeleteOnTermination=true`**; user data base64; instance profile by name or ARN. No Elastic IPs. No `DisableApiStop`: EC2's stop protection also refuses `TerminateInstances` (found on real EC2). |
| `Describe` | Always filtered by `cucina:cluster` **and** `cucina:managed-by`; empty cluster → `ErrInvalid`. Optional pool filter; states default pending/running/shutting-down; ID lists use the `instance-id` filter in batches of ≤ 200; every page is read. Pool and generation come from tags. |
| `Terminate` | Describes the IDs (by ID, any state) and refuses (`ErrNotOwned`) any instance without this cluster's `cucina:cluster` + `cucina:managed-by` tags; unknown IDs → `ErrNotFound`; terminated → success. Batches of ≤ 200; a batch refused for a per-resource reason is retried one ID at a time. `UnauthorizedOperation` (the tag-conditioned IAM policy) → `ErrNotOwned`. |
| `ListOrphans` | The cluster's volumes in `available` and unattached ENIs (owner-tag filters). A volume must be older than `OrphanGrace` (default 5 min) and seen unattached for that long; ENIs have no creation time, so their age is how long this process has seen them unattached (a restarted controller waits one grace period). |
| `DeleteOrphans` | Re-describes each orphan: must still exist, carry the owner tags (`ErrNotOwned` otherwise), be unattached and past the grace period; already deleted → success. |
| `ResolveImage` | By ID (any AMI the account can launch; must be `available`) or by tags: the newest available AMI owned by the account (`Owners=self`) carrying every selector tag. Returns arch, platform, snapshots, total size, `cucina:image-version`, `cucina:generation`. |
| `FastLaunch` | `enable`: `EnableFastLaunch` (`ResourceType=snapshot`, `TargetResourceCount`, `MaxParallelLaunches` ≥ 6, launch template `$Default`), skipped if already enabled/enabling with the same settings; waits for `enabled` (first snapshot exists). `disable`: `DisableFastLaunch` unless already disabled/disabling, then waits until the image has left Fast Launch (its snapshots are gone) — safe to deregister afterwards. Waits poll every `FastLaunchPollInterval` (15 s) and are bounded by the caller's context; on expiry the last status is returned with the context error and calling again continues. `Snapshots` counts snapshots tagged `CreatedBy=EC2 Fast Launch` that reference the AMI (best effort; EC2's exact count is the CloudWatch metric `NumberOfAvailableFastLaunchSnapshots`). |
| `InstancePrices` | Price List API (ADR 0522), 24 h cache, embedded us-west-1 table (2026-10-02) when the API is unreachable. Missing types → omitted + `ErrNotFound`. |

Error mapping: capacity (`InsufficientInstanceCapacity`, `InsufficientCapacity`, `InsufficientFreeAddressesInSubnet`,
`Unsupported` during a launch, …) → `ErrInsufficientCapacity`; `VcpuLimitExceeded`, `InstanceLimitExceeded`,
`MaxSpotInstanceCountExceeded`, … → `ErrQuotaExceeded`; `RequestLimitExceeded`/`Throttling`/SDK retry quota → `*ThrottleError`
(`ErrThrottled`, with `Retry-After`); `InvalidAMIID.*` → `ErrImageNotFound`; `*.NotFound` → `ErrNotFound`; permission and
validation errors (`UnauthorizedOperation`, `Invalid*`, `IdempotentParameterMismatch` outside the walk, …) → `ErrInvalid`;
5xx/network errors are returned unwrapped (transient).

Throttling (ADR 0521): token buckets per API class (RunInstances 5 / 2 s⁻¹, Describe* 50 / 10 s⁻¹, Terminate 20 / 5 s⁻¹,
mutations 20 / 5 s⁻¹, pricing 5 / 2 s⁻¹), SDK adaptive retry mode with ICE/quota/validation non-retryable.

## IAM: what the provider calls

| Action | Resources | Condition keys worth using |
| --- | --- | --- |
| `ec2:RunInstances` | `instance/*`, `volume/*`, `network-interface/*` (created) | `aws:RequestTag/cucina:cluster`, `aws:RequestTag/cucina:managed-by`, `aws:RequestTag/<campaign tags>`, `ec2:MetadataHttpTokens=required`, `ec2:InstanceMetadataTags=enabled` |
| `ec2:RunInstances` | `image/*`, `snapshot/*`, `subnet/*`, `security-group/*` (used) | `aws:ResourceTag/…` on AMIs/snapshots, `ec2:Vpc`/`ec2:Subnet` |
| `ec2:RunInstances` + `ec2:CreateTags` | `spot-instances-request/*` (spot pools only) | as above; `ec2:CreateAction=RunInstances` |
| `ec2:CreateTags` | `instance/*`, `volume/*`, `network-interface/*` | `ec2:CreateAction=RunInstances` (tag on create only) |
| `iam:PassRole` | the worker role | `iam:PassedToService=ec2.amazonaws.com` |
| `ec2:DescribeInstances`, `DescribeVolumes`, `DescribeNetworkInterfaces`, `DescribeImages`, `DescribeSnapshots`, `DescribeFastLaunchImages` | `*` (no resource-level permissions) | — |
| `ec2:TerminateInstances` | `instance/*` | `aws:ResourceTag/cucina:cluster=<id>`, `aws:ResourceTag/cucina:managed-by=cucina-controller` |
| `ec2:DeleteVolume`, `ec2:DeleteNetworkInterface` | `volume/*`, `network-interface/*` | same resource-tag conditions |
| `ec2:EnableFastLaunch`, `ec2:DisableFastLaunch` | `image/*` | `aws:ResourceTag/…` on the AMI; enabling also validates that the caller may launch the AMI and the prep launch template, and creates the service-linked role `AWSServiceRoleForEC2FastLaunch` on first use (`iam:CreateServiceLinkedRole`, `iam:AWSServiceName=ec2fastlaunch.amazonaws.com`) |
| `pricing:GetProducts` | `*` | — |

Never needed: `ec2:StopInstances`, `ec2:AllocateAddress`/`AssociateAddress`, `ec2:CreateFleet`, `ec2:CreateLaunchTemplate*`
(ADR 0520).

### Diff against `deploy/aws-e2e/base/iam.tf` (2026-10-02, read-only review)
* Covered: launches (instance/volume/ENI with `cucina:env=e2e` + `cucina:run` + `cucina:expires` request tags — the provider
  satisfies them through `ExtraTags`), tag-on-create, `iam:PassRole` for the worker role, `Describe*`, `pricing:GetProducts`,
  terminate/delete volume/delete ENI on `cucina:env=e2e` resources, Enable/DisableFastLaunch on tagged AMIs.
* **Gap — spot**: `RunInstances`/`CreateTags` on `spot-instances-request/*` are not granted, so spot pools fail with
  `UnauthorizedOperation` (→ `ErrInvalid`). Only matters if a spot pool is configured (spot is a SHOULD).
* **Gap/risk — Fast Launch**: `EnableFastLaunch` checks the caller's permission to launch the AMI with the prep launch template;
  `LaunchRequiresTags` demands `aws:RequestTag/cucina:env`, which that validation may not carry, and `iam:CreateServiceLinkedRole`
  for `ec2fastlaunch.amazonaws.com` is not granted (needed only if the role does not exist yet). Verify on the first Windows AMI.
* Hardening for production (not needed for the campaign): condition the destructive actions on `cucina:cluster` +
  `cucina:managed-by` rather than only `cucina:env`, and require `ec2:MetadataHttpTokens=required` on `RunInstances`.
* `LaunchUsesTaggedResourcesOnly` restricts the controller to AMIs/snapshots tagged `cucina:env=e2e` — intended; the acceptance
  test launches a stock Amazon Linux AMI and therefore runs with the operator profile, not the controller role.

## Tests

| Tier | Command | What |
| --- | --- | --- |
| integration | `go test ./internal/providers/ec2/...` | The adapter against `fakeEC2`/`fakePricing` (`fake_test.go`), a small in-memory model of exactly the `ec2API`/`pricingAPI` surface: zonal client-token idempotency, per-(type, subnet, market) capacity errors, tag filters, pagination, DeleteOnTermination, eventual state changes, Fast Launch states, stop protection refusing termination. |
| unit | `go test ./internal/cost/...` | Example month checked against hand arithmetic + `rapid` properties: monotone in usage, 60 s minimum, zero instances ⇒ only standing cost. |
| acceptance | `CUCINA_EC2_ACCEPTANCE=1 CUCINA_RUN_ID=… CUCINA_EXPIRES=… go test -tags acceptance -run TestAcceptance -v -timeout 20m ./internal/providers/ec2/` | Real EC2 in us-west-1 (two t4g.nano for ~3 min, < $0.01): the walk past an unavailable type, idempotent relaunch, tags on instance/volumes/ENI, IMDS options, Describe by tag, `ErrNotOwned` for another cluster's instance, `CapacityError` for an unavailable type, termination releasing every volume and ENI, image resolution, live prices. Needs the aws-e2e base outputs; a cleanup sweep terminates anything left. |

`porttest.RunCompute` runs against the adapter on `fakeEC2` (`TestComputeConformance`, integration; `ForceICE` and
`ForceThrottle` drive `fakeEC2`) and against real EC2 (`TestAcceptanceConformance`, acceptance; ICE and throttling cannot be
forced there, so those subtests skip). If the real adapter fails it, fix `fakeEC2`. The first real-EC2 run (2026-10-02) showed
that the `RunInstances` response carries no block-device mappings (volumes appear in `DescribeInstances` 1–2 s later); the
suite now awaits volume IDs on `Describe` instead of expecting them from `Launch` (the provider does not wait, which would add
~1.5 s to every launch).

Lessons from real EC2 (2026-10-02): a failed `RunInstances` does not consume its client token; the same token with other
parameters answers `IdempotentParameterMismatch`; the same token after termination returns the terminated instance; a type not
offered in the region answers `Unsupported` in ≈ 0.6 s; `DisableApiStop` blocks termination.

## Cost model (`internal/cost`)

`Model{Rates, InstanceUSDPerHour}.Summarize(Usage, now)` → `Report` (today since 00:00 UTC, month to date, standing per month,
per-pool `PoolCost`, itemised `Line`s, unpriced types, assumptions). Inputs: `Launch` (type, Windows, optional spot price, start/end,
volumes incl. per-launch initialization, public IPv4), `StandaloneVolume`, `Image` (AMI snapshots: current + previous),
`FastLaunch` (pre-provisioned snapshots + prep instances), `Transfer` (bytes per path: inter-AZ ×2 directions, public IP ×2, NAT,
internet egress with the 100 GB/month free tier shared pro rata). Instance-seconds carry the 60 s minimum per launch; monthly rates
use 730 h; transfer GB = 2³⁰ bytes. Standing cost = AMI storage + Fast Launch (`StandingByCategory`), the only EC2 cost at zero
scale (NFR-C1). Line categories match `cucina.v1.CostLine`: compute, ebs, public-ipv4, ami-storage, fast-launch, data-transfer;
`PoolCost.PublicIPv4` has no field in `cucina.v1.PoolCost` yet. `DefaultRates("us-west-1")` holds the 2026-10-02 rates.

### Regenerating the fallback price table
For each type and OS, `aws pricing get-products --region us-east-1 --service-code AmazonEC2 --filters` with the TERM_MATCH
filters of ADR 0522, then write `{"asOf", "source", "regions": {"<region>": [{"type", "linux", "windows", "vcpu", "memoryGiB",
"nvmeGiB"}]}}` into `internal/providers/ec2/fallback_prices.json` (one row per line, sorted by type).
