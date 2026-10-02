// SPDX-License-Identifier: FSL-1.1-ALv2

package controller_test

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/controller"
	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/fakes"
	"github.com/sloper-ai/cucina/internal/ports"
)

// fakeSSM answers SendCommand and reports the invocation in progress once,
// then successful.
type fakeSSM struct {
	mu    sync.Mutex
	sent  []string
	polls int
}

func (f *fakeSSM) SendCommand(_ context.Context, in *ssm.SendCommandInput, _ ...func(*ssm.Options)) (*ssm.SendCommandOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, in.InstanceIds...)
	return &ssm.SendCommandOutput{Command: &ssmtypes.Command{CommandId: aws.String("cmd-1")}}, nil
}

func (f *fakeSSM) GetCommandInvocation(context.Context, *ssm.GetCommandInvocationInput, ...func(*ssm.Options)) (*ssm.GetCommandInvocationOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.polls++
	if f.polls == 1 {
		return &ssm.GetCommandInvocationOutput{Status: ssmtypes.CommandInvocationStatusInProgress}, nil
	}
	return &ssm.GetCommandInvocationOutput{Status: ssmtypes.CommandInvocationStatusSuccess, StandardOutputContent: aws.String("bb_worker log")}, nil
}

// Guards R-OBS-4 (worker logs reachable via SSM) and its safety rule: the
// controller only sends commands to running workers of its own cluster
// (tag-filtered Describe); anything else is refused without calling SSM.
func TestSSMShellRunsOnlyOnOwnWorkers(t *testing.T) {
	// The shell polls SSM with its clock: run it in a synctest bubble (fake time).
	synctest.Test(t, testSSMShell)
}

func testSSMShell(t *testing.T) {
	ctx := context.Background()
	clock := fakes.NewClock(time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	compute := fakes.NewCompute(clock, fakes.NewRand(3), fakes.DefaultComputeConfig())
	tags := map[string]string{domain.TagManagedBy: domain.ManagedByValue, domain.TagCluster: "test", domain.TagPool: "linux",
		domain.TagGeneration: "v1", domain.TagLaunchToken: "tok", domain.TagRole: "worker"}
	in, err := compute.Launch(ctx, ports.LaunchRequest{Pool: "linux", Generation: "v1", Token: "tok", ImageID: "ami-1",
		InstanceTypes: []string{"c8i.2xlarge"}, SubnetIDs: []string{"subnet-a"}, Tags: tags})
	require.NoError(t, err)
	clock.Advance(time.Minute)
	compute.Tick()

	api := &fakeSSM{}
	sh := &controller.SSMShell{API: api, Compute: compute, Cluster: "test", Clock: controller.SystemClock{}}
	out, err := sh.RunScript(ctx, in.ID, false, "journalctl -u bb-worker -n 100")
	require.NoError(t, err)
	assert.Equal(t, "bb_worker log", string(out))

	for _, id := range []string{"i-0123456789abcdef0", in.ID} {
		other := &controller.SSMShell{API: api, Compute: compute, Cluster: "another-cluster", Clock: controller.SystemClock{}}
		_, err = other.RunScript(ctx, id, false, "whoami")
		require.Error(t, err)
		require.ErrorIs(t, err, controller.ErrNotWorker)
	}
	assert.Equal(t, []string{in.ID}, api.sent, "SSM is only called for own workers")
}
