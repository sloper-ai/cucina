// SPDX-License-Identifier: FSL-1.1-ALv2

package ec2

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2sdk "github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	"github.com/aws/smithy-go"

	"github.com/sloper-ai/cucina/internal/ports"
)

// fakeEC2 is a small, stateful, in-memory model of the EC2 API surface in ec2API
// (no mock framework, no LocalStack — R-TEST-8a). It models what the provider's
// contract depends on: zonal client-token idempotency, per-(type, subnet) capacity
// errors, tag filters and pagination, DeleteOnTermination on volumes/ENIs, eventual
// state transitions observed through Describe, and Fast Launch states. When the
// acceptance run against real EC2 disagrees with it, fix the fake.
type fakeEC2 struct {
	mu       sync.Mutex
	clock    *stepClock
	seq      int
	pageSize int

	subnets    map[string]fakeSubnet
	images     map[string]ec2types.Image
	instances  map[string]*fakeInstance
	order      []string
	volumes    map[string]*ec2types.Volume
	enis       map[string]*ec2types.NetworkInterface
	snapshots  []ec2types.Snapshot
	fastLaunch map[string]*fakeFastLaunch
	tokens     map[string]tokenUse // client token + "|" + AZ (zonal idempotency)

	capacity map[string]string  // "type|subnet|market" → error code ("*" wildcards)
	failNext map[string][]error // op → errors returned before any effect
	lose     map[string]int     // op → calls that take effect but answer with an error
	stale    int                // next DescribeInstances calls that see nothing (eventual consistency)
	hooks    map[string]func()  // op → called on every call (e.g. to cancel a context mid-wait)
	throttle bool               // RunInstances answers RequestLimitExceeded while set
	calls    map[string]int
	tried    []Attempt // every RunInstances combination that reached the capacity check
}

type fakeSubnet struct {
	az          string
	mapPublicIP bool
}

type fakeInstance struct {
	inst             ec2types.Instance
	volumes          map[string]bool // volume ID → DeleteOnTermination
	enis             map[string]bool // ENI ID → DeleteOnTermination
	shutdownBehavior ec2types.ShutdownBehavior
	disableAPIStop   bool
	publicIP         bool
	userData         string
	profile          string
}

type tokenUse struct{ instanceID, fingerprint string }

type fakeFastLaunch struct {
	item  ec2types.DescribeFastLaunchImagesSuccessItem
	ticks int // describes left before the transitional state settles
}

const (
	fakeOwner = "self"
	azA       = "us-west-1b"
	azB       = "us-west-1c"
)

func newFakeEC2(clock *stepClock) *fakeEC2 {
	return &fakeEC2{
		clock:      clock,
		pageSize:   1000,
		subnets:    map[string]fakeSubnet{"subnet-a": {az: azA}, "subnet-a2": {az: azA, mapPublicIP: true}, "subnet-b": {az: azB}},
		images:     map[string]ec2types.Image{},
		instances:  map[string]*fakeInstance{},
		volumes:    map[string]*ec2types.Volume{},
		enis:       map[string]*ec2types.NetworkInterface{},
		fastLaunch: map[string]*fakeFastLaunch{},
		tokens:     map[string]tokenUse{},
		capacity:   map[string]string{},
		failNext:   map[string][]error{},
		lose:       map[string]int{},
		calls:      map[string]int{},
		hooks:      map[string]func(){},
	}
}

func apiError(code string) error {
	return &smithy.GenericAPIError{Code: code, Message: "fake: " + code}
}

// errLost models a response lost after EC2 acted (connection reset).
var errLost = errors.New("fake: connection reset after the request was processed")

func (f *fakeEC2) begin(op string) error {
	f.calls[op]++
	if h := f.hooks[op]; h != nil {
		h()
	}
	if q := f.failNext[op]; len(q) > 0 {
		f.failNext[op] = q[1:]
		return q[0]
	}
	return nil
}

