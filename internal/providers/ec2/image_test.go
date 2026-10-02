// SPDX-License-Identifier: FSL-1.1-ALv2

package ec2

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
)

// R-POOL-1 image selection, R-OPS-2 generations: by ID, or the newest available
// AMI owned by the account that carries every selector tag.
func TestResolveImage(t *testing.T) {
	e := newEnv(t)
	tagged := func(id, created, version string, mut func(*ec2types.Image)) {
		img := ec2types.Image{
			ImageId: aws.String(id), Name: aws.String(id), Architecture: ec2types.ArchitectureValuesArm64,
			CreationDate: aws.String(created), RootDeviceName: aws.String("/dev/xvda"),
			Tags: toEC2Tags(map[string]string{"cucina:pool-class": "linux-arm64", domain.TagImageVersion: version, domain.TagGeneration: "gen-" + version}),
			BlockDeviceMappings: []ec2types.BlockDeviceMapping{{DeviceName: aws.String("/dev/xvda"),
				Ebs: &ec2types.EbsBlockDevice{SnapshotId: aws.String("snap-" + id), VolumeSize: aws.Int32(6)}}},
		}
		if mut != nil {
			mut(&img)
		}
		e.ec2.addImage(img)
	}
	tagged("ami-old", "2026-09-01T00:00:00.000Z", "1.0", nil)
	tagged("ami-new", "2026-09-20T00:00:00.000Z", "1.1", nil)
	tagged("ami-newer-pending", "2026-09-25T00:00:00.000Z", "1.2", func(i *ec2types.Image) { i.State = ec2types.ImageStatePending })
	tagged("ami-newest-shared", "2026-09-28T00:00:00.000Z", "1.3", func(i *ec2types.Image) { i.OwnerId = aws.String("someone-else") })

	for _, tc := range []struct {
		name    string
		sel     ports.ImageSelector
		want    ports.Image
		wantErr error
	}{
		{name: "newest owned and available by tags", sel: ports.ImageSelector{Tags: map[string]string{"cucina:pool-class": "linux-arm64"}},
			want: ports.Image{ID: "ami-new", Name: "ami-new", Arch: "arm64", CreatedAt: time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC),
				Version: "1.1", Generation: "gen-1.1", SnapshotIDs: []string{"snap-ami-new"}, SizeGiB: 6, Platform: "linux"}},
		{name: "every tag must match", sel: ports.ImageSelector{Tags: map[string]string{"cucina:pool-class": "linux-arm64", domain.TagImageVersion: "1.0"}},
			want: ports.Image{ID: "ami-old", Name: "ami-old", Arch: "arm64", CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
				Version: "1.0", Generation: "gen-1.0", SnapshotIDs: []string{"snap-ami-old"}, SizeGiB: 6, Platform: "linux"}},
		{name: "by ID, Windows", sel: ports.ImageSelector{ID: "ami-win"},
			want: ports.Image{ID: "ami-win", Name: "cucina-windows", Arch: "x86_64", CreatedAt: time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC),
				SnapshotIDs: []string{"snap-win"}, SizeGiB: 30, Platform: "windows"}},
		{name: "no match", sel: ports.ImageSelector{Tags: map[string]string{"cucina:pool-class": "nope"}}, wantErr: ports.ErrImageNotFound},
		{name: "unknown ID", sel: ports.ImageSelector{ID: "ami-0123456789abcdef0"}, wantErr: ports.ErrImageNotFound},
		{name: "pending ID", sel: ports.ImageSelector{ID: "ami-newer-pending"}, wantErr: ports.ErrImageNotFound},
		{name: "empty selector", sel: ports.ImageSelector{}, wantErr: ports.ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := e.p.ResolveImage(context.Background(), tc.sel)
			if tc.wantErr != nil {
				assert.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// R-POOL-2 / R-OPS-2 Fast Launch: enable waits for "enabled" and is idempotent;
// disable returns only once the image has left Fast Launch (snapshots gone), which
// callers rely on before deregistering an AMI.
func TestFastLaunch(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	op := func(action string) ports.FastLaunchOp {
		return ports.FastLaunchOp{Action: action, ImageID: "ami-win", TargetCount: 4, MaxParallel: 2, LaunchTemplateID: "lt-prep"}
	}

	st, err := e.p.FastLaunch(ctx, op(FastLaunchDescribe))
	require.NoError(t, err)
	assert.Equal(t, ports.FastLaunchStatus{ImageID: "ami-win", State: "disabled"}, st)

	st, err = e.p.FastLaunch(ctx, op(FastLaunchEnable))
	require.NoError(t, err)
	assert.Equal(t, ports.FastLaunchStatus{ImageID: "ami-win", State: "enabled", Snapshots: 4}, st)
	e.ec2.mu.Lock()
	assert.Equal(t, int32(minParallelLaunches), aws.ToInt32(e.ec2.fastLaunch["ami-win"].item.MaxParallelLaunches), "raised to EC2's minimum")
	e.ec2.mu.Unlock()

	st, err = e.p.FastLaunch(ctx, op(FastLaunchEnable))
	require.NoError(t, err)
	assert.Equal(t, "enabled", st.State)
	assert.Equal(t, 1, e.ec2.callCount("EnableFastLaunch"), "re-enabling with the same settings makes no call")

	st, err = e.p.FastLaunch(ctx, op(FastLaunchDisable))
	require.NoError(t, err)
	assert.Equal(t, ports.FastLaunchStatus{ImageID: "ami-win", State: "disabled"}, st)
	e.ec2.mu.Lock()
	assert.Empty(t, e.ec2.snapshots, "pre-provisioned snapshots are gone when disable returns")
	e.ec2.mu.Unlock()

	_, err = e.p.FastLaunch(ctx, op(FastLaunchDisable))
	require.NoError(t, err)
	assert.Equal(t, 1, e.ec2.callCount("DisableFastLaunch"), "disabling a disabled image makes no call")

	_, err = e.p.FastLaunch(ctx, ports.FastLaunchOp{Action: FastLaunchEnable, ImageID: "ami-linux", TargetCount: 1})
	assert.ErrorIs(t, err, ports.ErrInvalid, "Fast Launch is Windows only")
	_, err = e.p.FastLaunch(ctx, ports.FastLaunchOp{Action: "toggle", ImageID: "ami-win"})
	assert.ErrorIs(t, err, ports.ErrInvalid)

	// The caller's context bounds the wait; the last observed state comes back with it.
	short, cancel := context.WithCancel(ctx)
	defer cancel()
	e.ec2.mu.Lock()
	e.ec2.fastLaunch["ami-win"] = &fakeFastLaunch{item: ec2types.DescribeFastLaunchImagesSuccessItem{
		ImageId: aws.String("ami-win"), State: ec2types.FastLaunchStateCodeDisabling}, ticks: 1 << 30}
	e.ec2.hooks["DescribeFastLaunchImages"] = cancel // expires during the first poll
	e.ec2.mu.Unlock()
	st, err = e.p.FastLaunch(short, op(FastLaunchDisable))
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, "disabling", st.State)
}
