// SPDX-License-Identifier: FSL-1.1-ALv2

package remote

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"
)

// NewLocal returns a POSIX campaign host. Live job ownership belongs to this
// Host, not to numeric identifiers left in a previous harness's scratch files.
func NewLocal(name, workDir string) Host {
	return &scriptHost{name: name, os: runtime.GOOS, workDir: workDir, t: &localTransport{}, d: sh{}, putChunk: 1 << 20}
}

type localTransport struct {
	mu   sync.Mutex
	jobs map[string]*localJob
}

func (*localTransport) exec(ctx context.Context, script string, timeout time.Duration) (string, int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return "", -1, err
	}
	cmd := exec.Command("/bin/sh", "-c", script)
	pr, pw, err := os.Pipe()
	if err != nil {
		return "", -1, err
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	owner, err := startOwned(cmd)
	_ = pw.Close()
	if err != nil {
		_ = pr.Close()
		return "", -1, err
	}
	var out bytes.Buffer
	copied := make(chan error, 1)
	go func() { _, err := io.Copy(&out, pr); copied <- err }()
	stop := false
	select {
	case <-ctx.Done():
		stop = true
		err = ctx.Err()
	case <-owner.exited:
	}
	if !stop {
		select {
		case copyErr := <-copied:
			err = copyErr
		case <-ctx.Done():
			stop = true
			err = ctx.Err()
		case <-time.After(time.Second):
			stop = true
			err = errors.New("local command descendants retained output pipes after exit")
		}
	}
	finishErr := owner.finish(stop)
	if stop {
		_ = pr.Close()
		<-copied
	} else {
		_ = pr.Close()
	}
	var exit *exec.ExitError
	if !stop && errors.As(finishErr, &exit) && err == nil {
		return out.String(), exit.ExitCode(), nil
	}
	if err = errors.Join(err, finishErr); err != nil {
		return out.String(), -1, err
	}
	return out.String(), 0, nil
}
