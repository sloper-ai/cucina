// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build acceptance

package ec2

// Acceptance tier (R-TEST-2): the provider against real EC2 in us-west-1 with
// t4g.nano instances and a stock Amazon Linux AMI. It creates a few cents of
// resources, tags all of them with the campaign tags (§12) and terminates them.
//
//	CUCINA_EC2_ACCEPTANCE=1 CUCINA_RUN_ID=… CUCINA_EXPIRES=… AWS_PROFILE=default \
//	  go test -tags acceptance -run TestAcceptance -v -timeout 20m ./internal/providers/ec2/
//
// Network inputs come from the aws-e2e base layer outputs
// (~/.config/cucina/aws-e2e/base-outputs.json, or $CUCINA_AWS_BASE_OUTPUTS).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	ec2sdk "github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/internal/ports/porttest"
)

const acceptanceRegion = "us-west-1"

type acceptanceEnv struct {
	p        *Provider
	raw      *ec2sdk.Client
	cluster  string
	campaign map[string]string
	subnet   string
	sg       string
	ami      string
}

func newAcceptanceEnv(t *testing.T, ctx context.Context) *acceptanceEnv {
	t.Helper()
	if os.Getenv("CUCINA_EC2_ACCEPTANCE") != "1" {
		t.Skip("set CUCINA_EC2_ACCEPTANCE=1 to run against real EC2 (creates tiny tagged instances in us-west-1)")
	}
	runID, expires := os.Getenv("CUCINA_RUN_ID"), os.Getenv("CUCINA_EXPIRES")
	require.NotEmpty(t, runID, "CUCINA_RUN_ID (campaign tag)")
	require.NotEmpty(t, expires, "CUCINA_EXPIRES (campaign tag)")

	path := os.Getenv("CUCINA_AWS_BASE_OUTPUTS")
	if path == "" {
		home, err := os.UserHomeDir()
		require.NoError(t, err)
		path = filepath.Join(home, ".config", "cucina", "aws-e2e", "base-outputs.json")
	}
	b, err := os.ReadFile(path)
	require.NoError(t, err, "base layer outputs from the aws-e2e environment")
	var outs map[string]any
	require.NoError(t, json.Unmarshal(b, &outs))
	str := func(k string) string {
		v, _ := outs[k].(string)
		require.NotEmpty(t, v, "base output %s", k)
		return v
	}

	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(acceptanceRegion))
	require.NoError(t, err)
	e := &acceptanceEnv{
		raw:      ec2sdk.NewFromConfig(cfg),
		cluster:  "acc-ec2-" + runID,
		campaign: map[string]string{"cucina:env": "e2e", "cucina:run": runID, "cucina:expires": expires},
		subnet:   str("private_subnet_id"),
		sg:       str("sg_workers"),
	}
	e.p, err = New(cfg, Options{ExtraTags: e.campaign, OrphanGrace: time.Minute, FastLaunchPollInterval: 5 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { _ = e.p.Close() })
	t.Cleanup(func() { e.sweep(t) })

	imgs, err := e.raw.DescribeImages(ctx, &ec2sdk.DescribeImagesInput{Owners: []string{"amazon"}, Filters: []ec2types.Filter{
		filter("name", "al2023-ami-minimal-2023.*-kernel-6.*-arm64"), filter("state", "available")}})
	require.NoError(t, err)
	require.NotEmpty(t, imgs.Images)
	slices.SortFunc(imgs.Images, func(a, b ec2types.Image) int {
		return -compareStrings(aws.ToString(a.CreationDate), aws.ToString(b.CreationDate))
	})
	e.ami = aws.ToString(imgs.Images[0].ImageId)
	return e
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// sweep terminates anything this test tagged, whatever happened (teardown always runs, §12).
func (e *acceptanceEnv) sweep(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := e.raw.DescribeInstances(ctx, &ec2sdk.DescribeInstancesInput{Filters: []ec2types.Filter{
		filter("tag:cucina:run", e.campaign["cucina:run"]),
		filter("tag:"+domain.TagCluster, e.cluster, e.cluster+"-foreign"),
		filter("instance-state-name", "pending", "running", "stopping", "stopped"),
	}})
	if err != nil {
		t.Errorf("sweep: %v", err)
		return
	}
	var ids []string
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			ids = append(ids, aws.ToString(i.InstanceId))
		}
	}
	if len(ids) > 0 {
		t.Logf("sweep: terminating leftovers %v", ids)
		if _, err := e.raw.TerminateInstances(ctx, &ec2sdk.TerminateInstancesInput{InstanceIds: ids}); err != nil {
			t.Errorf("sweep: %v", err)
		}
	}
}

