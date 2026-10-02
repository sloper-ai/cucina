// SPDX-License-Identifier: FSL-1.1-ALv2

package ec2

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
)

// R-POOL-1/2/3, NFR-C1/C4: a launched worker has IMDSv2 (hop limit 1, metadata tags),
// terminates on OS shutdown, cannot be stopped via the API, carries the same tags on
// instance, volumes and ENI, and every EBS volume — including the AMI's own mappings
// that say otherwise — is deleted on termination. Public IPv4 only when asked.
func TestLaunchShapesInstanceVolumesAndENI(t *testing.T) {
	for _, tc := range []struct {
		name     string
		subnet   string
		publicIP bool
	}{
		{"private subnet", "subnet-a", false},
		{"subnet that maps public IPs by default gets none", "subnet-a2", false},
		{"public IPv4 only on request", "subnet-a", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			inst, err := e.p.Launch(context.Background(), launchReq("tok-1", func(r *ports.LaunchRequest) {
				r.SubnetIDs = []string{tc.subnet}
				r.AssociatePublicIP = tc.publicIP
			}))
			require.NoError(t, err)

			fi := e.ec2.instance(inst.ID)
			md := fi.inst.MetadataOptions
			require.NotNil(t, md)
			assert.Equal(t, ec2types.HttpTokensStateRequired, md.HttpTokens)
			assert.Equal(t, int32(1), aws.ToInt32(md.HttpPutResponseHopLimit))
			assert.Equal(t, ec2types.InstanceMetadataTagsStateEnabled, md.InstanceMetadataTags)
			assert.Equal(t, ec2types.ShutdownBehaviorTerminate, fi.shutdownBehavior)
			assert.False(t, fi.disableAPIStop, "stop protection would also block termination")
			assert.Equal(t, tc.publicIP, fi.publicIP)
			assert.Equal(t, "cucina-worker", fi.profile)
			ud, err := base64.StdEncoding.DecodeString(fi.userData)
			require.NoError(t, err)
			assert.Equal(t, "#!/bin/sh\necho hi\n", string(ud))

			want := map[string]string{
				domain.TagCluster: testCluster, domain.TagManagedBy: domain.ManagedByValue, domain.TagPool: "linux-x86",
				domain.TagGeneration: "g1", domain.TagLaunchToken: "tok-1", domain.TagRole: "worker",
				domain.TagImageVersion: "1.0.0", "cucina:env": "test",
			}
			assert.Equal(t, want, fromEC2Tags(fi.inst.Tags))
			assert.Equal(t, want, inst.Tags)
			assert.Equal(t, domain.PoolName("linux-x86"), inst.Pool)
			assert.Equal(t, "g1", inst.Generation)

			require.Len(t, fi.volumes, 3, "AMI root, AMI data device, extra volume")
			sizes := map[int32]ec2types.Volume{}
			for id, dot := range fi.volumes {
				assert.True(t, dot, "volume %s must be DeleteOnTermination", id)
				v, ok := e.ec2.volume(id)
				require.True(t, ok)
				assert.Equal(t, want, fromEC2Tags(v.Tags), "volume tags")
				sizes[aws.ToInt32(v.Size)] = v
			}
			root := sizes[20]
			assert.Equal(t, ec2types.VolumeTypeGp3, root.VolumeType)
			assert.Equal(t, int32(4000), aws.ToInt32(root.Iops))
			assert.Equal(t, int32(250), aws.ToInt32(root.Throughput))
			assert.Equal(t, int32(200), aws.ToInt32(root.VolumeInitializationRate))
			assert.Equal(t, ec2types.VolumeTypeGp3, sizes[50].VolumeType, "gp3 is the default type")
			assert.Contains(t, sizes, int32(4), "the AMI's second device is kept, but deleted on termination")

			require.Len(t, fi.enis, 1)
			for id, dot := range fi.enis {
				assert.True(t, dot)
				n, ok := e.ec2.eni(id)
				require.True(t, ok)
				assert.Equal(t, want, fromEC2Tags(n.TagSet), "ENI tags")
			}
		})
	}
}