func (f *fakeEC2) lost(op string) bool {
	if f.lose[op] > 0 {
		f.lose[op]--
		return true
	}
	return false
}

func (f *fakeEC2) id(prefix string) string {
	f.seq++
	return fmt.Sprintf("%s-%017x", prefix, f.seq)
}

// setCapacity makes RunInstances fail with code for (type, subnet, market); use "*" as a wildcard.
func (f *fakeEC2) setCapacity(typ, subnet, market, code string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.capacity[typ+"|"+subnet+"|"+market] = code
}

func (f *fakeEC2) capacityError(typ, subnet, market string) string {
	for _, k := range []string{typ + "|" + subnet + "|" + market, typ + "|*|" + market, "*|" + subnet + "|" + market, typ + "|" + subnet + "|*", typ + "|*|*", "*|*|" + market, "*|*|*"} {
		if c, ok := f.capacity[k]; ok {
			return c
		}
	}
	return ""
}

func (f *fakeEC2) addImage(img ec2types.Image) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if img.State == "" {
		img.State = ec2types.ImageStateAvailable
	}
	if img.OwnerId == nil {
		img.OwnerId = aws.String(fakeOwner)
	}
	f.images[aws.ToString(img.ImageId)] = img
}

// ------------------------------------------------------------- instances

