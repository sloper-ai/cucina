// SPDX-License-Identifier: FSL-1.1-ALv2

package porttest

import (
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
)

// ComputeHarness configures RunCompute.
type ComputeHarness struct {
	Compute ports.Compute
	Cluster string
	// Request is the launch template: image, at least two instance types, at
	// least one subnet, security groups, profile and the tags
	// (cluster/managed-by/pool/role and any extra tags). The suite sets
	// Token, Generation and the launch-token tag per launch.
	Request ports.LaunchRequest
	// Await waits for eventually consistent state.
	Await Await
	// ForceICE makes the given instance type fail with insufficient capacity
	// in every subnet of Request until restore is called. nil (real EC2):
	// the capacity checks are skipped.
	ForceICE func(t *testing.T, instanceType string) (restore func())
	// PriceTypes are instance types whose price must be known (nil: skip).
	PriceTypes []string
	// ForceThrottle makes the launch API throttle until restore is called
	// (nil: skipped; a real account cannot be throttled safely on purpose).
	ForceThrottle func(t *testing.T) (restore func())
}

func (h ComputeHarness) launch(t *testing.T, token string) ports.LaunchRequest {
	req := h.Request
	req.Token = token
	if req.Generation == "" {
		req.Generation = "porttest"
	}
	req.Tags = maps.Clone(h.Request.Tags)
	req.Tags[domain.TagLaunchToken] = token
	req.Tags[domain.TagGeneration] = req.Generation
	return req
}

func (h ComputeHarness) describe(t *testing.T, ids ...string) []ports.Instance {
	t.Helper()
	got, err := h.Compute.Describe(context.Background(), ports.InstanceFilter{Cluster: h.Cluster, Pool: h.Request.Pool, IDs: ids,
		States: []ports.InstanceState{ports.InstancePending, ports.InstanceRunning, ports.InstanceShuttingDown,
			ports.InstanceTerminated, ports.InstanceStopping, ports.InstanceStopped}})
	require.NoError(t, err)
	return got
}

func (h ComputeHarness) terminateOnCleanup(t *testing.T, ids ...string) {
	t.Cleanup(func() { _, _ = h.Compute.Terminate(context.Background(), h.Cluster, ids) })
}

