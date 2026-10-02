<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0522 — Instance prices: Price List filtered by regionCode and operation, dated embedded fallback

* Status: accepted (2026-10-02)

## Context
R-OBS-5 prices instance-seconds with the AWS Price List API (cached). The task text suggests filtering on `location`; the price
list carries a stable `regionCode` attribute next to the display name, and the controller configuration already holds a region code
(`aws.pricingRegionCode`, default the EC2 region). On 2026-10-02 the price list returns three Windows on-demand products per
type (us-west-1, m6id.large): "No License required" / `RunInstances:0002` at $0.23165/h (licence included), "License Included -
Infrastructure" / `RunInstances:0002:box` and BYOL / `RunInstances:0800`, both at the Linux price ($0.13965/h). Air-gapped installs
and unit tests have no Price List access.

## Decision
`InstancePrices` calls `GetProducts` (us-east-1 endpoint, `AmazonEC2`) with TERM_MATCH filters `instanceType`, `regionCode`,
`tenancy=Shared`, `operatingSystem=Linux|Windows`, `preInstalledSw=NA`, `capacitystatus=Used`, `licenseModel=No License required`
and `operation=RunInstances` (Linux) or `RunInstances:0002` (Windows, licence included), parses vCPU, memory and instance-store NVMe
size, and caches answers for 24 h in otter v2. A failed API call opens a 10-minute circuit during which the embedded table is used.
The embedded table (`internal/providers/ec2/fallback_prices.json`, 97 types, us-west-1, "asOf": "2026-10-02", generated from the
same query) answers when the API is unreachable; a type in neither source is left out and reported with `ErrNotFound` next to the
partial result.

## Consequences
Prices for other regions need the API (or a regenerated table). The table is regenerated with the query documented in
`docs/dev/ec2-provider.md` when AWS changes prices; nothing else depends on its date.