func (f *fakeEC2) RunInstances(_ context.Context, in *ec2sdk.RunInstancesInput, _ ...func(*ec2sdk.Options)) (*ec2sdk.RunInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("RunInstances"); err != nil {
		return nil, err
	}
	if f.throttle {
		return nil, apiError("RequestLimitExceeded")
	}
	if aws.ToInt32(in.MinCount) != 1 || aws.ToInt32(in.MaxCount) != 1 {
		return nil, apiError("InvalidParameterValue")
	}
	var nic ec2types.InstanceNetworkInterfaceSpecification
	if len(in.NetworkInterfaces) == 1 {
		nic = in.NetworkInterfaces[0]
	} else {
		nic.SubnetId = in.SubnetId
	}
	sub, ok := f.subnets[aws.ToString(nic.SubnetId)]
	if !ok {
		return nil, apiError("InvalidSubnetID.NotFound")
	}
	img, ok := f.images[aws.ToString(in.ImageId)]
	if !ok || img.State != ec2types.ImageStateAvailable {
		return nil, apiError("InvalidAMIID.NotFound")
	}
	market := "on-demand"
	if in.InstanceMarketOptions != nil && in.InstanceMarketOptions.MarketType == ec2types.MarketTypeSpot {
		market = "spot"
	}
	typ, subnet := string(in.InstanceType), aws.ToString(nic.SubnetId)
	fingerprint := strings.Join([]string{typ, aws.ToString(in.ImageId), subnet, market}, "|")
	tokenKey := aws.ToString(in.ClientToken) + "|" + sub.az
	if in.ClientToken != nil {
		if use, ok := f.tokens[tokenKey]; ok {
			if use.fingerprint != fingerprint {
				return nil, apiError("IdempotentParameterMismatch")
			}
			return &ec2sdk.RunInstancesOutput{Instances: []ec2types.Instance{f.instances[use.instanceID].inst}}, nil
		}
	}
	f.tried = append(f.tried, Attempt{InstanceType: typ, SubnetID: subnet, CapacityType: capacityTypeOf(market)})
	if code := f.capacityError(typ, subnet, market); code != "" {
		return nil, apiError(code)
	}

	tags := map[ec2types.ResourceType][]ec2types.Tag{}
	for _, s := range in.TagSpecifications {
		tags[s.ResourceType] = append(tags[s.ResourceType], s.Tags...)
	}
	id := f.id("i")
	now := f.clock.Now()
	fi := &fakeInstance{volumes: map[string]bool{}, enis: map[string]bool{}, shutdownBehavior: in.InstanceInitiatedShutdownBehavior,
		disableAPIStop: aws.ToBool(in.DisableApiStop), userData: aws.ToString(in.UserData)}
	if in.IamInstanceProfile != nil {
		fi.profile = aws.ToString(in.IamInstanceProfile.Name) + aws.ToString(in.IamInstanceProfile.Arn)
	}
	fi.publicIP = sub.mapPublicIP
	if nic.AssociatePublicIpAddress != nil {
		fi.publicIP = *nic.AssociatePublicIpAddress
	}
	inst := ec2types.Instance{
		InstanceId:       aws.String(id),
		InstanceType:     in.InstanceType,
		ImageId:          in.ImageId,
		SubnetId:         aws.String(subnet),
		Placement:        &ec2types.Placement{AvailabilityZone: aws.String(sub.az)},
		State:            &ec2types.InstanceState{Name: ec2types.InstanceStateNamePending},
		LaunchTime:       aws.Time(now),
		ClientToken:      in.ClientToken,
		Tags:             tags[ec2types.ResourceTypeInstance],
		PrivateIpAddress: aws.String(fmt.Sprintf("10.0.%d.%d", f.seq/250, f.seq%250+1)),
	}
	if in.MetadataOptions != nil {
		inst.MetadataOptions = &ec2types.InstanceMetadataOptionsResponse{
			HttpTokens: in.MetadataOptions.HttpTokens, HttpPutResponseHopLimit: in.MetadataOptions.HttpPutResponseHopLimit,
			HttpEndpoint: in.MetadataOptions.HttpEndpoint, InstanceMetadataTags: in.MetadataOptions.InstanceMetadataTags,
		}
	}
	if market == "spot" {
		inst.InstanceLifecycle = ec2types.InstanceLifecycleTypeSpot
	}
	if fi.publicIP {
		inst.PublicIpAddress = aws.String("198.51.100.1")
	}

	// Volumes: the AMI's mappings, overridden by the request (matched by device).
	type vol struct {
		dev string
		ebs ec2types.EbsBlockDevice
	}
	var vols []vol
	for _, m := range img.BlockDeviceMappings {
		if m.Ebs != nil {
			vols = append(vols, vol{dev: aws.ToString(m.DeviceName), ebs: *m.Ebs})
		}
	}
	for _, m := range in.BlockDeviceMappings {
		if m.Ebs == nil {
			continue
		}
		i := slices.IndexFunc(vols, func(v vol) bool { return normDevice(v.dev) == normDevice(aws.ToString(m.DeviceName)) })
		if i < 0 {
			if m.Ebs.VolumeSize == nil {
				return nil, apiError("InvalidBlockDeviceMapping")
			}
			vols = append(vols, vol{dev: aws.ToString(m.DeviceName), ebs: *m.Ebs})
			continue
		}
		o := &vols[i].ebs
		if m.Ebs.DeleteOnTermination != nil {
			o.DeleteOnTermination = m.Ebs.DeleteOnTermination
		}
		if m.Ebs.VolumeSize != nil {
			if aws.ToInt32(m.Ebs.VolumeSize) < aws.ToInt32(o.VolumeSize) {
				return nil, apiError("InvalidBlockDeviceMapping")
			}
			o.VolumeSize = m.Ebs.VolumeSize
		}
		if m.Ebs.VolumeType != "" {
			o.VolumeType = m.Ebs.VolumeType
		}
		o.Iops, o.Throughput, o.VolumeInitializationRate = m.Ebs.Iops, m.Ebs.Throughput, m.Ebs.VolumeInitializationRate
	}
	for _, v := range vols {
		vid := f.id("vol")
		f.volumes[vid] = &ec2types.Volume{
			VolumeId: aws.String(vid), Size: v.ebs.VolumeSize, VolumeType: v.ebs.VolumeType, Iops: v.ebs.Iops,
			Throughput: v.ebs.Throughput, VolumeInitializationRate: v.ebs.VolumeInitializationRate,
			State: ec2types.VolumeStateInUse, CreateTime: aws.Time(now), AvailabilityZone: aws.String(sub.az),
			Tags: tags[ec2types.ResourceTypeVolume],
			Attachments: []ec2types.VolumeAttachment{{InstanceId: aws.String(id), Device: aws.String(v.dev),
				DeleteOnTermination: v.ebs.DeleteOnTermination, State: ec2types.VolumeAttachmentStateAttached}},
		}
		fi.volumes[vid] = aws.ToBool(v.ebs.DeleteOnTermination)
		inst.BlockDeviceMappings = append(inst.BlockDeviceMappings, ec2types.InstanceBlockDeviceMapping{
			DeviceName: aws.String(v.dev), Ebs: &ec2types.EbsInstanceBlockDevice{VolumeId: aws.String(vid), DeleteOnTermination: v.ebs.DeleteOnTermination},
		})
	}
	eid := f.id("eni")
	dot := nic.DeleteOnTermination == nil || *nic.DeleteOnTermination
	f.enis[eid] = &ec2types.NetworkInterface{
		NetworkInterfaceId: aws.String(eid), Status: ec2types.NetworkInterfaceStatusInUse, SubnetId: aws.String(subnet),
		AvailabilityZone: aws.String(sub.az), TagSet: tags[ec2types.ResourceTypeNetworkInterface],
		Attachment: &ec2types.NetworkInterfaceAttachment{InstanceId: aws.String(id), Status: ec2types.AttachmentStatusAttached, DeleteOnTermination: aws.Bool(dot)},
	}
	fi.enis[eid] = dot
	inst.NetworkInterfaces = []ec2types.InstanceNetworkInterface{{NetworkInterfaceId: aws.String(eid)}}
	fi.inst = inst
	f.instances[id] = fi
	f.order = append(f.order, id)
	if in.ClientToken != nil {
		f.tokens[tokenKey] = tokenUse{instanceID: id, fingerprint: fingerprint}
	}
	if f.lost("RunInstances") {
		return nil, errLost
	}
	// Like EC2, the response does not list the volumes yet.
	resp := inst
	resp.BlockDeviceMappings = nil
	return &ec2sdk.RunInstancesOutput{Instances: []ec2types.Instance{resp}}, nil
}

