// SPDX-License-Identifier: FSL-1.1-ALv2

// Package awsinv is the AWS side of the e2e collectors: tag-filtered
// describes of the campaign's worker resources (NFR-C1 "zero instances,
// volumes, ENIs, EIPs or public IPs at zero scale"), a sampler that turns
// periodic describes into instance-seconds by type, EBS GB-hours and
// instance lifecycle timestamps (§10.4), and the two EC2 fault injections of
// T9 (terminate a busy worker; cut a worker's network by swapping its
// security group), both of which refuse resources that do not carry the
// campaign's tags (§12: filter every destructive call by tag).
package awsinv

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// EC2API is the narrow EC2 surface used here.
type EC2API interface {
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	DescribeVolumes(ctx context.Context, in *ec2.DescribeVolumesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error)
	DescribeNetworkInterfaces(ctx context.Context, in *ec2.DescribeNetworkInterfacesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeNetworkInterfacesOutput, error)
	DescribeAddresses(ctx context.Context, in *ec2.DescribeAddressesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error)
	DescribeNatGateways(ctx context.Context, in *ec2.DescribeNatGatewaysInput, optFns ...func(*ec2.Options)) (*ec2.DescribeNatGatewaysOutput, error)
	TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	ModifyInstanceAttribute(ctx context.Context, in *ec2.ModifyInstanceAttributeInput, optFns ...func(*ec2.Options)) (*ec2.ModifyInstanceAttributeOutput, error)
}

// Instance is one worker instance as described.
type Instance struct {
	ID             string    `json:"id"`
	Type           string    `json:"type"`
	State          string    `json:"state"`
	Pool           string    `json:"pool"`
	Generation     string    `json:"generation"`
	Platform       string    `json:"platform"` // linux | windows
	AZ             string    `json:"az"`
	PublicIP       bool      `json:"publicIp"`
	LaunchTime     time.Time `json:"launchTime"`
	ImageID        string    `json:"imageId"`
	LaunchToken    string    `json:"launchToken,omitempty"`
	SecurityGroups []string  `json:"securityGroups,omitempty"`
}

// Volume is one EBS volume.
type Volume struct {
	ID       string    `json:"id"`
	State    string    `json:"state"`
	GiB      int32     `json:"gib"`
	IOPS     int32     `json:"iops"`
	MiBps    int32     `json:"mibps"`
	Created  time.Time `json:"created"`
	Attached []string  `json:"attached,omitempty"`
}

// Snapshot is one tag-filtered inventory.
type Snapshot struct {
	At        time.Time  `json:"at"`
	Instances []Instance `json:"instances"`
	Volumes   []Volume   `json:"volumes"`
	ENIs      []string   `json:"enis"`
	EIPs      []string   `json:"eips"`
}

// Residue counts what must not exist at zero scale (NFR-C1, T0, T8, T15).
type Residue struct {
	Instances int `json:"instances"`
	Volumes   int `json:"volumes"`
	ENIs      int `json:"enis"`
	EIPs      int `json:"eips"`
	PublicIPs int `json:"publicIps"`
}

// Zero reports whether nothing is left.
func (r Residue) Zero() bool { return r == Residue{} }

func (r Residue) String() string {
	return fmt.Sprintf("%d instances, %d volumes, %d ENIs, %d EIPs, %d public IPs", r.Instances, r.Volumes, r.ENIs, r.EIPs, r.PublicIPs)
}

// Inventory describes the campaign's worker resources.
type Inventory struct {
	API EC2API
	// Tags every worker resource carries: the campaign tags (cucina:env,
	// cucina:run) plus cucina:role=worker, set by the controller (extraTags).
	Tags map[string]string
	Now  func() time.Time
}

func (inv *Inventory) filters(extra ...ec2types.Filter) []ec2types.Filter {
	keys := make([]string, 0, len(inv.Tags))
	for k := range inv.Tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var fs []ec2types.Filter
	for _, k := range keys {
		fs = append(fs, ec2types.Filter{Name: aws.String("tag:" + k), Values: []string{inv.Tags[k]}})
	}
	return append(fs, extra...)
}

func (inv *Inventory) now() time.Time {
	if inv.Now != nil {
		return inv.Now()
	}
	return time.Now()
}

func tag(tags []ec2types.Tag, k string) string {
	for _, t := range tags {
		if aws.ToString(t.Key) == k {
			return aws.ToString(t.Value)
		}
	}
	return ""
}

