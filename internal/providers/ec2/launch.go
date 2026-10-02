// SPDX-License-Identifier: FSL-1.1-ALv2

package ec2

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"maps"
	"net/netip"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2sdk "github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
)

// maxUserData is EC2's limit on raw (pre-base64) user data.
const maxUserData = 16 << 10

// clientToken derives the EC2 ClientToken (at most 64 ASCII characters) from a
// launch token. The cluster is hashed in, so two installations sharing an account
// never collide on a token such as "<pool>-<generation>-<n>".
func clientToken(cluster, token string) string {
	sum := sha256.Sum256([]byte(cluster + "\x00" + token))
	return hex.EncodeToString(sum[:])
}

// Launch starts exactly one instance (ADR 0520). It is idempotent per req.Token:
// every RunInstances call of the walk carries the same ClientToken, so EC2 completes
// at most one of them per Availability Zone, and a retry returns the instance the
// first call created (found through the client-token filter, or through EC2's
// IdempotentParameterMismatch answer when a later combination had succeeded).
//
// The walk tries InstanceTypes x SubnetIDs in order (types outer), spot first when
// requested and then on-demand if FallbackOnDemand is set. Capacity and quota errors
// move to the next combination; anything else stops the walk. When every combination
// fails it returns a *CapacityError listing what was tried; it never sleeps between
// attempts — backoff is the caller's (R-SCALE-4).
func (p *Provider) Launch(ctx context.Context, req ports.LaunchRequest) (ports.Instance, error) {
	tags, err := p.launchTags(req)
	if err != nil {
		return ports.Instance{}, err
	}
	if err := validateLaunch(req); err != nil {
		return ports.Instance{}, err
	}
	cluster := tags[domain.TagCluster]
	token := clientToken(cluster, req.Token)

	if inst, ok, err := p.byClientToken(ctx, cluster, token); err != nil {
		return ports.Instance{}, err
	} else if ok {
		return inst, nil
	}

	img, err := p.imageInfo(ctx, req.ImageID)
	if err != nil {
		return ports.Instance{}, err
	}
	bdm, err := blockDevices(req.RootVolume, req.ExtraVolumes, img)
	if err != nil {
		return ports.Instance{}, err
	}
	ec2Tags := toEC2Tags(tags)

	var tried []Attempt
	for _, a := range attempts(req) {
		if err := p.runBucket.Wait(ctx); err != nil {
			return ports.Instance{}, fmt.Errorf("ec2 RunInstances: %w", err)
		}
		out, err := p.api.RunInstances(ctx, runInput(req, a, token, ec2Tags, bdm))
		if err == nil {
			if len(out.Instances) != 1 {
				return ports.Instance{}, fmt.Errorf("ec2 RunInstances: expected 1 instance, got %d", len(out.Instances))
			}
			inst := toInstance(out.Instances[0])
			fillLaunched(&inst, req, tags)
			p.log.Info("launched instance", "pool", req.Pool, "instance", inst.ID, "type", a.InstanceType,
				"subnet", a.SubnetID, "capacity", a.CapacityType, "attempts", len(tried)+1)
			return inst, nil
		}
		kind := classify(err)
		if errorCode(err) == "Unsupported" {
			kind = kindCapacity // type not offered in this AZ / configuration: try the next one
		}
		switch kind {
		case kindCapacity, kindQuota:
			a.Code = errorCode(err)
			tried = append(tried, a)
			p.noteAPIError("RunInstances", err)
			continue
		case kindIdempotentMismatch:
			// An earlier call with this token succeeded with other parameters (another
			// type/subnet): that instance is the answer.
			return p.awaitClientToken(ctx, cluster, token, req, tags)
		case kindTokenTerminated:
			return ports.Instance{}, fmt.Errorf("ec2 RunInstances: %w: launch token %q already launched an instance that has terminated; use a new token",
				ports.ErrInvalid, req.Token)
		}
		return ports.Instance{}, p.apiErrKind("RunInstances", p.runBucket, kind, err)
	}
	return ports.Instance{}, &CapacityError{Tried: tried}
}

