// SPDX-License-Identifier: FSL-1.1-ALv2

package ec2

import (
	"testing"
	"time"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/internal/ports/porttest"
)

// R-TEST-8b: the ports.Compute conformance suite against the EC2 adapter running on
// the in-memory SDK fake (integration tier). acceptance_test.go runs the same suite
// against real EC2.
func TestComputeConformance(t *testing.T) {
	porttest.RunCompute(t, func(t *testing.T) porttest.ComputeHarness {
		e := newEnv(t, func(o *Options) { o.OrphanGrace = time.Nanosecond })
		req := launchReq("", func(r *ports.LaunchRequest) {
			r.Tags = map[string]string{domain.TagCluster: testCluster, domain.TagManagedBy: domain.ManagedByValue,
				domain.TagPool: string(r.Pool), domain.TagRole: "worker"}
		})
		return porttest.ComputeHarness{
			Compute: e.p,
			Cluster: testCluster,
			Request: req,
			Await:   porttest.FakeAwait(e.clock.Advance, nil, time.Second, time.Minute),
			ForceICE: func(_ *testing.T, typ string) func() {
				e.ec2.setCapacity(typ, "*", "*", "InsufficientInstanceCapacity")
				return func() { e.ec2.setCapacity(typ, "*", "*", "") }
			},
			PriceTypes: []string{"c7i.large", "c7a.large"},
			ForceThrottle: func(*testing.T) func() {
				e.ec2.mu.Lock()
				e.ec2.throttle = true
				e.ec2.mu.Unlock()
				return func() {
					e.ec2.mu.Lock()
					e.ec2.throttle = false
					e.ec2.mu.Unlock()
				}
			},
		}
	})
}