func capacityTypeOf(market string) ports.CapacityType {
	if market == "spot" {
		return ports.Spot
	}
	return ports.OnDemand
}

// tick advances eventual states: pending → running, shutting-down → terminated.
func (f *fakeEC2) tick() {
	for _, id := range f.order {
		fi := f.instances[id]
		switch fi.inst.State.Name {
		case ec2types.InstanceStateNamePending:
			fi.inst.State = &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning}
		case ec2types.InstanceStateNameShuttingDown:
			fi.inst.State = &ec2types.InstanceState{Name: ec2types.InstanceStateNameTerminated}
			for vid, dot := range fi.volumes {
				if dot {
					delete(f.volumes, vid)
				} else if v := f.volumes[vid]; v != nil {
					v.State, v.Attachments = ec2types.VolumeStateAvailable, nil
				}
			}
			for eid, dot := range fi.enis {
				if dot {
					delete(f.enis, eid)
				} else if n := f.enis[eid]; n != nil {
					n.Status, n.Attachment = ec2types.NetworkInterfaceStatusAvailable, nil
				}
			}
			fi.inst.BlockDeviceMappings, fi.inst.NetworkInterfaces = nil, nil
		}
	}
}

func (f *fakeEC2) DescribeInstances(_ context.Context, in *ec2sdk.DescribeInstancesInput, _ ...func(*ec2sdk.Options)) (*ec2sdk.DescribeInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("DescribeInstances"); err != nil {
		return nil, err
	}
	if f.stale > 0 {
		f.stale--
		return &ec2sdk.DescribeInstancesOutput{}, nil
	}
	f.tick()
	var matched []ec2types.Instance
	for _, id := range f.order {
		inst := f.instances[id].inst
		ok, err := matchFilters(in.Filters, func(name string) ([]string, bool) {
			switch name {
			case "instance-id":
				return []string{id}, true
			case "instance-state-name":
				return []string{string(inst.State.Name)}, true
			case "client-token":
				return []string{aws.ToString(inst.ClientToken)}, true
			case "subnet-id":
				return []string{aws.ToString(inst.SubnetId)}, true
			}
			return nil, false
		}, inst.Tags)
		if err != nil {
			return nil, err
		}
		if ok && (len(in.InstanceIds) == 0 || slices.Contains(in.InstanceIds, id)) {
			matched = append(matched, inst)
		}
	}
	page, next, err := paginate(matched, in.NextToken, f.pageSize)
	if err != nil {
		return nil, err
	}
	out := &ec2sdk.DescribeInstancesOutput{NextToken: next}
	for _, i := range page {
		out.Reservations = append(out.Reservations, ec2types.Reservation{Instances: []ec2types.Instance{i}})
	}
	return out, nil
}