// attempts expands a request into the ordered (type, subnet, capacity) walk.
func attempts(req ports.LaunchRequest) []Attempt {
	markets := []ports.CapacityType{ports.OnDemand}
	if req.CapacityType == ports.Spot {
		markets = []ports.CapacityType{ports.Spot}
		if req.FallbackOnDemand {
			markets = append(markets, ports.OnDemand)
		}
	}
	out := make([]Attempt, 0, len(markets)*len(req.InstanceTypes)*len(req.SubnetIDs))
	for _, m := range markets {
		for _, t := range req.InstanceTypes {
			for _, s := range req.SubnetIDs {
				out = append(out, Attempt{InstanceType: t, SubnetID: s, CapacityType: m})
			}
		}
	}
	return out
}

// launchTags merges the provider's extra tags, the request's tags and the
// ownership tags the provider guarantees on every instance, volume and ENI.
func (p *Provider) launchTags(req ports.LaunchRequest) (map[string]string, error) {
	tags := make(map[string]string, len(p.opts.ExtraTags)+len(req.Tags)+5)
	maps.Copy(tags, p.opts.ExtraTags)
	maps.Copy(tags, req.Tags)
	own := map[string]string{
		domain.TagManagedBy:   domain.ManagedByValue,
		domain.TagPool:        string(req.Pool),
		domain.TagLaunchToken: req.Token,
	}
	if req.Generation != "" {
		own[domain.TagGeneration] = req.Generation
	}
	for k, v := range own {
		if cur, ok := req.Tags[k]; ok && cur != v {
			return nil, fmt.Errorf("%w: tag %s=%q contradicts the launch request (%q)", ports.ErrInvalid, k, cur, v)
		}
		tags[k] = v
	}
	if _, ok := tags[domain.TagRole]; !ok {
		tags[domain.TagRole] = "worker"
	}
	if tags[domain.TagCluster] == "" {
		return nil, fmt.Errorf("%w: launch request lacks the %s tag", ports.ErrInvalid, domain.TagCluster)
	}
	if err := validateTags(tags); err != nil {
		return nil, err
	}
	return tags, nil
}

func validateLaunch(req ports.LaunchRequest) error {
	var problems []string
	if req.Pool == "" {
		problems = append(problems, "pool is empty")
	}
	if req.Token == "" {
		problems = append(problems, "idempotency token is empty")
	}
	if req.ImageID == "" {
		problems = append(problems, "image ID is empty")
	}
	if len(req.InstanceTypes) == 0 || containsEmpty(req.InstanceTypes) {
		problems = append(problems, "instance types missing or empty")
	}
	if len(req.SubnetIDs) == 0 || containsEmpty(req.SubnetIDs) {
		problems = append(problems, "subnet IDs missing or empty")
	}
	switch req.CapacityType {
	case "", ports.OnDemand, ports.Spot:
	default:
		problems = append(problems, fmt.Sprintf("unknown capacity type %q", req.CapacityType))
	}
	if len(req.UserData) > maxUserData {
		problems = append(problems, fmt.Sprintf("user data is %d bytes (max %d)", len(req.UserData), maxUserData))
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: launch request: %s", ports.ErrInvalid, strings.Join(problems, "; "))
	}
	return nil
}

func containsEmpty(xs []string) bool {
	for _, x := range xs {
		if x == "" {
			return true
		}
	}
	return false
}

