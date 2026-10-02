// SPDX-License-Identifier: FSL-1.1-ALv2

package ec2

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2sdk "github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
)

// defaultStates are the instance states Describe returns unless asked otherwise.
var defaultStates = []ports.InstanceState{ports.InstancePending, ports.InstanceRunning, ports.InstanceShuttingDown}

// Describe lists the controller's instances. It is always filtered by the
// cluster and managed-by tags (R-SCALE-5/6); an empty cluster is rejected. ID
// lists are batched (at most 200 per call) and every page is read.
func (p *Provider) Describe(ctx context.Context, f ports.InstanceFilter) ([]ports.Instance, error) {
	if f.Cluster == "" {
		return nil, fmt.Errorf("%w: Describe needs the cluster tag", ports.ErrInvalid)
	}
	states := f.States
	if len(states) == 0 {
		states = defaultStates
	}
	base := ownerFilters(f.Cluster)
	if f.Pool != "" {
		base = append(base, filter("tag:"+domain.TagPool, string(f.Pool)))
	}
	sv := make([]string, len(states))
	for i, s := range states {
		sv[i] = string(s)
	}
	base = append(base, filter("instance-state-name", sv...))

	var batches [][]ec2types.Filter
	if len(f.IDs) == 0 {
		batches = [][]ec2types.Filter{base}
	} else {
		for _, ids := range chunks(uniq(f.IDs), idBatch) {
			batches = append(batches, append(slices.Clone(base), filter("instance-id", ids...)))
		}
	}
	var out []ports.Instance
	seen := map[string]bool{}
	for _, fs := range batches {
		insts, err := p.describeInstances(ctx, fs)
		if err != nil {
			return nil, err
		}
		for _, i := range insts {
			inst := toInstance(i)
			if !seen[inst.ID] {
				seen[inst.ID] = true
				out = append(out, inst)
			}
		}
	}
	return out, nil
}

// describeInstances reads every page of DescribeInstances for filters. The
// instance-id filter (not the InstanceIds parameter) is used for ID lists, so an
// unknown ID is simply absent instead of failing the whole call.
func (p *Provider) describeInstances(ctx context.Context, filters []ec2types.Filter) ([]ec2types.Instance, error) {
	var out []ec2types.Instance
	var next *string
	for {
		if err := p.describeBucket.Wait(ctx); err != nil {
			return nil, fmt.Errorf("ec2 DescribeInstances: %w", err)
		}
		resp, err := p.api.DescribeInstances(ctx, &ec2sdk.DescribeInstancesInput{
			Filters: filters, MaxResults: aws.Int32(1000), NextToken: next,
		})
		if err != nil {
			return nil, p.apiErr("DescribeInstances", p.describeBucket, err)
		}
		for _, r := range resp.Reservations {
			out = append(out, r.Instances...)
		}
		if aws.ToString(resp.NextToken) == "" {
			return out, nil
		}
		next = resp.NextToken
	}
}

// Terminate terminates the controller's instances in batches. Each ID is first
// described (by ID, any state) and refused with ErrNotOwned unless it carries this
// cluster's cluster and managed-by tags — the tag-conditioned IAM policy is only
// the second line of defence. Unknown IDs get ErrNotFound; already terminated
// instances succeed. The map holds the failures only.
func (p *Provider) Terminate(ctx context.Context, cluster string, ids []string) (map[string]error, error) {
	if cluster == "" {
		return nil, fmt.Errorf("%w: Terminate needs the cluster tag", ports.ErrInvalid)
	}
	failed := map[string]error{}
	for _, batch := range chunks(uniq(ids), idBatch) {
		insts, err := p.describeInstances(ctx, []ec2types.Filter{filter("instance-id", batch...)})
		if err != nil {
			return failed, err
		}
		byID := make(map[string]ec2types.Instance, len(insts))
		for _, i := range insts {
			byID[aws.ToString(i.InstanceId)] = i
		}
		var mine []string
		for _, id := range batch {
			i, ok := byID[id]
			switch {
			case !ok:
				failed[id] = fmt.Errorf("%w: instance %s", ports.ErrNotFound, id)
			case !owned(fromEC2Tags(i.Tags), cluster):
				failed[id] = fmt.Errorf("%w: instance %s lacks the %s=%s and %s tags", ports.ErrNotOwned, id, domain.TagCluster, cluster, domain.TagManagedBy)
			case i.State != nil && i.State.Name == ec2types.InstanceStateNameTerminated:
				// already gone: success
			default:
				mine = append(mine, id)
			}
		}
		if err := p.terminate(ctx, mine, failed); err != nil {
			return failed, err
		}
	}
	return failed, nil
}

