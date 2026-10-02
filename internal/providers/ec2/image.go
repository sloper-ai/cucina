// SPDX-License-Identifier: FSL-1.1-ALv2

package ec2

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2sdk "github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
)

// imageInfo holds the immutable AMI facts Launch needs (cached per image ID).
type imageInfo struct {
	ID         string
	RootDevice string
	Mappings   []ec2types.BlockDeviceMapping
}

// snapshotGiB is the size of the snapshot behind device dev (0 if unknown).
func (i imageInfo) snapshotGiB(dev string) int {
	for _, m := range i.Mappings {
		if m.DeviceName != nil && normDevice(*m.DeviceName) == normDevice(dev) && m.Ebs != nil {
			return int(aws.ToInt32(m.Ebs.VolumeSize))
		}
	}
	return 0
}

// imageInfo returns the cached launch facts of an available AMI.
func (p *Provider) imageInfo(ctx context.Context, id string) (imageInfo, error) {
	if v, ok := p.images.GetIfPresent(id); ok {
		return v, nil
	}
	img, err := p.imageByID(ctx, id)
	if err != nil {
		return imageInfo{}, err
	}
	info := imageInfo{ID: id, RootDevice: aws.ToString(img.RootDeviceName), Mappings: img.BlockDeviceMappings}
	p.images.Set(id, info)
	return info, nil
}

// imageByID describes one AMI and requires it to be available.
func (p *Provider) imageByID(ctx context.Context, id string) (ec2types.Image, error) {
	if err := p.describeBucket.Wait(ctx); err != nil {
		return ec2types.Image{}, fmt.Errorf("ec2 DescribeImages: %w", err)
	}
	out, err := p.api.DescribeImages(ctx, &ec2sdk.DescribeImagesInput{ImageIds: []string{id}})
	if err != nil {
		return ec2types.Image{}, p.apiErr("DescribeImages", p.describeBucket, err)
	}
	for _, img := range out.Images {
		if aws.ToString(img.ImageId) != id {
			continue
		}
		if img.State != ec2types.ImageStateAvailable {
			return ec2types.Image{}, fmt.Errorf("%w: image %s is %s", ports.ErrImageNotFound, id, img.State)
		}
		return img, nil
	}
	return ec2types.Image{}, fmt.Errorf("%w: image %s", ports.ErrImageNotFound, id)
}

// ResolveImage resolves an AMI by ID (any AMI the account may launch) or by tags
// (the newest available AMI owned by this account carrying every selector tag).
func (p *Provider) ResolveImage(ctx context.Context, sel ports.ImageSelector) (ports.Image, error) {
	switch {
	case sel.ID != "":
		img, err := p.imageByID(ctx, sel.ID)
		if err != nil {
			return ports.Image{}, err
		}
		return toImage(img), nil
	case len(sel.Tags) == 0:
		return ports.Image{}, fmt.Errorf("%w: image selector has neither an ID nor tags", ports.ErrInvalid)
	}
	filters := []ec2types.Filter{filter("state", string(ec2types.ImageStateAvailable))}
	for _, k := range slices.Sorted(maps.Keys(sel.Tags)) {
		filters = append(filters, filter("tag:"+k, sel.Tags[k]))
	}
	var best *ec2types.Image
	var bestAt time.Time
	var next *string
	for {
		if err := p.describeBucket.Wait(ctx); err != nil {
			return ports.Image{}, fmt.Errorf("ec2 DescribeImages: %w", err)
		}
		out, err := p.api.DescribeImages(ctx, &ec2sdk.DescribeImagesInput{
			Owners: []string{"self"}, Filters: filters, MaxResults: aws.Int32(1000), NextToken: next,
		})
		if err != nil {
			return ports.Image{}, p.apiErr("DescribeImages", p.describeBucket, err)
		}
		for i := range out.Images {
			img := &out.Images[i]
			at := imageCreated(*img)
			// Newest wins; ties break on the ID so the answer is deterministic.
			if best == nil || at.After(bestAt) || (at.Equal(bestAt) && aws.ToString(img.ImageId) > aws.ToString(best.ImageId)) {
				best, bestAt = img, at
			}
		}
		if aws.ToString(out.NextToken) == "" {
			break
		}
		next = out.NextToken
	}
	if best == nil {
		return ports.Image{}, fmt.Errorf("%w: no available image owned by this account matches %v", ports.ErrImageNotFound, sel.Tags)
	}
	return toImage(*best), nil
}

func imageCreated(img ec2types.Image) time.Time {
	t, err := time.Parse(time.RFC3339, aws.ToString(img.CreationDate))
	if err != nil {
		return time.Time{}
	}
	return t
}

func toImage(img ec2types.Image) ports.Image {
	tags := fromEC2Tags(img.Tags)
	out := ports.Image{
		ID:         aws.ToString(img.ImageId),
		Name:       aws.ToString(img.Name),
		Arch:       string(img.Architecture),
		CreatedAt:  imageCreated(img),
		Version:    tags[domain.TagImageVersion],
		Generation: tags[domain.TagGeneration],
		Platform:   "linux",
	}
	if img.Platform == ec2types.PlatformValuesWindows || strings.Contains(strings.ToLower(aws.ToString(img.PlatformDetails)), "windows") {
		out.Platform = "windows"
	}
	for _, m := range img.BlockDeviceMappings {
		if m.Ebs == nil {
			continue
		}
		if id := aws.ToString(m.Ebs.SnapshotId); id != "" {
			out.SnapshotIDs = append(out.SnapshotIDs, id)
		}
		out.SizeGiB += int(aws.ToInt32(m.Ebs.VolumeSize))
	}
	return out
}