// runInput builds the RunInstances request for one attempt (R-POOL-1/2/3).
func runInput(req ports.LaunchRequest, a Attempt, token string, tags []ec2types.Tag, bdm []ec2types.BlockDeviceMapping) *ec2sdk.RunInstancesInput {
	specs := []ec2types.TagSpecification{
		{ResourceType: ec2types.ResourceTypeInstance, Tags: tags},
		{ResourceType: ec2types.ResourceTypeVolume, Tags: tags},
		{ResourceType: ec2types.ResourceTypeNetworkInterface, Tags: tags},
	}
	in := &ec2sdk.RunInstancesInput{
		ImageId:      aws.String(req.ImageID),
		InstanceType: ec2types.InstanceType(a.InstanceType),
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		ClientToken:  aws.String(token),
		// An explicit primary ENI: AssociatePublicIpAddress is always set, so a subnet
		// that maps public IPs on launch still gives workers none unless asked (R-DATA-4).
		NetworkInterfaces: []ec2types.InstanceNetworkInterfaceSpecification{{
			DeviceIndex:              aws.Int32(0),
			SubnetId:                 aws.String(a.SubnetID),
			Groups:                   req.SecurityGroupIDs,
			AssociatePublicIpAddress: aws.Bool(req.AssociatePublicIP),
			DeleteOnTermination:      aws.Bool(true),
		}},
		MetadataOptions: &ec2types.InstanceMetadataOptionsRequest{
			HttpEndpoint:            ec2types.InstanceMetadataEndpointStateEnabled,
			HttpTokens:              ec2types.HttpTokensStateRequired,
			HttpPutResponseHopLimit: aws.Int32(1),
			InstanceMetadataTags:    ec2types.InstanceMetadataTagsStateEnabled,
		},
		InstanceInitiatedShutdownBehavior: ec2types.ShutdownBehaviorTerminate,
		BlockDeviceMappings:               bdm,
		TagSpecifications:                 specs,
	}
	if len(req.UserData) > 0 {
		in.UserData = aws.String(base64.StdEncoding.EncodeToString(req.UserData))
	}
	if req.InstanceProfile != "" {
		if strings.HasPrefix(req.InstanceProfile, "arn:") {
			in.IamInstanceProfile = &ec2types.IamInstanceProfileSpecification{Arn: aws.String(req.InstanceProfile)}
		} else {
			in.IamInstanceProfile = &ec2types.IamInstanceProfileSpecification{Name: aws.String(req.InstanceProfile)}
		}
	}
	if a.CapacityType == ports.Spot {
		in.InstanceMarketOptions = &ec2types.InstanceMarketOptionsRequest{
			MarketType: ec2types.MarketTypeSpot,
			SpotOptions: &ec2types.SpotMarketOptions{
				SpotInstanceType:             ec2types.SpotInstanceTypeOneTime,
				InstanceInterruptionBehavior: ec2types.InstanceInterruptionBehaviorTerminate,
			},
		}
		in.TagSpecifications = append(in.TagSpecifications, ec2types.TagSpecification{ResourceType: ec2types.ResourceTypeSpotInstancesRequest, Tags: tags})
	}
	// Deliberately no DisableApiStop: EC2's stop protection also refuses
	// TerminateInstances (OperationNotPermitted, found by the acceptance run). Workers
	// never stop because OS shutdown terminates them and the controller's role has no
	// ec2:StopInstances.
	return in
}

// byClientToken finds the instance a previous Launch created for token.
func (p *Provider) byClientToken(ctx context.Context, cluster, token string) (ports.Instance, bool, error) {
	insts, err := p.describeInstances(ctx, append(ownerFilters(cluster), filter("client-token", token)))
	if err != nil {
		return ports.Instance{}, false, err
	}
	if len(insts) == 0 {
		return ports.Instance{}, false, nil
	}
	best := insts[0]
	live := 0
	for _, i := range insts {
		if instanceLive(i) {
			live++
			if !instanceLive(best) || aws.ToTime(i.LaunchTime).Before(aws.ToTime(best.LaunchTime)) {
				best = i
			}
		}
	}
	if live > 1 {
		p.log.Error("several live instances share one launch token (cross-AZ retry race)", "count", live, "cluster", cluster)
	}
	return toInstance(best), true, nil
}

