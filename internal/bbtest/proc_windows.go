// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build windows

package bbtest

import "os/exec"

func setProcessGroup(*exec.Cmd) {}

// Windows has no SIGTERM; Buildbarn binaries are killed outright in tests.
func terminate(cmd *exec.Cmd) { _ = cmd.Process.Kill() }

func kill(cmd *exec.Cmd) { _ = cmd.Process.Kill() }