// Describe takes one snapshot. Terminated instances are excluded; "deleted"
// volumes are excluded by the API itself.
func (inv *Inventory) Describe(ctx context.Context) (Snapshot, error) {
	if len(inv.Tags) == 0 || inv.Tags["cucina:run"] == "" {
		return Snapshot{}, fmt.Errorf("awsinv: refusing an untagged describe (§12)")
	}
	s := Snapshot{At: inv.now()}
	states := ec2types.Filter{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "shutting-down", "stopping", "stopped"}}
	p := ec2.NewDescribeInstancesPaginator(inv.API, &ec2.DescribeInstancesInput{Filters: inv.filters(states)})
	for p.HasMorePages() {
		out, err := p.NextPage(ctx)
		if err != nil {
			return s, fmt.Errorf("describe-instances: %w", err)
		}
		for _, r := range out.Reservations {
			for _, i := range r.Instances {
				in := Instance{
					ID: aws.ToString(i.InstanceId), Type: string(i.InstanceType), State: string(i.State.Name),
					Pool: tag(i.Tags, "cucina:pool"), Generation: tag(i.Tags, "cucina:generation"),
					Platform: "linux", PublicIP: aws.ToString(i.PublicIpAddress) != "", LaunchTime: aws.ToTime(i.LaunchTime),
					ImageID: aws.ToString(i.ImageId), LaunchToken: tag(i.Tags, "cucina:launch-token"),
				}
				if i.Placement != nil {
					in.AZ = aws.ToString(i.Placement.AvailabilityZone)
				}
				if i.Platform == ec2types.PlatformValuesWindows {
					in.Platform = "windows"
				}
				for _, g := range i.SecurityGroups {
					in.SecurityGroups = append(in.SecurityGroups, aws.ToString(g.GroupId))
				}
				s.Instances = append(s.Instances, in)
			}
		}
	}
	vp := ec2.NewDescribeVolumesPaginator(inv.API, &ec2.DescribeVolumesInput{Filters: inv.filters()})
	for vp.HasMorePages() {
		out, err := vp.NextPage(ctx)
		if err != nil {
			return s, fmt.Errorf("describe-volumes: %w", err)
		}
		for _, v := range out.Volumes {
			vol := Volume{ID: aws.ToString(v.VolumeId), State: string(v.State), GiB: aws.ToInt32(v.Size), IOPS: aws.ToInt32(v.Iops),
				MiBps: aws.ToInt32(v.Throughput), Created: aws.ToTime(v.CreateTime)}
			for _, a := range v.Attachments {
				vol.Attached = append(vol.Attached, aws.ToString(a.InstanceId))
			}
			s.Volumes = append(s.Volumes, vol)
		}
	}
	np := ec2.NewDescribeNetworkInterfacesPaginator(inv.API, &ec2.DescribeNetworkInterfacesInput{Filters: inv.filters()})
	for np.HasMorePages() {
		out, err := np.NextPage(ctx)
		if err != nil {
			return s, fmt.Errorf("describe-network-interfaces: %w", err)
		}
		for _, n := range out.NetworkInterfaces {
			s.ENIs = append(s.ENIs, aws.ToString(n.NetworkInterfaceId))
		}
	}
	addrs, err := inv.API.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{Filters: inv.filters()})
	if err != nil {
		return s, fmt.Errorf("describe-addresses: %w", err)
	}
	for _, a := range addrs.Addresses {
		s.EIPs = append(s.EIPs, aws.ToString(a.AllocationId))
	}
	return s, nil
}

// Residue summarises a snapshot for the zero-scale checks.
func (s Snapshot) Residue() Residue {
	r := Residue{Instances: len(s.Instances), Volumes: len(s.Volumes), ENIs: len(s.ENIs), EIPs: len(s.EIPs)}
	for _, i := range s.Instances {
		if i.PublicIP {
			r.PublicIPs++
		}
	}
	return r
}

// NATGateways counts NAT gateways carrying the campaign's env/run tags
// (NFR-T9: the topology has none, so NAT bytes are zero by construction).
func (inv *Inventory) NATGateways(ctx context.Context) (int, error) {
	run := map[string]string{"cucina:env": inv.Tags["cucina:env"], "cucina:run": inv.Tags["cucina:run"]}
	scoped := &Inventory{API: inv.API, Tags: run}
	out, err := inv.API.DescribeNatGateways(ctx, &ec2.DescribeNatGatewaysInput{Filter: scoped.filters(ec2types.Filter{Name: aws.String("state"), Values: []string{"pending", "available"}})})
	if err != nil {
		return 0, err
	}
	return len(out.NatGateways), nil
}