// awaitClientToken waits (bounded) for the instance of an already-used token to
// become visible: DescribeInstances is eventually consistent.
func (p *Provider) awaitClientToken(ctx context.Context, cluster, token string, req ports.LaunchRequest, tags map[string]string) (ports.Instance, error) {
	delay := 250 * time.Millisecond
	for range 7 {
		inst, ok, err := p.byClientToken(ctx, cluster, token)
		if err != nil {
			return ports.Instance{}, err
		}
		if ok {
			fillLaunched(&inst, req, tags)
			return inst, nil
		}
		if err := p.clock.Sleep(ctx, delay); err != nil {
			return ports.Instance{}, fmt.Errorf("ec2 launch: %w", err)
		}
		delay = min(2*delay, 4*time.Second)
	}
	// Transient by design (no sentinel): the next Launch with the same token finds it.
	return ports.Instance{}, fmt.Errorf("ec2 launch: token %q already launched an instance that is not visible yet", req.Token)
}

func instanceLive(i ec2types.Instance) bool {
	return i.State == nil || i.State.Name != ec2types.InstanceStateNameTerminated
}

// fillLaunched completes an instance from the request when the RunInstances
// response omits tags.
func fillLaunched(inst *ports.Instance, req ports.LaunchRequest, tags map[string]string) {
	if len(inst.Tags) == 0 {
		inst.Tags = maps.Clone(tags)
	}
	if inst.Pool == "" {
		inst.Pool = req.Pool
	}
	if inst.Generation == "" {
		inst.Generation = req.Generation
	}
}

// toInstance converts an EC2 instance; pool and generation come from its tags.
func toInstance(i ec2types.Instance) ports.Instance {
	tags := fromEC2Tags(i.Tags)
	out := ports.Instance{
		ID:           aws.ToString(i.InstanceId),
		Pool:         domain.PoolName(tags[domain.TagPool]),
		Generation:   tags[domain.TagGeneration],
		Type:         string(i.InstanceType),
		SubnetID:     aws.ToString(i.SubnetId),
		ImageID:      aws.ToString(i.ImageId),
		Tags:         tags,
		CapacityType: ports.OnDemand,
		LaunchTime:   aws.ToTime(i.LaunchTime),
	}
	if i.State != nil {
		out.State = ports.InstanceState(i.State.Name)
	}
	if i.Placement != nil {
		out.AZ = aws.ToString(i.Placement.AvailabilityZone)
	}
	if ip, err := netip.ParseAddr(aws.ToString(i.PrivateIpAddress)); err == nil {
		out.PrivateIP = ip
	}
	if i.InstanceLifecycle == ec2types.InstanceLifecycleTypeSpot {
		out.CapacityType = ports.Spot
	}
	for _, b := range i.BlockDeviceMappings {
		if b.Ebs != nil && b.Ebs.VolumeId != nil {
			out.VolumeIDs = append(out.VolumeIDs, *b.Ebs.VolumeId)
		}
	}
	for _, n := range i.NetworkInterfaces {
		if n.NetworkInterfaceId != nil {
			out.ENIIDs = append(out.ENIIDs, *n.NetworkInterfaceId)
		}
	}
	return out
}

// ------------------------------------------------------------- block devices

