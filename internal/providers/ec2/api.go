// SPDX-License-Identifier: FSL-1.1-ALv2

package ec2

import (
	"context"

	ec2sdk "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
)

// ec2API is the narrow slice of the EC2 SDK client the provider uses. Every AWS
// call goes through it, so the adapter is tested against a hand-written in-memory
// fake of exactly this surface (fake_test.go). The IAM actions in
// docs/dev/ec2-provider.md are exactly these operations.
type ec2API interface {
	RunInstances(ctx context.Context, in *ec2sdk.RunInstancesInput, opts ...func(*ec2sdk.Options)) (*ec2sdk.RunInstancesOutput, error)
	DescribeInstances(ctx context.Context, in *ec2sdk.DescribeInstancesInput, opts ...func(*ec2sdk.Options)) (*ec2sdk.DescribeInstancesOutput, error)
	TerminateInstances(ctx context.Context, in *ec2sdk.TerminateInstancesInput, opts ...func(*ec2sdk.Options)) (*ec2sdk.TerminateInstancesOutput, error)

	DescribeVolumes(ctx context.Context, in *ec2sdk.DescribeVolumesInput, opts ...func(*ec2sdk.Options)) (*ec2sdk.DescribeVolumesOutput, error)
	DeleteVolume(ctx context.Context, in *ec2sdk.DeleteVolumeInput, opts ...func(*ec2sdk.Options)) (*ec2sdk.DeleteVolumeOutput, error)
	DescribeNetworkInterfaces(ctx context.Context, in *ec2sdk.DescribeNetworkInterfacesInput, opts ...func(*ec2sdk.Options)) (*ec2sdk.DescribeNetworkInterfacesOutput, error)
	DeleteNetworkInterface(ctx context.Context, in *ec2sdk.DeleteNetworkInterfaceInput, opts ...func(*ec2sdk.Options)) (*ec2sdk.DeleteNetworkInterfaceOutput, error)

	DescribeImages(ctx context.Context, in *ec2sdk.DescribeImagesInput, opts ...func(*ec2sdk.Options)) (*ec2sdk.DescribeImagesOutput, error)
	DescribeSnapshots(ctx context.Context, in *ec2sdk.DescribeSnapshotsInput, opts ...func(*ec2sdk.Options)) (*ec2sdk.DescribeSnapshotsOutput, error)

	EnableFastLaunch(ctx context.Context, in *ec2sdk.EnableFastLaunchInput, opts ...func(*ec2sdk.Options)) (*ec2sdk.EnableFastLaunchOutput, error)
	DisableFastLaunch(ctx context.Context, in *ec2sdk.DisableFastLaunchInput, opts ...func(*ec2sdk.Options)) (*ec2sdk.DisableFastLaunchOutput, error)
	DescribeFastLaunchImages(ctx context.Context, in *ec2sdk.DescribeFastLaunchImagesInput, opts ...func(*ec2sdk.Options)) (*ec2sdk.DescribeFastLaunchImagesOutput, error)
}

// pricingAPI is the Price List API surface (the client lives in us-east-1).
type pricingAPI interface {
	GetProducts(ctx context.Context, in *pricing.GetProductsInput, opts ...func(*pricing.Options)) (*pricing.GetProductsOutput, error)
}

var (
	_ ec2API     = (*ec2sdk.Client)(nil)
	_ pricingAPI = (*pricing.Client)(nil)
)