// Duplicates returns launch tokens carried by more than one instance (T9d:
// a controller restart must not launch twice for one idempotency token).
func Duplicates(snaps []Snapshot) map[string][]string {
	byToken := map[string]map[string]bool{}
	for _, s := range snaps {
		for _, i := range s.Instances {
			if i.LaunchToken == "" {
				continue
			}
			if byToken[i.LaunchToken] == nil {
				byToken[i.LaunchToken] = map[string]bool{}
			}
			byToken[i.LaunchToken][i.ID] = true
		}
	}
	out := map[string][]string{}
	for tok, ids := range byToken {
		if len(ids) > 1 {
			for id := range ids {
				out[tok] = append(out[tok], id)
			}
			sort.Strings(out[tok])
		}
	}
	return out
}

// ------------------------------------------------------------------ sampler

// Sampler describes periodically during a scenario.
type Sampler struct {
	inv   *Inventory
	mu    sync.Mutex
	snaps []Snapshot
	errs  int
	stop  chan struct{}
	done  chan struct{}
}

// StartSampler samples every interval until Stop.
func (inv *Inventory) StartSampler(ctx context.Context, every time.Duration) *Sampler {
	s := &Sampler{inv: inv, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			snap, err := inv.Describe(ctx)
			s.mu.Lock()
			if err != nil {
				s.errs++
			} else {
				s.snaps = append(s.snaps, snap)
			}
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-s.stop:
				return
			case <-t.C:
			}
		}
	}()
	return s
}

// Stop ends sampling and returns the snapshots taken.
func (s *Sampler) Stop() ([]Snapshot, int) {
	close(s.stop)
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Snapshot(nil), s.snaps...), s.errs
}

// Lifecycle is one instance's observed timeline.
type Lifecycle struct {
	ID         string    `json:"id"`
	Pool       string    `json:"pool"`
	Type       string    `json:"type"`
	Platform   string    `json:"platform"`
	Launched   time.Time `json:"launched"`          // EC2 LaunchTime (≈ RunInstances accepted)
	Running    time.Time `json:"running,omitempty"` // first sample in "running"
	LastSeen   time.Time `json:"lastSeen"`          // last sample not terminated
	Gone       time.Time `json:"gone,omitempty"`    // first sample without it
	RunSeconds float64   `json:"runSeconds"`
	PublicIP   bool      `json:"publicIp,omitempty"`
	// Volumes attached to the instance (root + extra; DeleteOnTermination).
	Volumes []Volume `json:"volumes,omitempty"`
}

// UsageSummary is what the sampler integrates.
type UsageSummary struct {
	Lifecycles      []Lifecycle        `json:"lifecycles"`
	InstanceSeconds map[string]float64 `json:"instanceSeconds"` // "<os>/<type>"
	VolumeGiBHours  float64            `json:"volumeGibHours"`
	IOPSHours       float64            `json:"iopsHoursAbove3000"`
	MiBpsHours      float64            `json:"mibpsHoursAbove125"`
	PublicIPv4Hours float64            `json:"publicIpv4Hours"`
	MaxInstances    int                `json:"maxInstances"`
	MaxByPool       map[string]int     `json:"maxByPool"`
	Samples         int                `json:"samples"`
	// DuplicateTokens lists launch tokens seen on more than one instance.
	DuplicateTokens map[string][]string `json:"duplicateTokens,omitempty"`
}

