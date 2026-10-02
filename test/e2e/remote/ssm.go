// SPDX-License-Identifier: FSL-1.1-ALv2

package remote

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// SSMAPI is the narrow SSM surface the transport uses.
type SSMAPI interface {
	SendCommand(ctx context.Context, in *ssm.SendCommandInput, optFns ...func(*ssm.Options)) (*ssm.SendCommandOutput, error)
	GetCommandInvocation(ctx context.Context, in *ssm.GetCommandInvocationInput, optFns ...func(*ssm.Options)) (*ssm.GetCommandInvocationOutput, error)
}

// NewSSMClient builds an SSM client for the campaign profile and region (§12:
// profile "default", us-west-1 only).
func NewSSMClient(ctx context.Context, profile, region string) (*ssm.Client, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithSharedConfigProfile(profile), config.WithRegion(region))
	if err != nil {
		return nil, err
	}
	return ssm.NewFromConfig(cfg), nil
}

// SSMHostConfig describes a VM reached through SSM Run Command.
type SSMHostConfig struct {
	Name       string
	OS         string // linux | windows
	InstanceID string
	WorkDir    string // default /var/tmp/cucina-e2e or C:\cucina-e2e
	Comment    string // shown in the SSM console, e.g. "cucina-e2e T1"
	// Profile/Region enable bulk transfers through SSM port forwarding
	// (`aws ssm start-session`, Session Manager plugin on the dev Mac).
	Profile, Region string
}

// NewSSMHost returns a Host backed by SSM Run Command.
func NewSSMHost(cfg SSMHostConfig, api SSMAPI) (Host, error) {
	h := &scriptHost{name: cfg.Name, os: cfg.OS, workDir: cfg.WorkDir, putChunk: 24 << 10}
	t := &ssmTransport{api: api, instanceID: cfg.InstanceID, comment: cfg.Comment, sleep: RealSleep}
	switch cfg.OS {
	case Linux:
		h.d, t.document = sh{}, "AWS-RunShellScript"
		if h.workDir == "" {
			h.workDir = "/var/tmp/cucina-e2e"
		}
	case Windows:
		h.d, t.document = ps{}, "AWS-RunPowerShellScript"
		if h.workDir == "" {
			h.workDir = `C:\cucina-e2e`
		}
	default:
		return nil, fmt.Errorf("ssm host %s: unsupported os %q", cfg.Name, cfg.OS)
	}
	h.t = t
	if cfg.Profile != "" && cfg.Region != "" {
		h.bulk = &portForward{instanceID: cfg.InstanceID, profile: cfg.Profile, region: cfg.Region}
		h.bulkThreshold = 256 << 10
	}
	return h, nil
}

type ssmTransport struct {
	api        SSMAPI
	instanceID string
	document   string
	comment    string
	sleep      Sleeper
}

// SSM limits: executionTimeout is 1..172800 s; output beyond 24,000
// characters is truncated (our scripts stay below it).
func (t *ssmTransport) exec(ctx context.Context, script string, timeout time.Duration) (string, int, error) {
	secs := int(timeout / time.Second)
	if secs < 30 {
		secs = 30
	}
	if secs > 172800 {
		secs = 172800
	}
	in := &ssm.SendCommandInput{
		DocumentName:   aws.String(t.document),
		InstanceIds:    []string{t.instanceID},
		Parameters:     map[string][]string{"commands": {script}, "executionTimeout": {strconv.Itoa(secs)}},
		TimeoutSeconds: aws.Int32(600), // delivery (the agent must pick it up within 10 min)
	}
	if t.comment != "" {
		in.Comment = aws.String(truncate(t.comment, 100))
	}
	sent, err := t.api.SendCommand(ctx, in)
	if err != nil {
		return "", -1, fmt.Errorf("ssm send-command: %w", err)
	}
	id := aws.ToString(sent.Command.CommandId)
	deadline := time.Now().Add(time.Duration(secs)*time.Second + 11*time.Minute)
	wait := 300 * time.Millisecond
	for {
		if err := t.sleep(ctx, wait); err != nil {
			return "", -1, err
		}
		if wait < 2*time.Second {
			wait *= 2
		}
		inv, err := t.api.GetCommandInvocation(ctx, &ssm.GetCommandInvocationInput{CommandId: aws.String(id), InstanceId: aws.String(t.instanceID)})
		var nf *ssmtypes.InvocationDoesNotExist
		switch {
		case errors.As(err, &nf):
			// Eventual consistency right after SendCommand.
		case err != nil:
			return "", -1, fmt.Errorf("ssm get-command-invocation %s: %w", id, err)
		default:
			out := aws.ToString(inv.StandardOutputContent)
			switch inv.Status {
			case ssmtypes.CommandInvocationStatusSuccess:
				return out, 0, nil
			case ssmtypes.CommandInvocationStatusFailed:
				return out, int(inv.ResponseCode), nil
			case ssmtypes.CommandInvocationStatusTimedOut, ssmtypes.CommandInvocationStatusCancelled, ssmtypes.CommandInvocationStatusCancelling:
				return out, int(inv.ResponseCode), fmt.Errorf("ssm command %s %s: %s", id, inv.Status, truncate(aws.ToString(inv.StandardErrorContent), 500))
			}
		}
		if time.Now().After(deadline) {
			return "", -1, fmt.Errorf("ssm command %s: no result after %s", id, time.Duration(secs)*time.Second)
		}
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