func (e *acceptanceEnv) request(token string, mut ...func(*ports.LaunchRequest)) ports.LaunchRequest {
	r := ports.LaunchRequest{
		Pool:             "acceptance",
		Generation:       "g1",
		Token:            token,
		ImageID:          e.ami,
		InstanceTypes:    []string{"t4g.nano"},
		SubnetIDs:        []string{e.subnet},
		SecurityGroupIDs: []string{e.sg},
		RootVolume:       ports.VolumeSpec{SizeGiB: 4, Type: "gp3", InitializationRate: 100},
		ExtraVolumes:     []ports.VolumeSpec{{SizeGiB: 1, Type: "gp3"}},
		Tags:             map[string]string{domain.TagCluster: e.cluster, "Name": "cucina-ec2-acceptance"},
	}
	for _, m := range mut {
		m(&r)
	}
	return r
}

// waitFor polls cond every 3 s until it holds or the deadline passes.
func waitFor(t *testing.T, ctx context.Context, what string, limit time.Duration, cond func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for {
		ok, err := cond()
		require.NoError(t, err, what)
		if ok {
			return
		}
		require.True(t, time.Now().Before(deadline), "timed out waiting for %s", what)
		select {
		case <-ctx.Done():
			t.Fatalf("%s: %v", what, ctx.Err())
		case <-time.After(3 * time.Second):
		}
	}
}

