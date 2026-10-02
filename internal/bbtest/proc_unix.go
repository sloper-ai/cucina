// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build unix

package bbtest

import (
	"os/exec"
	"syscall"
)

// setProcessGroup keeps children in the test's process group, so a test
// runner that kills the group on timeout (bazel, Ctrl-C) takes them along.
func setProcessGroup(*exec.Cmd) {}

func terminate(cmd *exec.Cmd) { _ = cmd.Process.Signal(syscall.SIGTERM) }

func kill(cmd *exec.Cmd) { _ = cmd.Process.Kill() }