// terminate calls TerminateInstances for ids (all verified as owned). If the batch
// call fails for a per-resource reason it retries one ID at a time so each failure
// is attributed to its instance.
func (p *Provider) terminate(ctx context.Context, ids []string, failed map[string]error) error {
	if len(ids) == 0 {
		return nil
	}
	if err := p.terminateBucket.Wait(ctx); err != nil {
		return fmt.Errorf("ec2 TerminateInstances: %w", err)
	}
	_, err := p.api.TerminateInstances(ctx, &ec2sdk.TerminateInstancesInput{InstanceIds: ids})
	if err == nil {
		return nil
	}
	if isContextErr(err) {
		return fmt.Errorf("ec2 TerminateInstances: %w", err)
	}
	kind := classify(err)
	if len(ids) > 1 && kind != kindThrottled && kind != kindUnknown {
		for _, id := range ids {
			if err := p.terminate(ctx, []string{id}, failed); err != nil {
				return err
			}
		}
		return nil
	}
	var werr error
	if errorCode(err) == "UnauthorizedOperation" {
		// The tag-conditioned IAM policy refused it: not ours as far as AWS is concerned.
		p.noteAPIError("TerminateInstances", err)
		werr = fmt.Errorf("ec2 TerminateInstances: %w: %w", ports.ErrNotOwned, err)
	} else {
		werr = p.apiErrKind("TerminateInstances", p.terminateBucket, kind, err)
	}
	for _, id := range ids {
		failed[id] = werr
	}
	return nil
}

// ----------------------------------------------------------------- orphans

// ListOrphans returns the cluster's volumes in state "available" and its
// unattached ENIs that have been seen unattached for at least OrphanGrace. Volumes
// must also be older than the grace period; ENIs carry no creation time, so their
// age is how long this provider has seen them unattached (a restarted controller
// waits one grace period before reporting them).
func (p *Provider) ListOrphans(ctx context.Context, cluster string) ([]ports.Orphan, error) {
	if cluster == "" {
		return nil, fmt.Errorf("%w: ListOrphans needs the cluster tag", ports.ErrInvalid)
	}
	vols, err := p.describeVolumes(ctx, append(ownerFilters(cluster), filter("status", string(ec2types.VolumeStateAvailable))))
	if err != nil {
		return nil, err
	}
	enis, err := p.describeENIs(ctx, append(ownerFilters(cluster), filter("status", string(ec2types.NetworkInterfaceStatusAvailable))))
	if err != nil {
		return nil, err
	}
	now := p.clock.Now()
	grace := p.opts.OrphanGrace
	p.mu.Lock()
	defer p.mu.Unlock()
	prev := p.firstSeen[cluster]
	seen := make(map[string]time.Time, len(vols)+len(enis)) // drops what is no longer unattached
	since := func(id string) time.Time {
		t, ok := prev[id]
		if !ok {
			t = now
		}
		seen[id] = t
		return t
	}
	var out []ports.Orphan
	for _, v := range vols {
		id := aws.ToString(v.VolumeId)
		first := since(id)
		age := now.Sub(aws.ToTime(v.CreateTime))
		if now.Sub(first) >= grace && age >= grace {
			out = append(out, ports.Orphan{Kind: ports.OrphanVolume, ID: id, Pool: domain.PoolName(fromEC2Tags(v.Tags)[domain.TagPool]), Age: age})
		}
	}
	for _, n := range enis {
		id := aws.ToString(n.NetworkInterfaceId)
		if age := now.Sub(since(id)); age >= grace {
			out = append(out, ports.Orphan{Kind: ports.OrphanENI, ID: id, Pool: domain.PoolName(fromEC2Tags(n.TagSet)[domain.TagPool]), Age: age})
		}
	}
	p.firstSeen[cluster] = seen
	return out, nil
}

