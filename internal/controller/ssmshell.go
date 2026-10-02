// SPDX-License-Identifier: FSL-1.1-ALv2

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
)

// SSMAPI is the subset of the SSM client the shell uses.
type SSMAPI interface {
	SendCommand(ctx context.Context, in *ssm.SendCommandInput, opts ...func(*ssm.Options)) (*ssm.SendCommandOutput, error)
	GetCommandInvocation(ctx context.Context, in *ssm.GetCommandInvocationInput, opts ...func(*ssm.Options)) (*ssm.GetCommandInvocationOutput, error)
}

// SSMShell runs the management API's read-only diagnostic scripts on EC2
// workers through SSM Run Command (mgmt.InstanceShell: `cucinactl workers
// logs`, R-OBS-4). It refuses any instance that a tag-filtered Describe does
// not show as this cluster's running worker, so the controller role can never
// be used to run commands elsewhere (the IAM policy scopes ssm:SendCommand by
// the same tags).
type SSMShell struct {
	API     SSMAPI
	Compute ports.Compute
	Cluster string
	Clock   ports.Clock
	// Poll is the GetCommandInvocation period (default 1 s); Timeout bounds a
	// whole call (default 60 s).
	Poll    time.Duration
	Timeout time.Duration
}

// ErrNotWorker is returned for instances that are not this cluster's running workers.
var ErrNotWorker = errors.New("not a running worker of this cluster")

// RunScript implements mgmt.InstanceShell.
func (s *SSMShell) RunScript(ctx context.Context, instanceID string, windows bool, script string) ([]byte, error) {
	ins, err := s.Compute.Describe(ctx, ports.InstanceFilter{Cluster: s.Cluster, IDs: []string{instanceID}, States: []ports.InstanceState{ports.InstanceRunning}})
	if err != nil {
		return nil, err
	}
	if len(ins) != 1 || ins[0].ID != instanceID || ins[0].Tags[domain.TagManagedBy] != domain.ManagedByValue || ins[0].Tags[domain.TagCluster] != s.Cluster {
		return nil, fmt.Errorf("instance %s: %w (%w)", instanceID, ErrNotWorker, ports.ErrNotFound)
	}
	timeout, poll := s.Timeout, s.Poll
	if timeout <= 0 {
		timeout = time.Minute
	}
	if poll <= 0 {
		poll = time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	doc := "AWS-RunShellScript"
	if windows {
		doc = "AWS-RunPowerShellScript"
	}
	out, err := s.API.SendCommand(ctx, &ssm.SendCommandInput{
		DocumentName:   aws.String(doc),
		InstanceIds:    []string{instanceID},
		Parameters:     map[string][]string{"commands": {script}},
		TimeoutSeconds: aws.Int32(30),
		Comment:        aws.String("cucina: read-only worker diagnostics"),
	})
	if err != nil {
		return nil, fmt.Errorf("ssm SendCommand: %w", err)
	}
	if out.Command == nil || out.Command.CommandId == nil {
		return nil, errors.New("ssm SendCommand returned no command id")
	}
	id := out.Command.CommandId
	for {
		if err := s.Clock.Sleep(ctx, poll); err != nil {
			return nil, fmt.Errorf("ssm command %s: %w", aws.ToString(id), err)
		}
		inv, err := s.API.GetCommandInvocation(ctx, &ssm.GetCommandInvocationInput{CommandId: id, InstanceId: aws.String(instanceID)})
		if err != nil {
			var notYet *ssmtypes.InvocationDoesNotExist
			if errors.As(err, &notYet) {
				continue // the invocation becomes visible shortly after SendCommand
			}
			return nil, fmt.Errorf("ssm GetCommandInvocation: %w", err)
		}
		switch inv.Status {
		case ssmtypes.CommandInvocationStatusSuccess:
			return []byte(aws.ToString(inv.StandardOutputContent)), nil
		case ssmtypes.CommandInvocationStatusPending, ssmtypes.CommandInvocationStatusInProgress, ssmtypes.CommandInvocationStatusDelayed:
			continue
		default:
			msg := strings.TrimSpace(aws.ToString(inv.StandardErrorContent))
			if len(msg) > 512 {
				msg = msg[:512]
			}
			return nil, fmt.Errorf("ssm command on %s ended %s: %s", instanceID, inv.Status, msg)
		}
	}
}