func (f *fakeEC2) TerminateInstances(_ context.Context, in *ec2sdk.TerminateInstancesInput, _ ...func(*ec2sdk.Options)) (*ec2sdk.TerminateInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("TerminateInstances"); err != nil {
		return nil, err
	}
	for _, id := range in.InstanceIds {
		switch fi := f.instances[id]; {
		case fi == nil:
			return nil, apiError("InvalidInstanceID.NotFound")
		case fi.disableAPIStop:
			// Verified on EC2 (2026-10-02): stop protection also refuses termination.
			return nil, apiError("OperationNotPermitted")
		}
	}
	out := &ec2sdk.TerminateInstancesOutput{}
	for _, id := range in.InstanceIds {
		fi := f.instances[id]
		if fi.inst.State.Name != ec2types.InstanceStateNameTerminated {
			fi.inst.State = &ec2types.InstanceState{Name: ec2types.InstanceStateNameShuttingDown}
		}
		out.TerminatingInstances = append(out.TerminatingInstances, ec2types.InstanceStateChange{InstanceId: aws.String(id)})
	}
	return out, nil
}

// ------------------------------------------------------------- volumes, ENIs

func (f *fakeEC2) DescribeVolumes(_ context.Context, in *ec2sdk.DescribeVolumesInput, _ ...func(*ec2sdk.Options)) (*ec2sdk.DescribeVolumesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("DescribeVolumes"); err != nil {
		return nil, err
	}
	var matched []ec2types.Volume
	for _, id := range sortedKeys(f.volumes) {
		v := f.volumes[id]
		ok, err := matchFilters(in.Filters, func(name string) ([]string, bool) {
			switch name {
			case "volume-id":
				return []string{id}, true
			case "status":
				return []string{string(v.State)}, true
			}
			return nil, false
		}, v.Tags)
		if err != nil {
			return nil, err
		}
		if ok {
			matched = append(matched, *v)
		}
	}
	page, next, err := paginate(matched, in.NextToken, f.pageSize)
	return &ec2sdk.DescribeVolumesOutput{Volumes: page, NextToken: next}, err
}

func (f *fakeEC2) DeleteVolume(_ context.Context, in *ec2sdk.DeleteVolumeInput, _ ...func(*ec2sdk.Options)) (*ec2sdk.DeleteVolumeOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("DeleteVolume"); err != nil {
		return nil, err
	}
	v := f.volumes[aws.ToString(in.VolumeId)]
	switch {
	case v == nil:
		return nil, apiError("InvalidVolume.NotFound")
	case v.State != ec2types.VolumeStateAvailable:
		return nil, apiError("VolumeInUse")
	}
	delete(f.volumes, aws.ToString(in.VolumeId))
	return &ec2sdk.DeleteVolumeOutput{}, nil
}

