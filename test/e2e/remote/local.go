// SPDX-License-Identifier: FSL-1.1-ALv2

package remote

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"runtime"
	"time"
)

// NewLocal returns the dev Mac (or any POSIX machine running the harness) as
// a Host. workDir holds job directories.
func NewLocal(name, workDir string) Host {
	return &scriptHost{name: name, os: runtime.GOOS, workDir: workDir, t: localTransport{}, d: sh{}, putChunk: 1 << 20}
}

type localTransport struct{}

func (localTransport) exec(ctx context.Context, script string, timeout time.Duration) (string, int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out.String(), ee.ExitCode(), nil
	}
	if err != nil {
		return out.String(), -1, err
	}
	return out.String(), 0, nil
}
