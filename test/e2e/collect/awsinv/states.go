// SPDX-License-Identifier: FSL-1.1-ALv2

package awsinv

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

// DescribeStates reads only instance states, INCLUDING terminated instances.
// It is for lifecycle proof, not residue: callers still use Describe for
// volumes/interfaces/addresses. An absent instance is never fabricated as a
// terminated one. Every request retains the campaign filters.
func (inv *Inventory) DescribeStates(ctx context.Context) (Snapshot, error) {
	if inv.Tags["cucina:run"] == "" || inv.Tags["cucina:env"] == "" {
		return Snapshot{}, fmt.Errorf("awsinv: refusing an untagged state describe")
	}
	s := Snapshot{At: inv.now()}
	p := ec2.NewDescribeInstancesPaginator(inv.API, &ec2.DescribeInstancesInput{Filters: inv.filters()})
	for p.HasMorePages() {
		out, err := p.NextPage(ctx)
		if err != nil {
			return s, fmt.Errorf("describe instance states: %w", err)
		}
		for _, r := range out.Reservations {
			for _, i := range r.Instances {
				// Recheck returned identity even with server-side filters. It keeps
				// malformed/incomplete sources from becoming a false lifecycle.
				for k, want := range inv.Tags {
					if tag(i.Tags, k) != want {
						return s, fmt.Errorf("instance state response escaped campaign tag filter")
					}
				}
				state := "unknown"
				if i.State != nil {
					state = string(i.State.Name)
				}
				s.Instances = append(s.Instances, Instance{ID: aws.ToString(i.InstanceId), Pool: tag(i.Tags, "cucina:pool"), State: state, LaunchTime: aws.ToTime(i.LaunchTime)})
			}
		}
	}
	return s, nil
}