func (f *fakeEC2) DescribeNetworkInterfaces(_ context.Context, in *ec2sdk.DescribeNetworkInterfacesInput, _ ...func(*ec2sdk.Options)) (*ec2sdk.DescribeNetworkInterfacesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("DescribeNetworkInterfaces"); err != nil {
		return nil, err
	}
	var matched []ec2types.NetworkInterface
	for _, id := range sortedKeys(f.enis) {
		n := f.enis[id]
		ok, err := matchFilters(in.Filters, func(name string) ([]string, bool) {
			switch name {
			case "network-interface-id":
				return []string{id}, true
			case "status":
				return []string{string(n.Status)}, true
			}
			return nil, false
		}, n.TagSet)
		if err != nil {
			return nil, err
		}
		if ok {
			matched = append(matched, *n)
		}
	}
	page, next, err := paginate(matched, in.NextToken, f.pageSize)
	return &ec2sdk.DescribeNetworkInterfacesOutput{NetworkInterfaces: page, NextToken: next}, err
}

func (f *fakeEC2) DeleteNetworkInterface(_ context.Context, in *ec2sdk.DeleteNetworkInterfaceInput, _ ...func(*ec2sdk.Options)) (*ec2sdk.DeleteNetworkInterfaceOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("DeleteNetworkInterface"); err != nil {
		return nil, err
	}
	n := f.enis[aws.ToString(in.NetworkInterfaceId)]
	switch {
	case n == nil:
		return nil, apiError("InvalidNetworkInterfaceID.NotFound")
	case n.Status != ec2types.NetworkInterfaceStatusAvailable:
		return nil, apiError("InvalidNetworkInterface.InUse")
	}
	delete(f.enis, aws.ToString(in.NetworkInterfaceId))
	return &ec2sdk.DeleteNetworkInterfaceOutput{}, nil
}

// addOrphan creates an unattached, tagged volume and ENI (what a crash between
// launch and attachment, or a DeleteOnTermination=false mapping, leaves behind).
func (f *fakeEC2) addOrphan(tags map[string]string, created time.Time) (volID, eniID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	volID, eniID = f.id("vol"), f.id("eni")
	f.volumes[volID] = &ec2types.Volume{VolumeId: aws.String(volID), State: ec2types.VolumeStateAvailable, CreateTime: aws.Time(created),
		Size: aws.Int32(8), VolumeType: ec2types.VolumeTypeGp3, Tags: toEC2Tags(tags)}
	f.enis[eniID] = &ec2types.NetworkInterface{NetworkInterfaceId: aws.String(eniID), Status: ec2types.NetworkInterfaceStatusAvailable, TagSet: toEC2Tags(tags)}
	return volID, eniID
}

// ------------------------------------------------------------- images, snapshots

func (f *fakeEC2) DescribeImages(_ context.Context, in *ec2sdk.DescribeImagesInput, _ ...func(*ec2sdk.Options)) (*ec2sdk.DescribeImagesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("DescribeImages"); err != nil {
		return nil, err
	}
	for _, id := range in.ImageIds {
		if _, ok := f.images[id]; !ok {
			return nil, apiError("InvalidAMIID.NotFound")
		}
	}
	var matched []ec2types.Image
	for _, id := range sortedKeys(f.images) {
		img := f.images[id]
		if len(in.ImageIds) > 0 && !slices.Contains(in.ImageIds, id) {
			continue
		}
		if slices.Contains(in.Owners, "self") && aws.ToString(img.OwnerId) != fakeOwner {
			continue
		}
		ok, err := matchFilters(in.Filters, func(name string) ([]string, bool) {
			if name == "state" {
				return []string{string(img.State)}, true
			}
			return nil, false
		}, img.Tags)
		if err != nil {
			return nil, err
		}
		if ok {
			matched = append(matched, img)
		}
	}
	page, next, err := paginate(matched, in.NextToken, f.pageSize)
	return &ec2sdk.DescribeImagesOutput{Images: page, NextToken: next}, err
}

