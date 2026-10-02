// SPDX-License-Identifier: FSL-1.1-ALv2

package ec2

import (
	"context"
	"fmt"
	"maps"
	"slices"

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
	fastLaunchTemplateTag    = "CreatedByLaunchTemplateId"
	fastLaunchDescription    = "This is Fast Launch snapshot for image "

	// minParallelLaunches is EC2's lower bound for MaxParallelLaunches.
	minParallelLaunches = 6
)

// FastLaunch drives Windows EC2 Fast Launch (R-POOL-2):
//
//   - "enable" enables pre-provisioned snapshots (ResourceType=snapshot,
//     TargetResourceCount, MaxParallelLaunches >= 6, verified launch template) and
//     waits until EC2 reports "enabled" (the first snapshot exists) or a failure.
//     Every explicit enable reconciles ExtraTags onto replacement snapshots, even
//     when configuration is unchanged (no additional EnableFastLaunch call). This
//     requires an owned AMI and launch template matching nonempty ExtraTags; AWS
//     does not inherit the template's instance/volume tags onto its snapshots.
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
	if op.LaunchTemplateID == "" && cur != nil && cur.LaunchTemplate != nil {
		op.LaunchTemplateID = aws.ToString(cur.LaunchTemplate.LaunchTemplateId)
	}
	if err := p.fastLaunchOwnedImage(ctx, op.ImageID); err != nil {
		return st, err
	}
	if err := p.fastLaunchOwnedTemplate(ctx, op.LaunchTemplateID); err != nil {
		return st, err
	}
	same := cur != nil && fastLaunchMatches(*cur, op.TargetCount, parallel, op.LaunchTemplateID)
	settle := false
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
		settle = true
	}
	st, err = p.waitFastLaunch(ctx, st, flEnabled, settle)
	if err != nil {
		return st, err
	}
	return st, p.tagFastLaunchSnapshots(ctx, op.ImageID, op.LaunchTemplateID)
}

