// SPDX-License-Identifier: FSL-1.1-ALv2

package ec2

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2sdk "github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/sloper-ai/cucina/internal/ports"
)

// Fast Launch actions and states (ports.FastLaunchOp / ports.FastLaunchStatus).
const (
	FastLaunchEnable   = "enable"
	FastLaunchDisable  = "disable"
	FastLaunchDescribe = "describe"

	flEnabling  = "enabling"
	flEnabled   = "enabled"
	flDisabling = "disabling"
	flDisabled  = "disabled"
	flFailed    = "failed"

	// FastLaunchCreatedByTag is the tag EC2 puts on every resource Fast Launch creates
	// (snapshots, prep instances, volumes, launch templates): CreatedBy=EC2 Fast Launch.
	FastLaunchCreatedByTag   = "CreatedBy"
	FastLaunchCreatedByValue = "EC2 Fast Launch"

	// minParallelLaunches is EC2's lower bound for MaxParallelLaunches.
	minParallelLaunches = 6
)

// FastLaunch drives Windows EC2 Fast Launch (R-POOL-2):
//
//   - "enable" enables pre-provisioned snapshots (ResourceType=snapshot,
//     TargetResourceCount, MaxParallelLaunches >= 6, optional launch template) and
//     waits until EC2 reports "enabled" (the first snapshot exists) or a failure.
//     Enabling an image that is already enabled/enabling with the same settings
//     makes no API call.
//   - "disable" disables it and waits until the image has left Fast Launch
//     entirely (its snapshots are gone) — callers rely on that before
//     deregistering an AMI. Disabling a disabled image is a no-op.
//   - "describe" reports the current state ("disabled" when not enabled).
//
// The waits poll every FastLaunchPollInterval and are bounded only by ctx: on
// expiry the last observed status is returned with the context error, and calling
// again simply continues waiting.
func (p *Provider) FastLaunch(ctx context.Context, op ports.FastLaunchOp) (ports.FastLaunchStatus, error) {
	if op.ImageID == "" {
		return ports.FastLaunchStatus{}, fmt.Errorf("%w: fast launch needs an image ID", ports.ErrInvalid)
	}
	switch op.Action {
	case FastLaunchDescribe:
		st, _, err := p.fastLaunchState(ctx, op.ImageID)
		return st, err
	case FastLaunchEnable:
		return p.enableFastLaunch(ctx, op)
	case FastLaunchDisable:
		return p.disableFastLaunch(ctx, op.ImageID)
	}
	return ports.FastLaunchStatus{}, fmt.Errorf("%w: unknown fast launch action %q", ports.ErrInvalid, op.Action)
}

func (p *Provider) enableFastLaunch(ctx context.Context, op ports.FastLaunchOp) (ports.FastLaunchStatus, error) {
	if op.TargetCount < 1 {
		return ports.FastLaunchStatus{}, fmt.Errorf("%w: fast launch target count must be >= 1", ports.ErrInvalid)
	}
	parallel := max(op.MaxParallel, minParallelLaunches)
	st, cur, err := p.fastLaunchState(ctx, op.ImageID)
	if err != nil {
		return st, err
	}
	same := cur != nil && fastLaunchMatches(*cur, op.TargetCount, parallel, op.LaunchTemplateID)
	switch {
	case st.State == flDisabling:
		// Re-enabling while EC2 cleans up is refused; wait for the clean-up first.
		if st, err = p.waitFastLaunch(ctx, st, flDisabled, false); err != nil {
			return st, err
		}
		fallthrough
	case !same || st.State == flFailed || st.State == flDisabled:
		in := &ec2sdk.EnableFastLaunchInput{
			ImageId:               aws.String(op.ImageID),
			ResourceType:          aws.String("snapshot"),
			SnapshotConfiguration: &ec2types.FastLaunchSnapshotConfigurationRequest{TargetResourceCount: aws.Int32(int32(op.TargetCount))},
			MaxParallelLaunches:   aws.Int32(int32(parallel)),
		}
		if op.LaunchTemplateID != "" {
			in.LaunchTemplate = &ec2types.FastLaunchLaunchTemplateSpecificationRequest{
				LaunchTemplateId: aws.String(op.LaunchTemplateID),
				Version:          aws.String("$Default"),
			}
		}
		if err := p.mutateBucket.Wait(ctx); err != nil {
			return st, fmt.Errorf("ec2 EnableFastLaunch: %w", err)
		}
		if _, err := p.api.EnableFastLaunch(ctx, in); err != nil {
			return st, p.apiErr("EnableFastLaunch", p.mutateBucket, err)
		}
		p.log.Info("fast launch enabling", "image", op.ImageID, "target", op.TargetCount, "parallel", parallel)
		st.State = flEnabling
		return p.waitFastLaunch(ctx, st, flEnabled, true)
	}
	return p.waitFastLaunch(ctx, st, flEnabled, false)
}

func (p *Provider) disableFastLaunch(ctx context.Context, imageID string) (ports.FastLaunchStatus, error) {
	st, _, err := p.fastLaunchState(ctx, imageID)
	if err != nil || st.State == flDisabled {
		return st, err
	}
	if st.State != flDisabling {
		if err := p.mutateBucket.Wait(ctx); err != nil {
			return st, fmt.Errorf("ec2 DisableFastLaunch: %w", err)
		}
		if _, err := p.api.DisableFastLaunch(ctx, &ec2sdk.DisableFastLaunchInput{ImageId: aws.String(imageID)}); err != nil {
			return st, p.apiErr("DisableFastLaunch", p.mutateBucket, err)
		}
		p.log.Info("fast launch disabling", "image", imageID)
		st.State = flDisabling
		return p.waitFastLaunch(ctx, st, flDisabled, true)
	}
	return p.waitFastLaunch(ctx, st, flDisabled, false)
}