func (f *fakeEC2) DescribeSnapshots(_ context.Context, in *ec2sdk.DescribeSnapshotsInput, _ ...func(*ec2sdk.Options)) (*ec2sdk.DescribeSnapshotsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("DescribeSnapshots"); err != nil {
		return nil, err
	}
	var matched []ec2types.Snapshot
	for _, s := range f.snapshots {
		ok, err := matchFilters(in.Filters, func(string) ([]string, bool) { return nil, false }, s.Tags)
		if err != nil {
			return nil, err
		}
		if ok {
			matched = append(matched, s)
		}
	}
	page, next, err := paginate(matched, in.NextToken, f.pageSize)
	return &ec2sdk.DescribeSnapshotsOutput{Snapshots: page, NextToken: next}, err
}

// ------------------------------------------------------------- fast launch

const fastLaunchSettle = 2 // describes before enabling/disabling settles

func (f *fakeEC2) EnableFastLaunch(_ context.Context, in *ec2sdk.EnableFastLaunchInput, _ ...func(*ec2sdk.Options)) (*ec2sdk.EnableFastLaunchOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("EnableFastLaunch"); err != nil {
		return nil, err
	}
	id := aws.ToString(in.ImageId)
	img, ok := f.images[id]
	switch {
	case !ok:
		return nil, apiError("InvalidAMIID.NotFound")
	case img.Platform != ec2types.PlatformValuesWindows:
		return nil, apiError("InvalidParameterValue")
	case aws.ToInt32(in.MaxParallelLaunches) < minParallelLaunches:
		return nil, apiError("InvalidParameterValue")
	case f.fastLaunch[id] != nil && f.fastLaunch[id].item.State == ec2types.FastLaunchStateCodeDisabling:
		return nil, apiError("IncorrectState")
	}
	item := ec2types.DescribeFastLaunchImagesSuccessItem{
		ImageId: in.ImageId, State: ec2types.FastLaunchStateCodeEnabling, MaxParallelLaunches: in.MaxParallelLaunches,
		ResourceType:          ec2types.FastLaunchResourceType(aws.ToString(in.ResourceType)),
		SnapshotConfiguration: &ec2types.FastLaunchSnapshotConfigurationResponse{TargetResourceCount: in.SnapshotConfiguration.TargetResourceCount},
	}
	if in.LaunchTemplate != nil {
		item.LaunchTemplate = &ec2types.FastLaunchLaunchTemplateSpecificationResponse{LaunchTemplateId: in.LaunchTemplate.LaunchTemplateId}
	}
	f.fastLaunch[id] = &fakeFastLaunch{item: item, ticks: fastLaunchSettle}
	return &ec2sdk.EnableFastLaunchOutput{ImageId: in.ImageId, State: item.State}, nil
}

func (f *fakeEC2) DisableFastLaunch(_ context.Context, in *ec2sdk.DisableFastLaunchInput, _ ...func(*ec2sdk.Options)) (*ec2sdk.DisableFastLaunchOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("DisableFastLaunch"); err != nil {
		return nil, err
	}
	fl := f.fastLaunch[aws.ToString(in.ImageId)]
	if fl == nil {
		return nil, apiError("InvalidParameterValue")
	}
	fl.item.State, fl.ticks = ec2types.FastLaunchStateCodeDisabling, fastLaunchSettle
	return &ec2sdk.DisableFastLaunchOutput{ImageId: in.ImageId, State: fl.item.State}, nil
}

