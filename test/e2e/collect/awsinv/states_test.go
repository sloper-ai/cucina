// SPDX-License-Identifier: FSL-1.1-ALv2

package awsinv

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/stretchr/testify/require"
)

type stateSource struct {
	EC2API
	rows []types.Instance
	err  error
}

func (s stateSource) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: s.rows}}}, nil
}

// Guards T8 provider proof: retain explicit terminated states, reject scope
// escape and provider errors, and never fabricate termination for absent IDs.
func TestDescribeTerminalStates(t *testing.T) {
	a := inst("i-owned", with(campaign, "cucina:pool", "linux"))
	a.State = &types.InstanceState{Name: types.InstanceStateNameTerminated}
	for _, tc := range []struct {
		name   string
		source stateSource
		want   int
		bad    bool
	}{
		{"terminal retained", stateSource{rows: []types.Instance{a}}, 1, false},
		{"absent remains absent", stateSource{}, 0, false},
		{"API failure", stateSource{err: errors.New("unavailable")}, 0, true},
		{"wrong tags", stateSource{rows: []types.Instance{inst("i-other", with(campaign, "cucina:run", "other"))}}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := (&Inventory{API: tc.source, Tags: campaign}).DescribeStates(context.Background())
			if tc.bad {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Len(t, s.Instances, tc.want)
				if tc.want > 0 {
					require.Equal(t, "terminated", s.Instances[0].State)
				}
			}
		})
	}
}