// DeleteOrphans deletes orphans after re-checking each one: it must still exist
// unattached, carry this cluster's tags (else ErrNotOwned) and be past the grace
// period. Already deleted orphans succeed. The map holds the failures only.
func (p *Provider) DeleteOrphans(ctx context.Context, cluster string, orphans []ports.Orphan) (map[string]error, error) {
	if cluster == "" {
		return nil, fmt.Errorf("%w: DeleteOrphans needs the cluster tag", ports.ErrInvalid)
	}
	failed := map[string]error{}
	var volIDs, eniIDs []string
	for _, o := range orphans {
		switch o.Kind {
		case ports.OrphanVolume:
			volIDs = append(volIDs, o.ID)
		case ports.OrphanENI:
			eniIDs = append(eniIDs, o.ID)
		default:
			failed[o.ID] = fmt.Errorf("%w: unknown orphan kind %q", ports.ErrInvalid, o.Kind)
		}
	}
	now := p.clock.Now()
	for _, batch := range chunks(uniq(volIDs), idBatch) {
		vols, err := p.describeVolumes(ctx, []ec2types.Filter{filter("volume-id", batch...)})
		if err != nil {
			return failed, err
		}
		byID := map[string]ec2types.Volume{}
		for _, v := range vols {
			byID[aws.ToString(v.VolumeId)] = v
		}
		for _, id := range batch {
			v, ok := byID[id]
			if !ok {
				continue // already deleted
			}
			switch {
			case !owned(fromEC2Tags(v.Tags), cluster):
				failed[id] = fmt.Errorf("%w: volume %s", ports.ErrNotOwned, id)
			case v.State != ec2types.VolumeStateAvailable:
				failed[id] = fmt.Errorf("%w: volume %s is %s, not an orphan", ports.ErrInvalid, id, v.State)
			case now.Sub(aws.ToTime(v.CreateTime)) < p.opts.OrphanGrace:
				failed[id] = fmt.Errorf("%w: volume %s is younger than the orphan grace period", ports.ErrInvalid, id)
			default:
				if err := p.deleteVolume(ctx, id); err != nil {
					if isContextErr(err) {
						return failed, err
					}
					failed[id] = err
				}
			}
		}
	}
	for _, batch := range chunks(uniq(eniIDs), idBatch) {
		enis, err := p.describeENIs(ctx, []ec2types.Filter{filter("network-interface-id", batch...)})
		if err != nil {
			return failed, err
		}
		byID := map[string]ec2types.NetworkInterface{}
		for _, n := range enis {
			byID[aws.ToString(n.NetworkInterfaceId)] = n
		}
		for _, id := range batch {
			n, ok := byID[id]
			if !ok {
				continue
			}
			p.mu.Lock()
			first, seen := p.firstSeen[cluster][id]
			p.mu.Unlock()
			switch {
			case !owned(fromEC2Tags(n.TagSet), cluster):
				failed[id] = fmt.Errorf("%w: network interface %s", ports.ErrNotOwned, id)
			case n.Status != ec2types.NetworkInterfaceStatusAvailable || n.Attachment != nil && n.Attachment.Status != ec2types.AttachmentStatusDetached:
				failed[id] = fmt.Errorf("%w: network interface %s is %s, not an orphan", ports.ErrInvalid, id, n.Status)
			case !seen || now.Sub(first) < p.opts.OrphanGrace:
				failed[id] = fmt.Errorf("%w: network interface %s has not been unattached for the orphan grace period", ports.ErrInvalid, id)
			default:
				if err := p.deleteENI(ctx, id); err != nil {
					if isContextErr(err) {
						return failed, err
					}
					failed[id] = err
				}
			}
		}
	}
	return failed, nil
}

func (p *Provider) deleteVolume(ctx context.Context, id string) error {
	if err := p.mutateBucket.Wait(ctx); err != nil {
		return fmt.Errorf("ec2 DeleteVolume: %w", err)
	}
	_, err := p.api.DeleteVolume(ctx, &ec2sdk.DeleteVolumeInput{VolumeId: aws.String(id)})
	if err == nil || classify(err) == kindNotFound {
		return nil
	}
	return p.apiErr("DeleteVolume", p.mutateBucket, err)
}

func (p *Provider) deleteENI(ctx context.Context, id string) error {
	if err := p.mutateBucket.Wait(ctx); err != nil {
		return fmt.Errorf("ec2 DeleteNetworkInterface: %w", err)
	}
	_, err := p.api.DeleteNetworkInterface(ctx, &ec2sdk.DeleteNetworkInterfaceInput{NetworkInterfaceId: aws.String(id)})
	if err == nil || classify(err) == kindNotFound {
		return nil
	}
	return p.apiErr("DeleteNetworkInterface", p.mutateBucket, err)
}

func (p *Provider) describeVolumes(ctx context.Context, filters []ec2types.Filter) ([]ec2types.Volume, error) {
	var out []ec2types.Volume
	var next *string
	for {
		if err := p.describeBucket.Wait(ctx); err != nil {
			return nil, fmt.Errorf("ec2 DescribeVolumes: %w", err)
		}
		resp, err := p.api.DescribeVolumes(ctx, &ec2sdk.DescribeVolumesInput{Filters: filters, MaxResults: aws.Int32(500), NextToken: next})
		if err != nil {
			return nil, p.apiErr("DescribeVolumes", p.describeBucket, err)
		}
		out = append(out, resp.Volumes...)
		if aws.ToString(resp.NextToken) == "" {
			return out, nil
		}
		next = resp.NextToken
	}
}

func (p *Provider) describeENIs(ctx context.Context, filters []ec2types.Filter) ([]ec2types.NetworkInterface, error) {
	var out []ec2types.NetworkInterface
	var next *string
	for {
		if err := p.describeBucket.Wait(ctx); err != nil {
			return nil, fmt.Errorf("ec2 DescribeNetworkInterfaces: %w", err)
		}
		resp, err := p.api.DescribeNetworkInterfaces(ctx, &ec2sdk.DescribeNetworkInterfacesInput{Filters: filters, MaxResults: aws.Int32(1000), NextToken: next})
		if err != nil {
			return nil, p.apiErr("DescribeNetworkInterfaces", p.describeBucket, err)
		}
		out = append(out, resp.NetworkInterfaces...)
		if aws.ToString(resp.NextToken) == "" {
			return out, nil
		}
		next = resp.NextToken
	}
}
