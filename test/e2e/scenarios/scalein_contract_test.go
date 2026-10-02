// SPDX-License-Identifier: FSL-1.1-ALv2

package scenarios_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/test/e2e/collect/awsinv"
	"github.com/sloper-ai/cucina/test/e2e/collect/scalein"
	"github.com/sloper-ai/cucina/test/e2e/harness"
	"github.com/sloper-ai/cucina/test/e2e/infra"
	"github.com/sloper-ai/cucina/test/e2e/scenarios"
)

type residueEC2 struct {
	awsinv.EC2API
	leaked bool
}

func (f residueEC2) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return &ec2.DescribeInstancesOutput{}, nil
}
func (f residueEC2) DescribeVolumes(context.Context, *ec2.DescribeVolumesInput, ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error) {
	v := &ec2.DescribeVolumesOutput{}
	if f.leaked {
		v.Volumes = []types.Volume{{}}
	}
	return v, nil
}
func (f residueEC2) DescribeNetworkInterfaces(context.Context, *ec2.DescribeNetworkInterfacesInput, ...func(*ec2.Options)) (*ec2.DescribeNetworkInterfacesOutput, error) {
	return &ec2.DescribeNetworkInterfacesOutput{}, nil
}
func (f residueEC2) DescribeAddresses(context.Context, *ec2.DescribeAddressesInput, ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error) {
	return &ec2.DescribeAddressesOutput{}, nil
}

// Guards T8's public scenario integration: completed live timing qualifies,
// missing telemetry stays unavailable, and a real residue failure wins even
// when timing evidence is incomplete. All provider calls use the local fake.
func TestLiveScaleInScenario(t *testing.T) {
	for _, tc := range []struct {
		name          string
		missing, leak bool
		changed, late bool
		want          harness.Status
	}{
		{"complete", false, false, false, false, harness.StatusPass},
		{"missing idle", true, false, false, false, harness.StatusSkip},
		{"leak and missing idle", true, true, false, false, harness.StatusFail},
		{"overwritten evidence", false, false, true, false, harness.StatusFail},
		{"later attempt", false, false, false, true, harness.StatusFail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t0 := time.Unix(1_800_000_000, 0).UTC()
			idle := 5 * time.Second
			samples := []scalein.Sample{
				{Started: t0, Finished: t0, Instances: []scalein.Instance{{Node: "i-test", Pool: "linux", State: "running"}}, Workers: []scalein.Worker{{Node: "i-test", Pool: "linux", State: "idle", IdleFor: &idle, Drained: true}}, Queues: map[string]scalein.Queue{"linux": {}}},
				{Started: t0.Add(10 * time.Second), Finished: t0.Add(11 * time.Second), Instances: []scalein.Instance{{Node: "i-test", Pool: "linux", State: "terminated"}}, Queues: map[string]scalein.Queue{"linux": {}}},
			}
			if tc.missing {
				samples[0].Workers[0].IdleFor = nil
			}
			path := filepath.Join(root, "run", "T7", "scale-in-evidence.json")
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			b, err := json.Marshal(map[string]any{"schemaVersion": 1, "runId": "run", "policies": map[string]scalein.Policy{"linux": {IdleTimeout: time.Minute, DrainGrace: 2 * time.Minute}}, "samples": samples})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, b, 0o600))
			hash := sha256.Sum256(b)
			finish := t0.Add(time.Minute)
			if tc.late {
				finish = t0.Add(5 * time.Second)
			}
			if tc.changed {
				require.NoError(t, os.WriteFile(path, append(b, '\n'), 0o600))
			}
			env := &harness.Env{RunID: "run", ArtifactsDir: root}
			reg := harness.NewRegistry()
			scenarios.Register(reg)
			s, ok := reg.Get("T8")
			require.True(t, ok)
			now := t0
			c := &harness.Context{Context: context.Background(), Env: env, Scenario: s, Result: &harness.Result{ID: "T8", RunID: "run"}, Prior: map[string]*harness.Result{"T7": {Started: t0, Finished: finish, Artifacts: []harness.Artifact{{Name: "scale-in-evidence", Path: filepath.Join("T7", "scale-in-evidence.json"), SHA256: hex.EncodeToString(hash[:]), Bytes: int64(len(b))}}, Values: map[string]any{"scaleInEvidencePath": path}}}, Now: func() time.Time { now = now.Add(time.Hour); return now }, Services: &infra.Services{Env: env, EC2: &awsinv.Inventory{API: residueEC2{leaked: tc.leak}, Tags: map[string]string{"cucina:env": "e2e", "cucina:run": "run"}}}}
			err = s.Run(c)
			require.Equal(t, tc.want, harness.Classify(err))
			if !tc.changed && !tc.late {
				require.NotEmpty(t, c.Result.Values["scaleIn"])
			}
			if tc.leak {
				require.False(t, c.Result.NFRs[0].Pass)
			}
		})
	}
}
