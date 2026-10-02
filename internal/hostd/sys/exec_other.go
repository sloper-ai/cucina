// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !darwin && !linux

package sys

import (
	"context"
	"errors"
	"fmt"
	"runtime"

	"github.com/sloper-ai/cucina/internal/ports"
)

// Run refuses hostd's Unix process adapter on unsupported platforms before
// wrapping or launching the command, even when RunAs or ModeNone is requested.
func (*Exec) Run(context.Context, ports.Command) (ports.ExecResult, error) {
	return ports.ExecResult{}, fmt.Errorf("hostd process execution on %s: %w", runtime.GOOS, errors.ErrUnsupported)
}

// Start cannot provide the required Unix session/identity semantics here.
// Portable worker-agent execution has its own adapter; there is no fallback.
func (*Exec) Start(context.Context, ports.Command) (ports.Process, error) {
	return nil, fmt.Errorf("hostd process execution on %s: %w", runtime.GOOS, errors.ErrUnsupported)
}
