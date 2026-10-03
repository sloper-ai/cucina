// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build windows

package boot_test

import (
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

// The pinned bb_storage grows files with SetEndOfFile BEFORE marking them
// sparse. A fresh 100 GiB–2 TiB profile therefore fails on a small Windows
// runner even though its sparse contents would fit. Mark only our fresh test
// backing files first; the rendered file sizes and all store dimensions remain
// unchanged, and the unmodified binary still performs its own initialization.
func prepareSparseBackingFile(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("create sparse backing file: %v", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Errorf("close sparse backing file: %v", err)
		}
	}()
	var returned uint32
	if err := windows.DeviceIoControl(windows.Handle(f.Fd()), windows.FSCTL_SET_SPARSE,
		nil, 0, nil, 0, &returned, nil); err != nil {
		t.Fatalf("mark backing file sparse before Buildbarn resizes it: %v", err)
	}
}