func (p *Provider) disableFastLaunch(ctx context.Context, imageID string) (ports.FastLaunchStatus, error) {
	st, _, err := p.fastLaunchState(ctx, imageID)
	if err != nil {
		return st, err
	}
	if err := p.fastLaunchOwnedImage(ctx, imageID); err != nil {
		return st, err
	}
	if st.State == flDisabled {
		return p.waitFastLaunch(ctx, st, flDisabled, false)
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
			// A disabled configuration may still have children awaiting deletion.
			if want != flDisabled || st.Snapshots == 0 {
				return st, nil
			}
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
		// Still read-only: expose children that outlive the configuration so lifecycle callers do not confuse
		// a missing Fast Launch record with completed snapshot cleanup.
		st.Snapshots, err = p.fastLaunchSnapshots(ctx, imageID)
		return st, nil, err
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
// owned by the account, tagged CreatedBy=EC2 Fast Launch, with the service's exact
// image description. No substring/tag-value heuristic may adopt a different image.
func (p *Provider) fastLaunchSnapshots(ctx context.Context, imageID string) (int, error) {
	n := 0
	var next *string
	for {
		if err := p.describeBucket.Wait(ctx); err != nil {
			return n, fmt.Errorf("ec2 DescribeSnapshots: %w", err)
		}
		out, err := p.api.DescribeSnapshots(ctx, &ec2sdk.DescribeSnapshotsInput{
			OwnerIds:   []string{"self"},
			Filters:    []ec2types.Filter{filter("tag:"+FastLaunchCreatedByTag, FastLaunchCreatedByValue), filter("description", fastLaunchDescription+imageID)},
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
	return aws.ToString(s.Description) == fastLaunchDescription+imageID
}

func (p *Provider) fastLaunchTagFilters() ([]ec2types.Filter, error) {
	if len(p.opts.ExtraTags) == 0 {
		return nil, fmt.Errorf("%w: Fast Launch mutations require nonempty ownership ExtraTags", ports.ErrInvalid)
	}
	filters := make([]ec2types.Filter, 0, len(p.opts.ExtraTags))
	for _, k := range slices.Sorted(maps.Keys(p.opts.ExtraTags)) {
		if p.opts.ExtraTags[k] == "" || k == FastLaunchCreatedByTag || k == fastLaunchTemplateTag {
			return nil, fmt.Errorf("%w: invalid Fast Launch ownership tag %s", ports.ErrInvalid, k)
		}
		filters = append(filters, filter("tag:"+k, p.opts.ExtraTags[k]))
	}
	return filters, nil
}

func (p *Provider) fastLaunchOwnedImage(ctx context.Context, id string) error {
	filters, err := p.fastLaunchTagFilters()
	if err != nil {
		return err
	}
	if err := p.describeBucket.Wait(ctx); err != nil {
		return err
	}
	out, err := p.api.DescribeImages(ctx, &ec2sdk.DescribeImagesInput{Owners: []string{"self"}, ImageIds: []string{id}, Filters: filters})
	if err != nil {
		return p.apiErr("DescribeImages", p.describeBucket, err)
	}
	for _, img := range out.Images {
		if aws.ToString(img.ImageId) == id && fastLaunchTagsMatch(img.Tags, p.opts.ExtraTags) {
			return nil
		}
	}
	return fmt.Errorf("%w: Fast Launch image is not owned with the configured tags", ports.ErrInvalid)
}

func (p *Provider) fastLaunchOwnedTemplate(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("%w: Fast Launch requires a tagged launch template", ports.ErrInvalid)
	}
	filters, err := p.fastLaunchTagFilters()
	if err != nil {
		return err
	}
	if err := p.describeBucket.Wait(ctx); err != nil {
		return err
	}
	// LaunchTemplateIds is the API parameter; launch-template-id is not a supported filter.
	out, err := p.api.DescribeLaunchTemplates(ctx, &ec2sdk.DescribeLaunchTemplatesInput{LaunchTemplateIds: []string{id}, Filters: filters})
	if err != nil {
		return p.apiErr("DescribeLaunchTemplates", p.describeBucket, err)
	}
	for _, lt := range out.LaunchTemplates {
		if aws.ToString(lt.LaunchTemplateId) == id && fastLaunchTagsMatch(lt.Tags, p.opts.ExtraTags) {
			return nil
		}
	}
	return fmt.Errorf("%w: Fast Launch template is not owned with the configured tags", ports.ErrInvalid)
}

func fastLaunchTagsMatch(tags []ec2types.Tag, want map[string]string) bool {
	have := fromEC2Tags(tags)
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// Reconcile only on explicit enable, including its idempotent path. Describe stays read-only.
// Collect/validate the entire paginated set before any write, so a conflicting child fails closed.
func (p *Provider) tagFastLaunchSnapshots(ctx context.Context, imageID, templateID string) error {
	var ids []string
	var next *string
	for {
		if err := p.describeBucket.Wait(ctx); err != nil {
			return err
		}
		out, err := p.api.DescribeSnapshots(ctx, &ec2sdk.DescribeSnapshotsInput{
			OwnerIds: []string{"self"}, MaxResults: aws.Int32(1000), NextToken: next,
			Filters: []ec2types.Filter{filter("tag:"+FastLaunchCreatedByTag, FastLaunchCreatedByValue), filter("tag:"+fastLaunchTemplateTag, templateID), filter("description", fastLaunchDescription+imageID)},
		})
		if err != nil {
			return p.apiErr("DescribeSnapshots", p.describeBucket, err)
		}
		for _, s := range out.Snapshots {
			have := fromEC2Tags(s.Tags)
			if !snapshotReferences(s, imageID) || have[FastLaunchCreatedByTag] != FastLaunchCreatedByValue || have[fastLaunchTemplateTag] != templateID {
				continue
			}
			missing := false
			for k, v := range p.opts.ExtraTags {
				got, exists := have[k]
				if exists && got != v {
					return fmt.Errorf("%w: Fast Launch child %s has a conflicting %s tag", ports.ErrInvalid, aws.ToString(s.SnapshotId), k)
				}
				missing = missing || !exists
			}
			if missing {
				ids = append(ids, aws.ToString(s.SnapshotId))
			}
		}
		if aws.ToString(out.NextToken) == "" {
			break
		}
		next = out.NextToken
	}
	for batch := range slices.Chunk(ids, 1000) {
		if err := p.mutateBucket.Wait(ctx); err != nil {
			return err
		}
		_, err := p.api.CreateTags(ctx, &ec2sdk.CreateTagsInput{Resources: batch, Tags: toEC2Tags(p.opts.ExtraTags)})
		if err != nil {
			return p.apiErr("CreateTags", p.mutateBucket, err)
		}
	}
	return nil
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
