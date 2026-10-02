// SPDX-License-Identifier: FSL-1.1-ALv2

package awsinv

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/stretchr/testify/require"
)

var campaign = map[string]string{"cucina:env": "e2e", "cucina:run": "e2e-test", "cucina:role": "worker"}

// fakeEC2 is a tiny in-memory EC2: instances by ID with tags and state.
type fakeEC2 struct {
	EC2API
	instances map[string]ec2types.Instance
}

func (f *fakeEC2) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	var out []ec2types.Instance
	for _, id := range in.InstanceIds {
		if i, ok := f.instances[id]; ok {
			out = append(out, i)
		}
	}
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: out}}}, nil
}

func (f *fakeEC2) TerminateInstances(_ context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	for _, id := range in.InstanceIds {
		i := f.instances[id]
		i.State = &ec2types.InstanceState{Name: ec2types.InstanceStateNameShuttingDown}
		f.instances[id] = i
	}
	return &ec2.TerminateInstancesOutput{}, nil
}

func inst(id string, tags map[string]string) ec2types.Instance {
	i := ec2types.Instance{InstanceId: aws.String(id), State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning}}
	for k, v := range tags {
		i.Tags = append(i.Tags, ec2types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return i
}

func with(m map[string]string, kv ...string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "" {
			delete(out, kv[i])
		} else {
			out[kv[i]] = kv[i+1]
		}
	}
	return out
}

// Guards §12 ("filter every destructive call by tag") for the T9a fault: only
// a controller-managed worker carrying every campaign tag is terminated.
func TestTerminateWorkerRefusesUntaggedInstances(t *testing.T) {
	owned := with(campaign, "cucina:managed-by", "cucina-controller")
	f := &fakeEC2{instances: map[string]ec2types.Instance{
		"i-owned":     inst("i-owned", owned),
		"i-other-run": inst("i-other-run", with(owned, "cucina:run", "e2e-other")),
		"i-manual":    inst("i-manual", with(owned, "cucina:managed-by", "")),
	}}
	inv := &Inventory{API: f, Tags: campaign}
	ctx := context.Background()
	require.Error(t, inv.TerminateWorker(ctx, "i-other-run"))
	require.Error(t, inv.TerminateWorker(ctx, "i-manual"))
	require.Error(t, inv.TerminateWorker(ctx, "i-missing"))
	require.NoError(t, inv.TerminateWorker(ctx, "i-owned"))
	require.Equal(t, ec2types.InstanceStateNameShuttingDown, f.instances["i-owned"].State.Name)
	require.Equal(t, ec2types.InstanceStateNameRunning, f.instances["i-other-run"].State.Name)

	_, err := (&Inventory{API: f}).Describe(ctx)
	require.ErrorContains(t, err, "untagged describe")
}

// Guards §10.4 "instance-seconds by type, EBS GB-hours … instance lifecycle
// timestamps" and NFR-C1's residue count, from sampled describes.
func TestIntegrateSnapshots(t *testing.T) {
	t0 := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	lin := Instance{ID: "i-1", Type: "c8i.8xlarge", Pool: "linux-x86-64", Platform: "linux", LaunchTime: t0}
	win := Instance{ID: "i-2", Type: "c7a.8xlarge", Pool: "windows-x86-64", Platform: "windows", LaunchTime: at(600), PublicIP: true}
	vol := Volume{ID: "vol-1", GiB: 30, IOPS: 3000, MiBps: 125, Attached: []string{"i-1"}}
	state := func(i Instance, s string) Instance { i.State = s; return i }
	snaps := []Snapshot{
		{At: at(0), Instances: []Instance{state(lin, "pending")}, Volumes: []Volume{vol}},
		{At: at(600), Instances: []Instance{state(lin, "running"), state(win, "pending")}, Volumes: []Volume{vol}},
		{At: at(1200), Instances: []Instance{state(lin, "running"), state(win, "running")}, Volumes: []Volume{vol}},
		{At: at(3600), Instances: []Instance{state(win, "running")}},
		{At: at(4200)},
	}
	require.Equal(t, Residue{Instances: 2, Volumes: 1, PublicIPs: 1}, snaps[2].Residue())
	require.True(t, snaps[4].Residue().Zero())

	u := Integrate(snaps)
	require.Equal(t, 2, u.MaxInstances)
	require.Equal(t, map[string]int{"linux-x86-64": 1, "windows-x86-64": 1}, u.MaxByPool)
	require.Len(t, u.Lifecycles, 2)
	l1 := u.Lifecycles[0]
	require.Equal(t, at(600), l1.Running)
	require.Equal(t, at(3600), l1.Gone)
	require.InDelta(t, 3600.0, l1.RunSeconds, 1e-9)
	require.Equal(t, []Volume{vol}, l1.Volumes)
	require.Equal(t, map[string]float64{"linux/c8i.8xlarge": 3600, "windows/c7a.8xlarge": 3600}, u.InstanceSeconds)
	require.InDelta(t, 30.0*3600/3600, u.VolumeGiBHours, 1e-9) // 30 GiB for the hour it was seen
	require.InDelta(t, 1.0, u.PublicIPv4Hours, 1e-9)           // i-2 from 600 s to 4200 s, attributed per interval
}
