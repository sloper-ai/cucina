// SPDX-License-Identifier: FSL-1.1-ALv2
//go:build windows

package integration_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func bootstrapPlatform(t testing.TB) (string, []string) {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	require.NoError(t, err)
	// Model the configured service reader without requiring the test process
	// itself to run as SYSTEM or an elevated administrator.
	return "windows", []string{user.User.Sid.String()}
}

func assertBootstrapFileAccess(t testing.TB, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	require.NoError(t, err, path)
	require.True(t, fi.Mode().IsRegular(), path)
	// Go maps Windows writeability to mode bits; confidentiality is in the
	// actual protected DACL, not a fictitious POSIX 0600 bit pattern.
	require.Equal(t, os.FileMode(0o666), fi.Mode().Perm(), path)
	if want == 0o600 {
		_, readers := bootstrapPlatform(t)
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		require.NoError(t, err, path)
		expected, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;" + readers[0] + ")")
		require.NoError(t, err)
		require.Equal(t, expected.String(), sd.String(), "only SYSTEM/Administrators and the configured reader may access the private key")
	}
}
