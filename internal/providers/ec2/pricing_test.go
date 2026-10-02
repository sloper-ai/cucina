// SPDX-License-Identifier: FSL-1.1-ALv2

package ec2

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/ports"
)

// Price List product documents as GetProducts returns them (trimmed to the fields
// the provider reads plus a few it must ignore; captured 2026-10-02, us-west-1).
const (
	m6idLinuxDoc   = `{"product":{"productFamily":"Compute Instance","attributes":{"instanceType":"m6id.large","vcpu":"2","memory":"8 GiB","storage":"1 x 118 NVMe SSD","operatingSystem":"Linux","licenseModel":"No License required","operation":"RunInstances","regionCode":"us-west-1","tenancy":"Shared"}},"terms":{"OnDemand":{"YU3N3EM67VD82727.JRTCKXETXF":{"priceDimensions":{"YU3N3EM67VD82727.JRTCKXETXF.6YS6EN2CT7":{"unit":"Hrs","pricePerUnit":{"USD":"0.1396500000"},"description":"$0.13965 per On Demand Linux m6id.large Instance Hour"}}}}}}`
	m6idWindowsDoc = `{"product":{"productFamily":"Compute Instance","attributes":{"instanceType":"m6id.large","vcpu":"2","memory":"8 GiB","storage":"1 x 118 NVMe SSD","operatingSystem":"Windows","licenseModel":"No License required","operation":"RunInstances:0002","regionCode":"us-west-1","tenancy":"Shared"}},"terms":{"OnDemand":{"YU3N3EM67VD82727.JRTCKXETXF":{"priceDimensions":{"YU3N3EM67VD82727.JRTCKXETXF.6YS6EN2CT7":{"unit":"Hrs","pricePerUnit":{"USD":"0.2316500000"},"description":"$0.23165 per On Demand Windows m6id.large Instance Hour"}}}}}}`
	bigMemDoc      = `{"product":{"attributes":{"instanceType":"x2iedn.32xlarge","vcpu":"128","memory":"4,096 GiB","storage":"2 x 1900 NVMe SSD"}},"terms":{"OnDemand":{"A.B":{"priceDimensions":{"A.B.C":{"unit":"Hrs","pricePerUnit":{"USD":"26.6800000000"}}}}}}}`
)

// R-OBS-5 / R-POOL-2: instance prices come from the Price List API (vCPU, memory,
// NVMe parsed; Windows includes the licence), are cached with a TTL, and fall back
// to the embedded dated table when the API is unreachable.
func TestInstancePrices(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.price.docs["m6id.large|Linux"] = []string{m6idLinuxDoc}
	e.price.docs["m6id.large|Windows"] = []string{m6idWindowsDoc}
	e.price.docs["x2iedn.32xlarge|Linux"] = []string{bigMemDoc}

	got, err := e.p.InstancePrices(ctx, []string{"m6id.large", "x2iedn.32xlarge"}, false)
	require.NoError(t, err)
	assert.Equal(t, map[string]ports.InstancePrice{
		"m6id.large":      {Type: "m6id.large", USDPerHour: 0.13965, VCPU: 2, MemoryGiB: 8, NVMeGiB: 118},
		"x2iedn.32xlarge": {Type: "x2iedn.32xlarge", USDPerHour: 26.68, VCPU: 128, MemoryGiB: 4096, NVMeGiB: 3800},
	}, got)

	win, err := e.p.InstancePrices(ctx, []string{"m6id.large"}, true)
	require.NoError(t, err)
	assert.Equal(t, ports.InstancePrice{Type: "m6id.large", USDPerHour: 0.23165, VCPU: 2, MemoryGiB: 8, NVMeGiB: 118, Windows: true}, win["m6id.large"])

	calls := e.price.calls
	_, err = e.p.InstancePrices(ctx, []string{"m6id.large"}, false)
	require.NoError(t, err)
	assert.Equal(t, calls, e.price.calls, "served from the cache")
	e.clock.Advance(25 * time.Hour)
	_, err = e.p.InstancePrices(ctx, []string{"m6id.large"}, false)
	require.NoError(t, err)
	assert.Equal(t, calls+1, e.price.calls, "refetched after the TTL")
}

func TestInstancePricesFallBackToTheEmbeddedTable(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.price.err = apiError("ServiceUnavailableException")

	got, err := e.p.InstancePrices(ctx, []string{"t4g.nano", "c7i.8xlarge", "z9.huge"}, false)
	assert.ErrorIs(t, err, ports.ErrNotFound, "a type in neither source is reported")
	assert.Equal(t, ports.InstancePrice{Type: "t4g.nano", USDPerHour: 0.005, VCPU: 2, MemoryGiB: 0.5}, got["t4g.nano"])
	assert.Equal(t, 32, got["c7i.8xlarge"].VCPU)
	assert.NotContains(t, got, "z9.huge")
	calls := e.price.calls

	win, err := e.p.InstancePrices(ctx, []string{"m6id.large"}, true)
	require.NoError(t, err)
	assert.Equal(t, ports.InstancePrice{Type: "m6id.large", USDPerHour: 0.23165, VCPU: 2, MemoryGiB: 8, NVMeGiB: 118, Windows: true}, win["m6id.large"])
	assert.Equal(t, calls, e.price.calls, "the API is not retried during an outage window")

	_, err = e.p.InstancePrices(ctx, []string{"t4g.nano"}, true)
	assert.ErrorIs(t, err, ports.ErrNotFound, "Graviton has no Windows price")

	_, err = time.Parse(time.DateOnly, FallbackPrices().AsOf)
	assert.NoError(t, err, "the embedded table is dated")
}
