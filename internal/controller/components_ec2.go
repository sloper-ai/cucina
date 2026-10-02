// SPDX-License-Identifier: FSL-1.1-ALv2

package controller

import (
	"context"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/internal/providers/ec2"
)

// EC2 adapter (ports.Compute, agent `ec2`). Credentials come from the default
// chain: IRSA / EKS Pod Identity, or the k3s node's instance profile (R-CP-8).
func init() {
	ProvideCompute(func(ctx context.Context, d *Deps) (ports.Compute, error) {
		a := d.Config.AWS
		cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(a.Region))
		if err != nil {
			return nil, err
		}
		opts := ec2.Options{
			Region:            a.Region,
			ExtraTags:         a.ExtraTags,
			PricingRegionCode: a.PricingRegionCode,
			Clock:             d.Clock,
			Logger:            d.Log.With("component", "ec2"),
		}
		if d.Metrics != nil {
			opts.OnAPIError = func(op, code string) { d.Metrics.EC2APIErrors.WithLabelValues(op, code).Inc() }
		}
		return ec2.New(cfg, opts)
	})
}