// blockDevices maps the request's volumes and makes every EBS volume of the
// instance DeleteOnTermination — including mappings inherited from the AMI, so a
// terminated worker leaves no volume behind (zero idle cost, NFR-C1).
func blockDevices(root ports.VolumeSpec, extra []ports.VolumeSpec, img imageInfo) ([]ec2types.BlockDeviceMapping, error) {
	rootDev := root.DeviceName
	if rootDev == "" {
		rootDev = img.RootDevice
	}
	if rootDev == "" {
		return nil, fmt.Errorf("%w: image %s has no root device name", ports.ErrInvalid, img.ID)
	}
	rootEBS, err := ebsFor(root, true, img.snapshotGiB(rootDev))
	if err != nil {
		return nil, fmt.Errorf("root volume: %w", err)
	}
	out := []ec2types.BlockDeviceMapping{{DeviceName: aws.String(rootDev), Ebs: rootEBS}}
	used := map[string]bool{normDevice(rootDev): true}
	for _, m := range img.Mappings {
		if m.DeviceName != nil && m.Ebs == nil {
			used[normDevice(*m.DeviceName)] = true // instance store or suppressed device
		}
	}
	for i, v := range extra {
		dev := v.DeviceName
		if dev == "" {
			if dev = nextFreeDevice(used); dev == "" {
				return nil, fmt.Errorf("%w: no free device name for extra volume %d", ports.ErrInvalid, i)
			}
		}
		if used[normDevice(dev)] {
			return nil, fmt.Errorf("%w: device %s mapped twice", ports.ErrInvalid, dev)
		}
		e, err := ebsFor(v, false, 0)
		if err != nil {
			return nil, fmt.Errorf("extra volume %d: %w", i, err)
		}
		out = append(out, ec2types.BlockDeviceMapping{DeviceName: aws.String(dev), Ebs: e})
		used[normDevice(dev)] = true
	}
	for _, m := range img.Mappings {
		if m.Ebs == nil || m.DeviceName == nil || used[normDevice(*m.DeviceName)] {
			continue
		}
		out = append(out, ec2types.BlockDeviceMapping{
			DeviceName: m.DeviceName,
			Ebs:        &ec2types.EbsBlockDevice{DeleteOnTermination: aws.Bool(true)},
		})
		used[normDevice(*m.DeviceName)] = true
	}
	return out, nil
}

func ebsFor(v ports.VolumeSpec, root bool, snapshotGiB int) (*ec2types.EbsBlockDevice, error) {
	e := &ec2types.EbsBlockDevice{DeleteOnTermination: aws.Bool(true)}
	typ := v.Type
	if typ == "" {
		typ = string(ec2types.VolumeTypeGp3) // NFR-C4: gp3 baseline
	}
	e.VolumeType = ec2types.VolumeType(typ)
	switch {
	case v.SizeGiB < 0 || v.IOPS < 0 || v.Throughput < 0:
		return nil, fmt.Errorf("%w: negative size, IOPS or throughput", ports.ErrInvalid)
	case v.SizeGiB > 0:
		if root && snapshotGiB > 0 && v.SizeGiB < snapshotGiB {
			return nil, fmt.Errorf("%w: %d GiB is smaller than the image's %d GiB snapshot", ports.ErrInvalid, v.SizeGiB, snapshotGiB)
		}
		e.VolumeSize = aws.Int32(int32(v.SizeGiB))
	case !root:
		return nil, fmt.Errorf("%w: an extra volume needs SizeGiB", ports.ErrInvalid)
	}
	if v.IOPS > 0 {
		e.Iops = aws.Int32(int32(v.IOPS))
	}
	if v.Throughput > 0 {
		e.Throughput = aws.Int32(int32(v.Throughput))
	}
	if v.InitializationRate != 0 {
		if !root {
			return nil, fmt.Errorf("%w: an initialization rate needs a snapshot-backed (root) volume", ports.ErrInvalid)
		}
		if v.InitializationRate < 100 || v.InitializationRate > 300 {
			return nil, fmt.Errorf("%w: initialization rate %d MiB/s outside 100–300", ports.ErrInvalid, v.InitializationRate)
		}
		e.VolumeInitializationRate = aws.Int32(int32(v.InitializationRate))
	}
	return e, nil
}

// normDevice folds the equivalent spellings /dev/sdf, /dev/xvdf, sdf and xvdf.
func normDevice(d string) string {
	d = strings.TrimPrefix(d, "/dev/")
	if rest, ok := strings.CutPrefix(d, "xvd"); ok {
		return "sd" + rest
	}
	return d
}

func nextFreeDevice(used map[string]bool) string {
	for c := 'f'; c <= 'p'; c++ {
		if d := "/dev/sd" + string(c); !used[normDevice(d)] {
			return d
		}
	}
	return ""
}