func (f *fakeEC2) DescribeFastLaunchImages(_ context.Context, in *ec2sdk.DescribeFastLaunchImagesInput, _ ...func(*ec2sdk.Options)) (*ec2sdk.DescribeFastLaunchImagesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("DescribeFastLaunchImages"); err != nil {
		return nil, err
	}
	out := &ec2sdk.DescribeFastLaunchImagesOutput{}
	for _, id := range sortedKeys(f.fastLaunch) {
		fl := f.fastLaunch[id]
		if fl.ticks > 0 {
			fl.ticks--
		} else {
			switch fl.item.State {
			case ec2types.FastLaunchStateCodeEnabling:
				fl.item.State = ec2types.FastLaunchStateCodeEnabled
				for range aws.ToInt32(fl.item.SnapshotConfiguration.TargetResourceCount) {
					f.snapshots = append(f.snapshots, ec2types.Snapshot{SnapshotId: aws.String(f.id("snap")),
						Description: aws.String("Created by EC2 Fast Launch for " + id),
						Tags:        []ec2types.Tag{{Key: aws.String(FastLaunchCreatedByTag), Value: aws.String(FastLaunchCreatedByValue)}}})
				}
			case ec2types.FastLaunchStateCodeDisabling:
				delete(f.fastLaunch, id)
				f.snapshots = slices.DeleteFunc(f.snapshots, func(s ec2types.Snapshot) bool { return snapshotReferences(s, id) })
				continue
			}
		}
		if len(in.ImageIds) == 0 || slices.Contains(in.ImageIds, id) {
			out.FastLaunchImages = append(out.FastLaunchImages, fl.item)
		}
	}
	return out, nil
}

// ------------------------------------------------------------- pricing

// fakePricing serves Price List product documents keyed by "type|OS".
type fakePricing struct {
	mu    sync.Mutex
	docs  map[string][]string
	err   error
	calls int
}

func (p *fakePricing) GetProducts(_ context.Context, in *pricing.GetProductsInput, _ ...func(*pricing.Options)) (*pricing.GetProductsOutput, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	var typ, os string
	for _, f := range in.Filters {
		switch aws.ToString(f.Field) {
		case "instanceType":
			typ = aws.ToString(f.Value)
		case "operatingSystem":
			os = aws.ToString(f.Value)
		}
	}
	return &pricing.GetProductsOutput{PriceList: p.docs[typ+"|"+os]}, nil
}

// ------------------------------------------------------------- helpers

// matchFilters evaluates EC2 filters: tag:K, tag-key and resource attributes from
// attr. Values within a filter are OR-ed, filters are AND-ed; unknown names fail
// like EC2 does, so a typo in the provider shows up in tests.
func matchFilters(filters []ec2types.Filter, attr func(string) ([]string, bool), tags []ec2types.Tag) (bool, error) {
	tm := fromEC2Tags(tags)
	for _, flt := range filters {
		name := aws.ToString(flt.Name)
		var have []string
		switch {
		case strings.HasPrefix(name, "tag:"):
			if v, ok := tm[strings.TrimPrefix(name, "tag:")]; ok {
				have = []string{v}
			}
		case name == "tag-key":
			for k := range tm {
				have = append(have, k)
			}
		default:
			v, ok := attr(name)
			if !ok {
				return false, apiError("InvalidParameterValue")
			}
			have = v
		}
		if len(flt.Values) > 200 {
			return false, apiError("FilterLimitExceeded")
		}
		if !slices.ContainsFunc(have, func(h string) bool { return slices.Contains(flt.Values, h) }) {
			return false, nil
		}
	}
	return true, nil
}

func paginate[T any](items []T, token *string, size int) ([]T, *string, error) {
	start := 0
	if t := aws.ToString(token); t != "" {
		n, err := strconv.Atoi(t)
		if err != nil || n > len(items) {
			return nil, nil, apiError("InvalidPaginationToken")
		}
		start = n
	}
	end := min(start+size, len(items))
	var next *string
	if end < len(items) {
		next = aws.String(strconv.Itoa(end))
	}
	return items[start:end], next, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// stepClock is a deterministic clock: Sleep advances time instead of blocking,
// so token-bucket waits and polling loops run instantly yet measurably.
type stepClock struct {
	mu    sync.Mutex
	t     time.Time
	slept time.Duration
}

func newStepClock() *stepClock { return &stepClock{t: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)} }

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *stepClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (c *stepClock) After(d time.Duration) <-chan time.Time {
	c.Advance(d)
	ch := make(chan time.Time, 1)
	ch <- c.Now()
	return ch
}

func (c *stepClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.slept += d
	c.mu.Unlock()
	return nil
}

func (c *stepClock) Slept() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.slept
}