// RunCompute checks the ports.Compute contract (R-POOL-2, R-SCALE-5/6): launches
// are idempotent per token and fully tagged, Describe is tag-filtered,
// Terminate refuses foreign instances and never stops, capacity errors walk the
// instance-type list, missing images and empty filters are reported.
func RunCompute(t *testing.T, newCompute func(t *testing.T) ComputeHarness) {
	ctx := context.Background()

	t.Run("LaunchIsIdempotentPerToken", func(t *testing.T) {
		h := newCompute(t)
		tok := uniq(t, "porttest")
		a, err := h.Compute.Launch(ctx, h.launch(t, tok))
		require.NoError(t, err)
		h.terminateOnCleanup(t, a.ID)
		b, err := h.Compute.Launch(ctx, h.launch(t, tok))
		require.NoError(t, err)
		require.Equal(t, a.ID, b.ID, "a second Launch with the same token must return the first instance")
		other := h.launch(t, tok)
		other.InstanceTypes = append([]string{other.InstanceTypes[len(other.InstanceTypes)-1]}, other.InstanceTypes[:len(other.InstanceTypes)-1]...)
		c, err := h.Compute.Launch(ctx, other)
		require.NoError(t, err)
		require.Equal(t, a.ID, c.ID, "the same token with other parameters still returns the first instance")
		h.Await(t, "instance visible in Describe", func() bool { return len(h.describe(t, a.ID)) == 1 })
		var withToken int
		for _, in := range h.describe(t) {
			if in.Tags[domain.TagLaunchToken] == tok {
				withToken++
			}
		}
		require.Equal(t, 1, withToken, "exactly one instance per token")
	})

	t.Run("LaunchTagsTheInstance", func(t *testing.T) {
		h := newCompute(t)
		req := h.launch(t, uniq(t, "porttest"))
		in, err := h.Compute.Launch(ctx, req)
		require.NoError(t, err)
		h.terminateOnCleanup(t, in.ID)
		for k, v := range req.Tags {
			require.Equal(t, v, in.Tags[k], "tag %s", k)
		}
		require.Equal(t, req.Pool, in.Pool)
		require.Contains(t, req.InstanceTypes, in.Type)
		require.Contains(t, req.SubnetIDs, in.SubnetID)
		// RunInstances responses carry no block-device mappings; Describe does.
		h.Await(t, "root volume reported by Describe", func() bool {
			got := h.describe(t, in.ID)
			return len(got) == 1 && len(got[0].VolumeIDs) > 0
		})
	})

	t.Run("TokenOfTerminatedInstanceReturnsIt", func(t *testing.T) {
		// A stale token returns its (terminated) instance instead of launching:
		// the planner then knows the seq is consumed (R-SCALE-5).
		h := newCompute(t)
		tok := uniq(t, "porttest")
		a, err := h.Compute.Launch(ctx, h.launch(t, tok))
		require.NoError(t, err)
		h.terminateOnCleanup(t, a.ID)
		_, err = h.Compute.Terminate(ctx, h.Cluster, []string{a.ID})
		require.NoError(t, err)
		h.Await(t, "instance terminated", func() bool {
			got := h.describe(t, a.ID)
			return len(got) == 0 || got[0].State == ports.InstanceTerminated
		})
		b, err := h.Compute.Launch(ctx, h.launch(t, tok))
		require.NoError(t, err)
		require.Equal(t, a.ID, b.ID)
		require.Contains(t, []ports.InstanceState{ports.InstanceShuttingDown, ports.InstanceTerminated}, b.State)
	})

	t.Run("DescribeIsTagFiltered", func(t *testing.T) {
		h := newCompute(t)
		_, err := h.Compute.Describe(ctx, ports.InstanceFilter{})
		require.Error(t, err, "Describe without the cluster tag must be refused")
		in, err := h.Compute.Launch(ctx, h.launch(t, uniq(t, "porttest")))
		require.NoError(t, err)
		h.terminateOnCleanup(t, in.ID)
		h.Await(t, "instance visible", func() bool { return len(h.describe(t, in.ID)) == 1 })
		other, err := h.Compute.Describe(ctx, ports.InstanceFilter{Cluster: h.Cluster, Pool: h.Request.Pool + "-other"})
		require.NoError(t, err)
		require.Empty(t, other, "another pool's filter must not match")
		foreign, err := h.Compute.Describe(ctx, ports.InstanceFilter{Cluster: h.Cluster + "-other", IDs: []string{in.ID}})
		require.NoError(t, err)
		require.Empty(t, foreign, "another cluster's filter must not match")
	})

	t.Run("TerminateRefusesForeignAndNeverStops", func(t *testing.T) {
		h := newCompute(t)
		in, err := h.Compute.Launch(ctx, h.launch(t, uniq(t, "porttest")))
		require.NoError(t, err)
		h.terminateOnCleanup(t, in.ID)
		errs, err := h.Compute.Terminate(ctx, h.Cluster+"-other", []string{in.ID})
		require.NoError(t, err)
		require.ErrorIs(t, errs[in.ID], ports.ErrNotOwned)
		errs, err = h.Compute.Terminate(ctx, h.Cluster, []string{in.ID})
		require.NoError(t, err)
		require.NoError(t, errs[in.ID])
		h.Await(t, "instance terminated", func() bool {
			got := h.describe(t, in.ID)
			for _, g := range got {
				require.NotContains(t, []ports.InstanceState{ports.InstanceStopping, ports.InstanceStopped}, g.State)
			}
			return len(got) == 0 || got[0].State == ports.InstanceTerminated
		})
		errs, err = h.Compute.Terminate(ctx, h.Cluster, []string{in.ID})
		require.NoError(t, err)
		require.NoError(t, errs[in.ID], "terminating a terminated instance is idempotent")
	})

	t.Run("NoOrphansAfterTermination", func(t *testing.T) {
		h := newCompute(t)
		launched, err := h.Compute.Launch(ctx, h.launch(t, uniq(t, "porttest")))
		require.NoError(t, err)
		h.terminateOnCleanup(t, launched.ID)
		var in ports.Instance
		h.Await(t, "volumes reported by Describe", func() bool {
			got := h.describe(t, launched.ID)
			if len(got) == 1 && len(got[0].VolumeIDs) > 0 {
				in = got[0]
				return true
			}
			return false
		})
		_, err = h.Compute.Terminate(ctx, h.Cluster, []string{in.ID})
		require.NoError(t, err)
		h.Await(t, "volumes and ENIs released", func() bool {
			got := h.describe(t, in.ID)
			if len(got) == 1 && got[0].State != ports.InstanceTerminated {
				return false
			}
			orphans, err := h.Compute.ListOrphans(ctx, h.Cluster)
			require.NoError(t, err)
			return !slices.ContainsFunc(orphans, func(o ports.Orphan) bool {
				return slices.Contains(in.VolumeIDs, o.ID) || slices.Contains(in.ENIIDs, o.ID)
			})
		})
	})

	t.Run("CapacityErrorsWalkTheTypeList", func(t *testing.T) {
		h := newCompute(t)
		if h.ForceICE == nil {
			t.Skip("backend cannot force InsufficientInstanceCapacity deterministically")
		}
		require.GreaterOrEqual(t, len(h.Request.InstanceTypes), 2, "harness must offer two instance types")
		first, second := h.Request.InstanceTypes[0], h.Request.InstanceTypes[1]
		restore := h.ForceICE(t, first)
		in, err := h.Compute.Launch(ctx, h.launch(t, uniq(t, "porttest")))
		require.NoError(t, err)
		h.terminateOnCleanup(t, in.ID)
		require.Equal(t, second, in.Type, "an ICE on the first type falls through to the next")
		restoreAll := []func(){restore}
		for _, typ := range h.Request.InstanceTypes[1:] {
			restoreAll = append(restoreAll, h.ForceICE(t, typ))
		}
		_, err = h.Compute.Launch(ctx, h.launch(t, uniq(t, "porttest")))
		require.ErrorIs(t, err, ports.ErrInsufficientCapacity)
		for _, r := range restoreAll {
			r()
		}
	})

	t.Run("MissingImageIsReported", func(t *testing.T) {
		h := newCompute(t)
		_, err := h.Compute.ResolveImage(ctx, ports.ImageSelector{ID: "ami-00000000000000000"})
		require.ErrorIs(t, err, ports.ErrImageNotFound)
	})

	t.Run("PricesAreKnown", func(t *testing.T) {
		h := newCompute(t)
		if len(h.PriceTypes) == 0 {
			t.Skip("no price types configured")
		}
		prices, err := h.Compute.InstancePrices(ctx, h.PriceTypes, false)
		require.NoError(t, err)
		for _, typ := range h.PriceTypes {
			require.Positive(t, prices[typ].USDPerHour, "price of %s", typ)
			require.Positive(t, prices[typ].VCPU, "vCPUs of %s", typ)
		}
		prices, err = h.Compute.InstancePrices(ctx, append([]string{"zz9.nonexistent"}, h.PriceTypes[0]), false)
		require.ErrorIs(t, err, ports.ErrNotFound, "an unknown type is reported")
		require.Positive(t, prices[h.PriceTypes[0]].USDPerHour, "known types are still priced")
	})

	t.Run("ThrottlingIsReportedAsErrThrottled", func(t *testing.T) {
		// A throttled launch must surface as ErrThrottled, never as a generic
		// error (the planner backs off and keeps the token).
		h := newCompute(t)
		if h.ForceThrottle == nil {
			t.Skip("backend cannot be throttled on purpose")
		}
		restore := h.ForceThrottle(t)
		defer restore()
		_, err := h.Compute.Launch(ctx, h.launch(t, uniq(t, "porttest")))
		require.ErrorIs(t, err, ports.ErrThrottled)
	})
}
