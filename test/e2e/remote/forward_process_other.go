// SPDX-License-Identifier: FSL-1.1-ALv2
//go:build !darwin && !linux && !windows

package remote

import (
	"context"
	"errors"
	"os/exec"
)

type forwardProcess struct{}

func ownForwardProcess(*exec.Cmd) (*forwardProcess, error) {
	return nil, errors.New("owned subprocesses require Darwin, Linux or Windows")
}
func (*forwardProcess) started() error { return errors.New("owned process unavailable") }
func (*forwardProcess) waitExited(context.Context) error {
	return errors.New("owned process unavailable")
}
func (*forwardProcess) release() error { return errors.New("owned process unavailable") }
func (*forwardProcess) waitReap(context.Context) error {
	return errors.New("owned process unavailable")
}
func (*forwardProcess) terminate() error { return errors.New("owned process unavailable") }
func (*forwardProcess) kill() error      { return errors.New("owned process unavailable") }
func (*forwardProcess) close() error     { return nil }
