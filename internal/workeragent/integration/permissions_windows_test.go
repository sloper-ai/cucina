// SPDX-License-Identifier: FSL-1.1-ALv2
//go:build windows

package integration_test

import (
	"os"
	"testing"
	"unsafe"

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
		control, _, err := sd.Control()
		require.NoError(t, err)
		require.NotZero(t, control&windows.SE_DACL_PROTECTED, "the key DACL must block inheritance")
		// Windows may add SE_DACL_AUTO_INHERITED without adding inherited ACEs.
		// Compare the exact grants, not that bookkeeping bit in serialized SDDL.
		type grant struct {
			sid  string
			mask windows.ACCESS_MASK
		}
		grants := func(descriptor *windows.SECURITY_DESCRIPTOR) []grant {
			dacl, _, err := descriptor.DACL()
			require.NoError(t, err)
			require.NotNil(t, dacl, "a null DACL permits unrestricted access")
			entries := make([]grant, 0, dacl.AceCount)
			for i := uint32(0); i < uint32(dacl.AceCount); i++ {
				var ace *windows.ACCESS_ALLOWED_ACE
				require.NoError(t, windows.GetAce(dacl, i, &ace))
				require.Equal(t, uint8(windows.ACCESS_ALLOWED_ACE_TYPE), ace.Header.AceType)
				require.Zero(t, ace.Header.AceFlags, "key grants must be explicit, non-inheriting ACEs")
				sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
				entries = append(entries, grant{sid.String(), ace.Mask})
			}
			return entries
		}
		expected, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;" + readers[0] + ")")
		require.NoError(t, err)
		require.ElementsMatch(t, grants(expected), grants(sd), "only the exact SYSTEM/Administrators and configured-reader grants may access the private key")
	}
}
