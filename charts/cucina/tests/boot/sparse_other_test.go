// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !windows

package boot_test

import "testing"

// On Unix the pinned binary's ftruncate already creates sparse files.
func prepareSparseBackingFile(_ *testing.T, _ string) {}