// waitFastLaunch polls until the image reaches want (or "failed"). On error it
// returns the last observed status (starting with last). After a mutating call
// (settle) it waits one interval first, so an eventually consistent describe
// cannot report the state from before the call.
func (p *Provider) waitFastLaunch(ctx context.Context, last ports.FastLaunchStatus, want string, settle bool) (ports.FastLaunchStatus, error) {
	if settle {
		if err := p.clock.Sleep(ctx, p.opts.FastLaunchPollInterval); err != nil {
			return last, fmt.Errorf("ec2 fast launch: waiting for %q on %s: %w", want, last.ImageID, err)
		}
	}
	for {
		st, _, err := p.fastLaunchState(ctx, last.ImageID)
		if err != nil {
			return last, err
		}
		last = st
		switch st.State {
		case want:
			return st, nil
		case flFailed:
			return st, fmt.Errorf("ec2 fast launch: image %s failed while waiting for %q", st.ImageID, want)
		}
		if err := p.clock.Sleep(ctx, p.opts.FastLaunchPollInterval); err != nil {
			return st, fmt.Errorf("ec2 fast launch: waiting for %q on %s (now %q): %w", want, st.ImageID, st.State, err)
		}
	}
}

// fastLaunchState describes the Fast Launch configuration of one image. An image
// absent from DescribeFastLaunchImages is "disabled".
func (p *Provider) fastLaunchState(ctx context.Context, imageID string) (ports.FastLaunchStatus, *ec2types.DescribeFastLaunchImagesSuccessItem, error) {
	st := ports.FastLaunchStatus{ImageID: imageID} // State "" = unknown (on error)
	if err := p.describeBucket.Wait(ctx); err != nil {
		return st, nil, fmt.Errorf("ec2 DescribeFastLaunchImages: %w", err)
	}
	out, err := p.api.DescribeFastLaunchImages(ctx, &ec2sdk.DescribeFastLaunchImagesInput{ImageIds: []string{imageID}})
	if err != nil {
		return st, nil, p.apiErr("DescribeFastLaunchImages", p.describeBucket, err)
	}
	st.State = flDisabled
	var item *ec2types.DescribeFastLaunchImagesSuccessItem
	for i := range out.FastLaunchImages {
		if aws.ToString(out.FastLaunchImages[i].ImageId) == imageID {
			item = &out.FastLaunchImages[i]
		}
	}
	if item == nil {
		return st, nil, nil
	}
	switch item.State {
	case ec2types.FastLaunchStateCodeEnabling:
		st.State = flEnabling
	case ec2types.FastLaunchStateCodeEnabled:
		st.State = flEnabled
	case ec2types.FastLaunchStateCodeDisabling:
		st.State = flDisabling
	default: // enabling-failed, enabled-failed, disabling-failed
		st.State = flFailed
	}
	if st.State == flEnabled || st.State == flEnabling {
		n, err := p.fastLaunchSnapshots(ctx, imageID)
		if err != nil {
			return st, item, err
		}
		st.Snapshots = n
	}
	return st, item, nil
}

// fastLaunchSnapshots counts the pre-provisioned snapshots of an image: snapshots
// owned by the account, tagged CreatedBy=EC2 Fast Launch, that reference the image
// in a tag value or their description (best effort; EC2 publishes the exact count
// only as the CloudWatch metric NumberOfAvailableFastLaunchSnapshots).
func (p *Provider) fastLaunchSnapshots(ctx context.Context, imageID string) (int, error) {
	n := 0
	var next *string
	for {
		if err := p.describeBucket.Wait(ctx); err != nil {
			return n, fmt.Errorf("ec2 DescribeSnapshots: %w", err)
		}
		out, err := p.api.DescribeSnapshots(ctx, &ec2sdk.DescribeSnapshotsInput{
			OwnerIds:   []string{"self"},
			Filters:    []ec2types.Filter{filter("tag:"+FastLaunchCreatedByTag, FastLaunchCreatedByValue)},
			MaxResults: aws.Int32(1000),
			NextToken:  next,
		})
		if err != nil {
			return n, p.apiErr("DescribeSnapshots", p.describeBucket, err)
		}
		for _, s := range out.Snapshots {
			if snapshotReferences(s, imageID) {
				n++
			}
		}
		if aws.ToString(out.NextToken) == "" {
			return n, nil
		}
		next = out.NextToken
	}
}

func snapshotReferences(s ec2types.Snapshot, imageID string) bool {
	if strings.Contains(aws.ToString(s.Description), imageID) {
		return true
	}
	for _, t := range s.Tags {
		if aws.ToString(t.Value) == imageID {
			return true
		}
	}
	return false
}

func fastLaunchMatches(cur ec2types.DescribeFastLaunchImagesSuccessItem, target, parallel int, lt string) bool {
	if cur.SnapshotConfiguration == nil || int(aws.ToInt32(cur.SnapshotConfiguration.TargetResourceCount)) != target {
		return false
	}
	if int(aws.ToInt32(cur.MaxParallelLaunches)) != parallel {
		return false
	}
	curLT := ""
	if cur.LaunchTemplate != nil {
		curLT = aws.ToString(cur.LaunchTemplate.LaunchTemplateId)
	}
	return lt == "" || curLT == lt
}
