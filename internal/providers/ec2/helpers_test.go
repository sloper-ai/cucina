// SPDX-License-Identifier: FSL-1.1-ALv2

package ec2

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
)

const testCluster = "c1"

// env is one provider wired to fresh fakes.
type env struct {
	p     *Provider
	ec2   *fakeEC2
	price *fakePricing
	clock *stepClock
}

func newEnv(t *testing.T, mut ...func(*Options)) *env {
	t.Helper()
	clock := newStepClock()
	f := newFakeEC2(clock)
	// A Linux AMI whose own mappings would leak volumes: DeleteOnTermination=false on
	// the root and on a second EBS device, plus an instance-store device.
	f.addImage(ec2types.Image{
		ImageId: aws.String("ami-linux"), Name: aws.String("cucina-linux"), Architecture: ec2types.ArchitectureValuesX8664,
		RootDeviceName: aws.String("/dev/xvda"), CreationDate: aws.String("2026-09-30T10:00:00.000Z"),
		BlockDeviceMappings: []ec2types.BlockDeviceMapping{
			{DeviceName: aws.String("/dev/xvda"), Ebs: &ec2types.EbsBlockDevice{SnapshotId: aws.String("snap-root"), VolumeSize: aws.Int32(8), VolumeType: ec2types.VolumeTypeGp2, DeleteOnTermination: aws.Bool(false)}},
			{DeviceName: aws.String("/dev/sdb"), Ebs: &ec2types.EbsBlockDevice{SnapshotId: aws.String("snap-data"), VolumeSize: aws.Int32(4), VolumeType: ec2types.VolumeTypeGp2, DeleteOnTermination: aws.Bool(false)}},
			{DeviceName: aws.String("/dev/sdc"), VirtualName: aws.String("ephemeral0")},
		},
	})
	f.addImage(ec2types.Image{
		ImageId: aws.String("ami-win"), Name: aws.String("cucina-windows"), Architecture: ec2types.ArchitectureValuesX8664,
		Platform: ec2types.PlatformValuesWindows, PlatformDetails: aws.String("Windows"), RootDeviceName: aws.String("/dev/sda1"),
		CreationDate: aws.String("2026-09-30T11:00:00.000Z"),
		BlockDeviceMappings: []ec2types.BlockDeviceMapping{
			{DeviceName: aws.String("/dev/sda1"), Ebs: &ec2types.EbsBlockDevice{SnapshotId: aws.String("snap-win"), VolumeSize: aws.Int32(30), VolumeType: ec2types.VolumeTypeGp3, DeleteOnTermination: aws.Bool(true)}},
		},
	})
	pr := &fakePricing{docs: map[string][]string{}}
	opts := Options{
		Region:                 "us-west-1",
		ExtraTags:              map[string]string{"cucina:env": "test"},
		Clock:                  clock,
		Logger:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
		FastLaunchPollInterval: 1, // the step clock makes polling free anyway
	}
	for _, m := range mut {
		m(&opts)
	}
	p, err := newProvider(f, pr, opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })
	return &env{p: p, ec2: f, price: pr, clock: clock}
}

// launchReq is a valid request; mutators adjust it per test row.
func launchReq(token string, mut ...func(*ports.LaunchRequest)) ports.LaunchRequest {
	r := ports.LaunchRequest{
		Pool:             "linux-x86",
		Generation:       "g1",
		Token:            token,
		ImageID:          "ami-linux",
		InstanceTypes:    []string{"c7i.large", "c7a.large"},
		SubnetIDs:        []string{"subnet-a", "subnet-a2"},
		SecurityGroupIDs: []string{"sg-workers"},
		InstanceProfile:  "cucina-worker",
		RootVolume:       ports.VolumeSpec{SizeGiB: 20, Type: "gp3", IOPS: 4000, Throughput: 250, InitializationRate: 200},
		ExtraVolumes:     []ports.VolumeSpec{{SizeGiB: 50}},
		UserData:         []byte("#!/bin/sh\necho hi\n"),
		Tags:             map[string]string{domain.TagCluster: testCluster, domain.TagImageVersion: "1.0.0"},
	}
	for _, m := range mut {
		m(&r)
	}
	return r
}

// liveInstances counts the fake's instances that are not terminated.
func (f *fakeEC2) liveInstances() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, fi := range f.instances {
		if fi.inst.State.Name != ec2types.InstanceStateNameTerminated && fi.inst.State.Name != ec2types.InstanceStateNameShuttingDown {
			n++
		}
	}
	return n
}

func (f *fakeEC2) instance(id string) fakeInstance {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.instances[id]
}

func (f *fakeEC2) volume(id string) (ec2types.Volume, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.volumes[id]
	if !ok {
		return ec2types.Volume{}, false
	}
	return *v, true
}

func (f *fakeEC2) eni(id string) (ec2types.NetworkInterface, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.enis[id]
	if !ok {
		return ec2types.NetworkInterface{}, false
	}
	return *n, true
}

func (f *fakeEC2) callCount(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[op]
}

func (f *fakeEC2) attempts() []Attempt {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Attempt(nil), f.tried...)
}

func (f *fakeEC2) failOnce(op string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext[op] = append(f.failNext[op], err)
}

// refresh describes once so the fake advances eventual states (pending → running,
// shutting-down → terminated with DeleteOnTermination resources deleted).
func (e *env) refresh(t *testing.T) {
	t.Helper()
	_, err := e.p.Describe(context.Background(), ports.InstanceFilter{Cluster: testCluster})
	require.NoError(t, err)
}

// addInstance creates an instance directly (no launch walk), e.g. a foreign one.
func (f *fakeEC2) addInstance(tags map[string]string, state ec2types.InstanceStateName) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.id("i")
	f.instances[id] = &fakeInstance{volumes: map[string]bool{}, enis: map[string]bool{}, inst: ec2types.Instance{
		InstanceId: aws.String(id), InstanceType: ec2types.InstanceTypeT3Nano, ImageId: aws.String("ami-linux"),
		State: &ec2types.InstanceState{Name: state}, Tags: toEC2Tags(tags), LaunchTime: aws.Time(f.clock.Now()),
		SubnetId: aws.String("subnet-a"), Placement: &ec2types.Placement{AvailabilityZone: aws.String(azA)},
	}}
	f.order = append(f.order, id)
	return id
}

// ownedTags are the tags Launch puts on everything for cluster/pool.
func ownedTags(cluster, pool string) map[string]string {
	return map[string]string{domain.TagCluster: cluster, domain.TagManagedBy: domain.ManagedByValue, domain.TagPool: pool, domain.TagGeneration: "g1"}
}