// R-POOL-1/2/3, R-SCALE-4/5/6, NFR-C1 on real EC2.
func TestAcceptanceRealEC2(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	e := newAcceptanceEnv(t, ctx)
	token := "acc-" + strconv.FormatInt(time.Now().UnixNano(), 36)

	// ICE path, cheaply: c7gn.medium is not offered in us-west-1, so EC2 refuses it at
	// once; the walk moves on to t4g.nano.
	start := time.Now()
	inst, err := e.p.Launch(ctx, e.request(token, func(r *ports.LaunchRequest) { r.InstanceTypes = []string{"c7gn.medium", "t4g.nano"} }))
	require.NoError(t, err)
	t.Logf("launched %s (%s) in %v", inst.ID, inst.Type, time.Since(start).Round(time.Millisecond))
	assert.Equal(t, "t4g.nano", inst.Type)

	again, err := e.p.Launch(ctx, e.request(token, func(r *ports.LaunchRequest) { r.InstanceTypes = []string{"c7gn.medium", "t4g.nano"} }))
	require.NoError(t, err)
	assert.Equal(t, inst.ID, again.ID, "same token, same instance (R-SCALE-5)")

	_, err = e.p.Launch(ctx, e.request(token+"-ice", func(r *ports.LaunchRequest) { r.InstanceTypes = []string{"c7gn.medium"} }))
	var ce *CapacityError
	require.ErrorAs(t, err, &ce)
	assert.ErrorIs(t, err, ports.ErrInsufficientCapacity)
	require.Len(t, ce.Tried, 1)
	t.Logf("unavailable type answered %q", ce.Tried[0].Code)

	var running ports.Instance
	waitFor(t, ctx, "instance running with volumes and ENI", 4*time.Minute, func() (bool, error) {
		got, err := e.p.Describe(ctx, ports.InstanceFilter{Cluster: e.cluster, IDs: []string{inst.ID}})
		if err != nil || len(got) != 1 {
			return false, err
		}
		running = got[0]
		return running.State == ports.InstanceRunning && len(running.VolumeIDs) == 2 && len(running.ENIIDs) == 1, nil
	})
	assert.Equal(t, domain.PoolName("acceptance"), running.Pool)
	assert.Equal(t, "g1", running.Generation)
	assert.True(t, running.PrivateIP.IsValid())

	wantTags := map[string]string{domain.TagCluster: e.cluster, domain.TagManagedBy: domain.ManagedByValue, domain.TagPool: "acceptance",
		domain.TagGeneration: "g1", domain.TagLaunchToken: token, domain.TagRole: "worker", "Name": "cucina-ec2-acceptance"}
	for k, v := range e.campaign {
		wantTags[k] = v
	}
	assert.Equal(t, wantTags, running.Tags)

	raw, err := e.raw.DescribeInstances(ctx, &ec2sdk.DescribeInstancesInput{InstanceIds: []string{inst.ID}})
	require.NoError(t, err)
	ri := raw.Reservations[0].Instances[0]
	assert.Equal(t, ec2types.HttpTokensStateRequired, ri.MetadataOptions.HttpTokens)
	assert.Equal(t, int32(1), aws.ToInt32(ri.MetadataOptions.HttpPutResponseHopLimit))
	assert.Equal(t, ec2types.InstanceMetadataTagsStateEnabled, ri.MetadataOptions.InstanceMetadataTags)
	assert.Nil(t, ri.PublicIpAddress, "no public IPv4 unless asked")
	for _, b := range ri.BlockDeviceMappings {
		assert.True(t, aws.ToBool(b.Ebs.DeleteOnTermination), "volume %s", aws.ToString(b.Ebs.VolumeId))
	}
	attr, err := e.raw.DescribeInstanceAttribute(ctx, &ec2sdk.DescribeInstanceAttributeInput{InstanceId: aws.String(inst.ID),
		Attribute: ec2types.InstanceAttributeNameInstanceInitiatedShutdownBehavior})
	require.NoError(t, err)
	assert.Equal(t, "terminate", aws.ToString(attr.InstanceInitiatedShutdownBehavior.Value))

	vols, err := e.raw.DescribeVolumes(ctx, &ec2sdk.DescribeVolumesInput{VolumeIds: running.VolumeIDs})
	require.NoError(t, err)
	for _, v := range vols.Volumes {
		assert.Equal(t, wantTags, fromEC2Tags(v.Tags), "volume %s tags", aws.ToString(v.VolumeId))
		assert.Equal(t, ec2types.VolumeTypeGp3, v.VolumeType)
	}
	enis, err := e.raw.DescribeNetworkInterfaces(ctx, &ec2sdk.DescribeNetworkInterfacesInput{NetworkInterfaceIds: running.ENIIDs})
	require.NoError(t, err)
	require.Len(t, enis.NetworkInterfaces, 1)
	assert.Equal(t, wantTags, fromEC2Tags(enis.NetworkInterfaces[0].TagSet), "ENI tags")

	// A worker of another installation is never terminated (ErrNotOwned), even by ID.
	foreign, err := e.p.Launch(ctx, e.request(token+"-foreign", func(r *ports.LaunchRequest) {
		r.Tags[domain.TagCluster] = e.cluster + "-foreign"
		r.RootVolume = ports.VolumeSpec{}
		r.ExtraVolumes = nil
	}))
	require.NoError(t, err)
	failed, err := e.p.Terminate(ctx, e.cluster, []string{inst.ID, foreign.ID, "i-0123456789abcdef0"})
	require.NoError(t, err)
	assert.ErrorIs(t, failed[foreign.ID], ports.ErrNotOwned)
	assert.ErrorIs(t, failed["i-0123456789abcdef0"], ports.ErrNotFound)
	assert.NotContains(t, failed, inst.ID)
	still, err := e.p.Describe(ctx, ports.InstanceFilter{Cluster: e.cluster + "-foreign"})
	require.NoError(t, err)
	require.Len(t, still, 1, "the foreign instance survives")
	failed, err = e.p.Terminate(ctx, e.cluster+"-foreign", []string{foreign.ID})
	require.NoError(t, err)
	assert.Empty(t, failed)

	// NFR-C1: once terminated, the worker's volumes and ENI are gone (zero idle cost).
	waitFor(t, ctx, "volumes and ENI released", 5*time.Minute, func() (bool, error) {
		v, err := e.raw.DescribeVolumes(ctx, &ec2sdk.DescribeVolumesInput{Filters: []ec2types.Filter{filter("volume-id", running.VolumeIDs...)}})
		if err != nil {
			return false, err
		}
		for _, vol := range v.Volumes {
			if vol.State != ec2types.VolumeStateDeleted {
				return false, nil
			}
		}
		n, err := e.raw.DescribeNetworkInterfaces(ctx, &ec2sdk.DescribeNetworkInterfacesInput{Filters: []ec2types.Filter{filter("network-interface-id", running.ENIIDs...)}})
		if err != nil {
			return false, err
		}
		return len(n.NetworkInterfaces) == 0, nil
	})
	left, err := e.p.Describe(ctx, ports.InstanceFilter{Cluster: e.cluster})
	require.NoError(t, err)
	assert.Empty(t, left, "no live instance for the cluster")
	orphans, err := e.p.ListOrphans(ctx, e.cluster)
	require.NoError(t, err)
	assert.Empty(t, orphans)

	// Image resolution and live prices.
	img, err := e.p.ResolveImage(ctx, ports.ImageSelector{ID: e.ami})
	require.NoError(t, err)
	assert.Equal(t, "arm64", img.Arch)
	assert.Equal(t, "linux", img.Platform)
	assert.NotEmpty(t, img.SnapshotIDs)
	// r7i.large is not in the embedded table: a price proves the live Price List path.
	prices, err := e.p.InstancePrices(ctx, []string{"t4g.nano", "m6id.large", "r7i.large"}, false)
	require.NoError(t, err)
	assert.Greater(t, prices["t4g.nano"].USDPerHour, 0.0)
	assert.Equal(t, 118, prices["m6id.large"].NVMeGiB)
	assert.Equal(t, ports.InstancePrice{Type: "r7i.large", USDPerHour: prices["r7i.large"].USDPerHour, VCPU: 2, MemoryGiB: 16}, prices["r7i.large"])
	assert.Greater(t, prices["r7i.large"].USDPerHour, 0.0)
	win, err := e.p.InstancePrices(ctx, []string{"m6id.large"}, true)
	require.NoError(t, err)
	assert.Greater(t, win["m6id.large"].USDPerHour, prices["m6id.large"].USDPerHour, "Windows includes the licence")
}

// R-TEST-8b: the ports.Compute conformance suite against real EC2.
func TestAcceptanceConformance(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	e := newAcceptanceEnv(t, ctx)
	porttest.RunCompute(t, func(t *testing.T) porttest.ComputeHarness {
		req := e.request("", func(r *ports.LaunchRequest) {
			r.InstanceTypes = []string{"t4g.nano", "t4g.micro"}
			r.RootVolume, r.ExtraVolumes = ports.VolumeSpec{}, nil
			r.Tags = map[string]string{domain.TagCluster: e.cluster, domain.TagManagedBy: domain.ManagedByValue,
				domain.TagPool: string(r.Pool), domain.TagRole: "worker", "Name": "cucina-ec2-porttest"}
		})
		return porttest.ComputeHarness{
			Compute: e.p,
			Cluster: e.cluster,
			Request: req,
			Await: func(t *testing.T, what string, cond func() bool) {
				t.Helper()
				waitFor(t, ctx, what, 5*time.Minute, func() (bool, error) { return cond(), nil })
			},
			PriceTypes: []string{"t4g.nano", "c7i.8xlarge"},
		}
	})
}