// Integrate turns ordered snapshots into usage. Billing of an instance runs
// from LaunchTime while it is seen pending/running until it disappears;
// each sample interval is attributed to the state at its start.
func Integrate(snaps []Snapshot) UsageSummary {
	u := UsageSummary{InstanceSeconds: map[string]float64{}, MaxByPool: map[string]int{}, Samples: len(snaps), DuplicateTokens: Duplicates(snaps)}
	if len(u.DuplicateTokens) == 0 {
		u.DuplicateTokens = nil
	}
	life := map[string]*Lifecycle{}
	var order []string
	for i, s := range snaps {
		if len(s.Instances) > u.MaxInstances {
			u.MaxInstances = len(s.Instances)
		}
		byPool := map[string]int{}
		seen := map[string]bool{}
		for _, in := range s.Instances {
			byPool[in.Pool]++
			seen[in.ID] = true
			l, ok := life[in.ID]
			if !ok {
				l = &Lifecycle{ID: in.ID, Pool: in.Pool, Type: in.Type, Platform: in.Platform, Launched: in.LaunchTime}
				life[in.ID] = l
				order = append(order, in.ID)
			}
			if in.State == "running" && l.Running.IsZero() {
				l.Running = s.At
			}
			l.PublicIP = l.PublicIP || in.PublicIP
			l.LastSeen = s.At
		}
		for _, v := range s.Volumes {
			for _, id := range v.Attached {
				if l, ok := life[id]; ok && !hasVolume(l.Volumes, v.ID) {
					l.Volumes = append(l.Volumes, v)
				}
			}
		}
		for p, n := range byPool {
			if n > u.MaxByPool[p] {
				u.MaxByPool[p] = n
			}
		}
		for id, l := range life {
			if !seen[id] && l.Gone.IsZero() && s.At.After(l.LastSeen) {
				l.Gone = s.At
			}
		}
		if i+1 < len(snaps) {
			dt := snaps[i+1].At.Sub(s.At).Hours()
			for _, v := range s.Volumes {
				u.VolumeGiBHours += float64(v.GiB) * dt
				if v.IOPS > 3000 {
					u.IOPSHours += float64(v.IOPS-3000) * dt
				}
				if v.MiBps > 125 {
					u.MiBpsHours += float64(v.MiBps-125) * dt
				}
			}
			for _, in := range s.Instances {
				if in.PublicIP && (in.State == "pending" || in.State == "running") {
					u.PublicIPv4Hours += dt
				}
			}
		}
	}
	for _, id := range order {
		l := life[id]
		end := l.Gone
		if end.IsZero() {
			end = l.LastSeen
		}
		start := l.Launched
		if start.IsZero() {
			start = l.Running
		}
		if !start.IsZero() && end.After(start) {
			l.RunSeconds = end.Sub(start).Seconds()
		}
		u.InstanceSeconds[l.Platform+"/"+l.Type] += l.RunSeconds
		u.Lifecycles = append(u.Lifecycles, *l)
	}
	return u
}

func hasVolume(vs []Volume, id string) bool {
	for _, v := range vs {
		if v.ID == id {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------------ faults

// ownedWorker checks that an instance carries every campaign tag and the
// controller's managed-by tag before a destructive call.
func (inv *Inventory) ownedWorker(ctx context.Context, id string) (ec2types.Instance, error) {
	out, err := inv.API.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if err != nil {
		return ec2types.Instance{}, err
	}
	var found []ec2types.Instance
	for _, r := range out.Reservations {
		found = append(found, r.Instances...)
	}
	if len(found) != 1 {
		return ec2types.Instance{}, fmt.Errorf("awsinv: %s not found", id)
	}
	i := found[0]
	for k, v := range inv.Tags {
		if tag(i.Tags, k) != v {
			return i, fmt.Errorf("awsinv: %s lacks tag %s=%s; refusing (§12)", id, k, v)
		}
	}
	if tag(i.Tags, "cucina:managed-by") != "cucina-controller" {
		return i, fmt.Errorf("awsinv: %s is not a controller-managed worker; refusing", id)
	}
	return i, nil
}

// TerminateWorker terminates one tagged worker (T9a).
func (inv *Inventory) TerminateWorker(ctx context.Context, id string) error {
	if _, err := inv.ownedWorker(ctx, id); err != nil {
		return err
	}
	_, err := inv.API.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{id}})
	return err
}

// SwapSecurityGroups replaces a tagged worker's security groups (T9e: cut the
// worker off the control plane, then restore) and returns the previous ones.
func (inv *Inventory) SwapSecurityGroups(ctx context.Context, id string, groups []string) ([]string, error) {
	i, err := inv.ownedWorker(ctx, id)
	if err != nil {
		return nil, err
	}
	var prev []string
	for _, g := range i.SecurityGroups {
		prev = append(prev, aws.ToString(g.GroupId))
	}
	_, err = inv.API.ModifyInstanceAttribute(ctx, &ec2.ModifyInstanceAttributeInput{InstanceId: aws.String(id), Groups: groups})
	return prev, err
}
