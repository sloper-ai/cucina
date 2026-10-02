// SPDX-License-Identifier: FSL-1.1-ALv2

package ec2

import (
	"context"
	"fmt"
	"testing"
	"time"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
)

// R-SCALE-5/6: Describe is always tag-filtered, batches ID lists (the fake rejects
// filters with more than 200 values, like EC2) and reads every page.
func TestDescribeIsTagFilteredBatchedAndPaged(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.ec2.pageSize = 7
	var mine []string
	for i := range 450 {
		pool := "linux-x86"
		if i%2 == 1 {
			pool = "linux-arm"
		}
		mine = append(mine, e.ec2.addInstance(ownedTags(testCluster, pool), ec2types.InstanceStateNameRunning))
	}
	foreign := e.ec2.addInstance(ownedTags("c2", "linux-x86"), ec2types.InstanceStateNameRunning)
	unmanaged := e.ec2.addInstance(map[string]string{domain.TagCluster: testCluster}, ec2types.InstanceStateNameRunning)
	gone := e.ec2.addInstance(ownedTags(testCluster, "linux-x86"), ec2types.InstanceStateNameTerminated)

	_, err := e.p.Describe(ctx, ports.InstanceFilter{})
	require.ErrorIs(t, err, ports.ErrInvalid, "an untagged Describe is refused")

	all, err := e.p.Describe(ctx, ports.InstanceFilter{Cluster: testCluster})
	require.NoError(t, err)
	assert.ElementsMatch(t, mine, ids(all), "own running instances only: not c2's, not unmanaged, not terminated")

	arm, err := e.p.Describe(ctx, ports.InstanceFilter{Cluster: testCluster, Pool: "linux-arm"})
	require.NoError(t, err)
	assert.Len(t, arm, 225)
	for _, i := range arm {
		assert.Equal(t, domain.PoolName("linux-arm"), i.Pool)
		assert.Equal(t, "g1", i.Generation)
	}

	byID, err := e.p.Describe(ctx, ports.InstanceFilter{Cluster: testCluster, IDs: append(append([]string{}, mine...), foreign, unmanaged, "i-doesnotexist")})
	require.NoError(t, err)
	assert.ElementsMatch(t, mine, ids(byID))

	withGone, err := e.p.Describe(ctx, ports.InstanceFilter{Cluster: testCluster, IDs: []string{gone, mine[0]},
		States: []ports.InstanceState{ports.InstanceTerminated, ports.InstanceRunning}})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{gone, mine[0]}, ids(withGone))
}

func ids(insts []ports.Instance) []string {
	out := make([]string, len(insts))
	for i, inst := range insts {
		out[i] = inst.ID
	}
	return out
}

// R-POOL-2, NFR-C1: Terminate refuses anything without this cluster's tags,
// reports unknown IDs, treats terminated instances as done, batches IDs, and a
// terminated worker leaves no volume or ENI behind (zero idle cost).
func TestTerminate(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	var launched []ports.Instance
	for i := range 3 {
		inst, err := e.p.Launch(ctx, launchReq(fmt.Sprintf("tok-%d", i)))
		require.NoError(t, err)
		launched = append(launched, inst)
	}
	e.refresh(t) // pending → running; volumes attached
	foreign := e.ec2.addInstance(ownedTags("c2", "linux-x86"), ec2types.InstanceStateNameRunning)
	untagged := e.ec2.addInstance(map[string]string{"Name": "someone else"}, ec2types.InstanceStateNameRunning)
	done := e.ec2.addInstance(ownedTags(testCluster, "linux-x86"), ec2types.InstanceStateNameTerminated)

	var vols, enis []string
	for _, inst := range launched {
		fi := e.ec2.instance(inst.ID)
		for v := range fi.volumes {
			vols = append(vols, v)
		}
		for n := range fi.enis {
			enis = append(enis, n)
		}
	}
	require.Len(t, vols, 9)

	targets := []string{launched[0].ID, launched[1].ID, launched[2].ID, foreign, untagged, done, "i-0000000000000dead", launched[0].ID}
	failed, err := e.p.Terminate(ctx, testCluster, targets)
	require.NoError(t, err)
	require.Len(t, failed, 3)
	assert.ErrorIs(t, failed[foreign], ports.ErrNotOwned)
	assert.ErrorIs(t, failed[untagged], ports.ErrNotOwned)
	assert.ErrorIs(t, failed["i-0000000000000dead"], ports.ErrNotFound)
	assert.Equal(t, 1, e.ec2.callCount("TerminateInstances"), "one batched call")

	e.refresh(t) // shutting-down → terminated
	assert.Equal(t, 2, e.ec2.liveInstances(), "only the foreign and untagged instances survive")
	for _, v := range vols {
		_, ok := e.ec2.volume(v)
		assert.False(t, ok, "volume %s survived its instance", v)
	}
	for _, n := range enis {
		_, ok := e.ec2.eni(n)
		assert.False(t, ok, "ENI %s survived its instance", n)
	}

	_, err = e.p.Terminate(ctx, "", []string{foreign})
	assert.ErrorIs(t, err, ports.ErrInvalid)

	// The IAM tag condition is the second line of defence: a denial is "not owned".
	inst, err := e.p.Launch(ctx, launchReq("tok-iam"))
	require.NoError(t, err)
	e.ec2.failOnce("TerminateInstances", apiError("UnauthorizedOperation"))
	failed, err = e.p.Terminate(ctx, testCluster, []string{inst.ID})
	require.NoError(t, err)
	assert.ErrorIs(t, failed[inst.ID], ports.ErrNotOwned)
}

