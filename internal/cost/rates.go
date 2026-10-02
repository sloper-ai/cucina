// SPDX-License-Identifier: FSL-1.1-ALv2

package cost

// HoursPerMonth converts monthly rates (GB-month, IOPS-month, …) into per-second
// rates, as AWS's own price calculator does.
const HoursPerMonth = 730

// Rates are the non-instance unit prices of one region in USD. Instance prices come
// from ports.Compute.InstancePrices (the AWS Price List API) and are passed to the
// Model separately.
type Rates struct {
	Region string
	// AsOf dates the snapshot of the price list these rates come from.
	AsOf string

	// EBS volumes, per GB-month by volume type (gp3 is the default, NFR-C4).
	VolumeGBMonth map[string]float64
	// gp3 performance above the free baseline (3000 IOPS, 125 MiB/s).
	GP3IOPSMonth       float64 // per provisioned IOPS-month above GP3FreeIOPS
	GP3ThroughputMonth float64 // per MiB/s-month above GP3FreeThroughput
	GP3FreeIOPS        int
	GP3FreeThroughput  int
	// Provisioned Rate for Volume Initialization, per GB of snapshot data initialized,
	// for rates up to InitRateTierMiBps and above it (billed per launch, R-POOL-2).
	InitRateLowGB     float64
	InitRateHighGB    float64
	InitRateTierMiBps int

	// EBS snapshots (AMIs and Fast Launch pre-provisioned snapshots), per GB-month.
	SnapshotGBMonth float64
	// In-use public IPv4 address, per hour.
	PublicIPv4Hour float64

	// Data transfer, per GB.
	InterAZGB        float64 // charged in each direction: a byte crossing AZs pays twice
	PublicIPGB       float64 // same-region traffic through public IPv4, each direction
	NATGB            float64 // NAT gateway data processing
	InternetEgressGB float64 // to the internet (first tier)
	// InternetFreeGBMonth is the monthly internet egress free tier (account-wide).
	InternetFreeGBMonth float64
}

// DefaultRates returns the built-in rates for region (ok=false if unknown). The
// us-west-1 values were read from the AWS Price List API on 2026-10-02 (AmazonEC2
// Storage/System Operation/Provisioned Throughput/Storage Snapshot/
// ProvisionedRateVolumeInitialization/NAT Gateway, AmazonVPC PublicIPv4:InUseAddress)
// and the data-transfer table of PROMPT §6.14.
func DefaultRates(region string) (Rates, bool) {
	switch region {
	case "us-west-1":
		return Rates{
			Region:              region,
			AsOf:                "2026-10-02",
			VolumeGBMonth:       map[string]float64{"gp3": 0.096, "gp2": 0.12},
			GP3IOPSMonth:        0.006,
			GP3ThroughputMonth:  0.048, // $49.152 per GiBps-month
			GP3FreeIOPS:         3000,
			GP3FreeThroughput:   125,
			InitRateLowGB:       0.0029,
			InitRateHighGB:      0.0043,
			InitRateTierMiBps:   200,
			SnapshotGBMonth:     0.055,
			PublicIPv4Hour:      0.005,
			InterAZGB:           0.01,
			PublicIPGB:          0.01,
			NATGB:               0.048,
			InternetEgressGB:    0.09,
			InternetFreeGBMonth: 100,
		}, true
	}
	return Rates{}, false
}
