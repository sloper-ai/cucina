// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !darwin && !linux

package privdrop

import (
	"errors"
	"fmt"
	"runtime"
)

// DropAndExec cannot implement Unix user/group and exec semantics here. Fail
// before changing credentials or starting a program, including in CLI builds
// compiled for platforms that can only exercise hostd's portable components.
func DropAndExec(TrampolineArgs) error {
	return fmt.Errorf("hostd privilege-drop trampoline on %s: %w", runtime.GOOS, errors.ErrUnsupported)
}