// R-SCALE-6: more than 200 IDs are split into batches.
func TestTerminateBatches(t *testing.T) {
	e := newEnv(t)
	var targets []string
	for range 450 {
		targets = append(targets, e.ec2.addInstance(ownedTags(testCluster, "p"), ec2types.InstanceStateNameRunning))
	}
	failed, err := e.p.Terminate(context.Background(), testCluster, targets)
	require.NoError(t, err)
	assert.Empty(t, failed)
	assert.Equal(t, 3, e.ec2.callCount("TerminateInstances"))
	assert.Zero(t, e.ec2.liveInstances())
}

// R-POOL-2 sweep, NFR-C1: pool-tagged volumes/ENIs left unattached beyond the grace
// period are orphans; deletion re-checks ownership and state.
func TestOrphans(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	t0 := e.clock.Now()
	vol, eni := e.ec2.addOrphan(ownedTags(testCluster, "linux-x86"), t0)
	e.ec2.addOrphan(ownedTags("c2", "linux-x86"), t0) // another installation's
	_, err := e.p.Launch(ctx, launchReq("tok"))       // attached resources are never orphans
	require.NoError(t, err)

	got, err := e.p.ListOrphans(ctx, testCluster)
	require.NoError(t, err)
	assert.Empty(t, got, "within the grace period")
	_, err = e.p.ListOrphans(ctx, "c2") // another cluster's sweep must not reset c1's ENI timer
	require.NoError(t, err)

	e.clock.Advance(6 * time.Minute)
	got, err = e.p.ListOrphans(ctx, testCluster)
	require.NoError(t, err)
	require.ElementsMatch(t, []ports.Orphan{
		{Kind: ports.OrphanVolume, ID: vol, Pool: "linux-x86", Age: 6 * time.Minute},
		{Kind: ports.OrphanENI, ID: eni, Pool: "linux-x86", Age: 6 * time.Minute},
	}, got)

	// Someone re-tags the volume between list and delete: it is no longer ours.
	e.ec2.mu.Lock()
	e.ec2.volumes[vol].Tags = toEC2Tags(ownedTags("c2", "linux-x86"))
	e.ec2.mu.Unlock()
	failed, err := e.p.DeleteOrphans(ctx, testCluster, got)
	require.NoError(t, err)
	assert.ErrorIs(t, failed[vol], ports.ErrNotOwned)
	_, volLeft := e.ec2.volume(vol)
	_, eniLeft := e.ec2.eni(eni)
	assert.True(t, volLeft)
	assert.False(t, eniLeft, "the ENI was deleted")

	failed, err = e.p.DeleteOrphans(ctx, testCluster, []ports.Orphan{{Kind: ports.OrphanENI, ID: eni}})
	require.NoError(t, err)
	assert.Empty(t, failed, "deleting an already deleted orphan succeeds")

	_, err = e.p.ListOrphans(ctx, "")
	assert.ErrorIs(t, err, ports.ErrInvalid)
}

// A restarted controller does not delete an ENI it has not watched for a grace period.
func TestOrphanENIGraceSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	_, eni := e.ec2.addOrphan(ownedTags(testCluster, "p"), e.clock.Now().Add(-time.Hour))
	failed, err := e.p.DeleteOrphans(ctx, testCluster, []ports.Orphan{{Kind: ports.OrphanENI, ID: eni}})
	require.NoError(t, err)
	assert.ErrorIs(t, failed[eni], ports.ErrInvalid)
	_, ok := e.ec2.eni(eni)
	assert.True(t, ok)
}