// R-POOL-2, R-SCALE-4: the walk over InstanceTypes x SubnetIDs (types outer), spot
// with on-demand fallback, and the error contract: capacity/quota move on, every
// combination is tried exactly once (no hot loop on ICE), anything else stops.
func TestLaunchWalkAndErrors(t *testing.T) {
	type capRule struct{ typ, subnet, market, code string }
	for _, tc := range []struct {
		name      string
		req       func(*ports.LaunchRequest)
		capacity  []capRule
		failRun   error // first RunInstances answer
		wantType  string
		wantNet   string
		wantCap   ports.CapacityType
		wantErr   []error
		notErr    []error
		wantTried int
	}{
		{name: "first choice", wantType: "c7i.large", wantNet: "subnet-a", wantCap: ports.OnDemand, wantTried: 1},
		{name: "ICE moves to the next subnet first", capacity: []capRule{{"c7i.large", "subnet-a", "*", "InsufficientInstanceCapacity"}},
			wantType: "c7i.large", wantNet: "subnet-a2", wantCap: ports.OnDemand, wantTried: 2},
		{name: "ICE on a type moves to the next type", capacity: []capRule{{"c7i.large", "*", "*", "InsufficientInstanceCapacity"}},
			wantType: "c7a.large", wantNet: "subnet-a", wantCap: ports.OnDemand, wantTried: 3},
		{name: "type not offered in the AZ is skipped", capacity: []capRule{{"c7i.large", "*", "*", "Unsupported"}},
			wantType: "c7a.large", wantNet: "subnet-a", wantCap: ports.OnDemand, wantTried: 3},
		{name: "ICE everywhere", capacity: []capRule{{"*", "*", "*", "InsufficientInstanceCapacity"}},
			wantErr: []error{ports.ErrInsufficientCapacity}, notErr: []error{ports.ErrQuotaExceeded}, wantTried: 4},
		{name: "vCPU quota everywhere", capacity: []capRule{{"*", "*", "*", "VcpuLimitExceeded"}},
			wantErr: []error{ports.ErrQuotaExceeded}, notErr: []error{ports.ErrInsufficientCapacity}, wantTried: 4},
		{name: "mixed ICE and quota", capacity: []capRule{{"c7i.large", "*", "*", "InsufficientInstanceCapacity"}, {"c7a.large", "*", "*", "InstanceLimitExceeded"}},
			wantErr: []error{ports.ErrInsufficientCapacity, ports.ErrQuotaExceeded}, wantTried: 4},
		{name: "spot", req: func(r *ports.LaunchRequest) { r.CapacityType = ports.Spot },
			wantType: "c7i.large", wantNet: "subnet-a", wantCap: ports.Spot, wantTried: 1},
		{name: "spot falls back to on-demand", req: func(r *ports.LaunchRequest) { r.CapacityType, r.FallbackOnDemand = ports.Spot, true },
			capacity: []capRule{{"*", "*", "spot", "InsufficientInstanceCapacity"}},
			wantType: "c7i.large", wantNet: "subnet-a", wantCap: ports.OnDemand, wantTried: 5},
		{name: "spot without fallback", req: func(r *ports.LaunchRequest) { r.CapacityType = ports.Spot },
			capacity: []capRule{{"*", "*", "spot", "MaxSpotInstanceCountExceeded"}},
			wantErr:  []error{ports.ErrQuotaExceeded}, wantTried: 4},
		{name: "throttling stops the walk", failRun: apiError("RequestLimitExceeded"), wantErr: []error{ports.ErrThrottled}, wantTried: 0},
		{name: "image gone", failRun: apiError("InvalidAMIID.Unavailable"), wantErr: []error{ports.ErrImageNotFound}, wantTried: 0},
		{name: "permission", failRun: apiError("UnauthorizedOperation"), wantErr: []error{ports.ErrInvalid}, wantTried: 0},
		{name: "validation", failRun: apiError("InvalidParameterCombination"), wantErr: []error{ports.ErrInvalid}, wantTried: 0},
		{name: "unknown server error is returned unwrapped", failRun: apiError("InternalError"),
			notErr: []error{ports.ErrInsufficientCapacity, ports.ErrThrottled, ports.ErrInvalid}, wantTried: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			for _, c := range tc.capacity {
				e.ec2.setCapacity(c.typ, c.subnet, c.market, c.code)
			}
			if tc.failRun != nil {
				e.ec2.failOnce("RunInstances", tc.failRun)
			}
			req := launchReq("tok", func(r *ports.LaunchRequest) { r.SubnetIDs = []string{"subnet-a", "subnet-a2"} })
			if tc.req != nil {
				tc.req(&req)
			}
			inst, err := e.p.Launch(context.Background(), req)
			tried := e.ec2.attempts()
			assert.Len(t, tried, tc.wantTried, "combinations reaching EC2's capacity check")
			seen := map[Attempt]bool{}
			for _, a := range tried {
				assert.False(t, seen[a], "combination %v tried twice", a)
				seen[a] = true
			}
			if tc.wantType != "" {
				require.NoError(t, err)
				assert.Equal(t, tc.wantType, inst.Type)
				assert.Equal(t, tc.wantNet, inst.SubnetID)
				assert.Equal(t, tc.wantCap, inst.CapacityType)
				assert.Equal(t, 1, e.ec2.liveInstances())
				return
			}
			require.Error(t, err)
			for _, s := range tc.wantErr {
				require.ErrorIs(t, err, s)
			}
			for _, s := range tc.notErr {
				require.NotErrorIs(t, err, s)
			}
			var ce *CapacityError
			if errors.As(err, &ce) {
				assert.Len(t, ce.Tried, tc.wantTried, "CapacityError lists every combination")
			}
			assert.Equal(t, 0, e.ec2.liveInstances())
		})
	}
}

// Fail fast on configuration (R-TEST-7): malformed requests never reach EC2.
func TestLaunchRejectsInvalidRequests(t *testing.T) {
	for name, mut := range map[string]func(*ports.LaunchRequest){
		"no cluster tag":           func(r *ports.LaunchRequest) { delete(r.Tags, domain.TagCluster) },
		"pool tag contradicts":     func(r *ports.LaunchRequest) { r.Tags[domain.TagPool] = "other" },
		"tag key unusable in IMDS": func(r *ports.LaunchRequest) { r.Tags["team/owner"] = "x" },
		"reserved aws: tag":        func(r *ports.LaunchRequest) { r.Tags["aws:foo"] = "x" },
		"no token":                 func(r *ports.LaunchRequest) { r.Token = "" },
		"no instance types":        func(r *ports.LaunchRequest) { r.InstanceTypes = nil },
		"no subnets":               func(r *ports.LaunchRequest) { r.SubnetIDs = nil },
		"unknown capacity type":    func(r *ports.LaunchRequest) { r.CapacityType = "reserved" },
		"init rate out of range":   func(r *ports.LaunchRequest) { r.RootVolume.InitializationRate = 50 },
		"init rate on an empty volume": func(r *ports.LaunchRequest) {
			r.ExtraVolumes[0].InitializationRate = 200
		},
		"extra volume without size":          func(r *ports.LaunchRequest) { r.ExtraVolumes[0].SizeGiB = 0 },
		"root smaller than the AMI snapshot": func(r *ports.LaunchRequest) { r.RootVolume.SizeGiB = 4 },
		"device mapped twice": func(r *ports.LaunchRequest) {
			r.ExtraVolumes = []ports.VolumeSpec{{DeviceName: "/dev/sdf", SizeGiB: 1}, {DeviceName: "/dev/xvdf", SizeGiB: 1}}
		},
		"user data too large": func(r *ports.LaunchRequest) { r.UserData = make([]byte, maxUserData+1) },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			_, err := e.p.Launch(context.Background(), launchReq("tok", mut))
			require.ErrorIs(t, err, ports.ErrInvalid)
			assert.Zero(t, e.ec2.callCount("RunInstances"))
		})
	}
}

// R-SCALE-5: a Launch is idempotent per token — retries after lost responses,
// eventual consistency or an earlier partially-walked call never double-launch.
func TestLaunchIsIdempotentPerToken(t *testing.T) {
	ctx := context.Background()

	t.Run("same token returns the same instance", func(t *testing.T) {
		e := newEnv(t)
		a, err := e.p.Launch(ctx, launchReq("tok"))
		require.NoError(t, err)
		b, err := e.p.Launch(ctx, launchReq("tok"))
		require.NoError(t, err)
		assert.Equal(t, a.ID, b.ID)
		assert.Equal(t, 1, e.ec2.liveInstances())
	})

	t.Run("lost response then retry", func(t *testing.T) {
		e := newEnv(t)
		e.ec2.lose["RunInstances"] = 1
		_, err := e.p.Launch(ctx, launchReq("tok"))
		require.Error(t, err)
		e.ec2.stale = 1 // the retry's pre-check does not see the instance yet
		b, err := e.p.Launch(ctx, launchReq("tok"))
		require.NoError(t, err)
		assert.Equal(t, 1, e.ec2.liveInstances())
		assert.NotEmpty(t, b.ID)
	})

	t.Run("retry after a walk whose earlier combination regained capacity", func(t *testing.T) {
		e := newEnv(t)
		e.ec2.setCapacity("c7i.large", "subnet-a", "*", "InsufficientInstanceCapacity")
		e.ec2.lose["RunInstances"] = 1 // the success on subnet-a2 (same AZ) is lost
		_, err := e.p.Launch(ctx, launchReq("tok"))
		require.Error(t, err, "the successful call's response was lost")
		require.Equal(t, 1, e.ec2.liveInstances())
		// Capacity is back on the first choice and Describe lags: EC2 answers
		// IdempotentParameterMismatch and the provider returns the original instance.
		delete(e.ec2.capacity, "c7i.large|subnet-a|*")
		e.ec2.stale = 1
		b, err := e.p.Launch(ctx, launchReq("tok"))
		require.NoError(t, err)
		assert.Equal(t, "subnet-a2", b.SubnetID)
		assert.Equal(t, 1, e.ec2.liveInstances())
	})

	t.Run("tokens are scoped per cluster", func(t *testing.T) {
		e := newEnv(t)
		a, err := e.p.Launch(ctx, launchReq("tok"))
		require.NoError(t, err)
		b, err := e.p.Launch(ctx, launchReq("tok", func(r *ports.LaunchRequest) { r.Tags[domain.TagCluster] = "c2" }))
		require.NoError(t, err)
		assert.NotEqual(t, a.ID, b.ID)
	})

	t.Run("token of a terminated instance is not relaunched", func(t *testing.T) {
		e := newEnv(t)
		a, err := e.p.Launch(ctx, launchReq("tok"))
		require.NoError(t, err)
		failed, err := e.p.Terminate(ctx, testCluster, []string{a.ID})
		require.NoError(t, err)
		require.Empty(t, failed)
		e.refresh(t)
		b, err := e.p.Launch(ctx, launchReq("tok"))
		require.NoError(t, err)
		assert.Equal(t, a.ID, b.ID)
		assert.Equal(t, ports.InstanceTerminated, b.State)
		assert.Equal(t, 0, e.ec2.liveInstances())
	})
}
