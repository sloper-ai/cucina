// SPDX-License-Identifier: FSL-1.1-ALv2
//go:build !windows

package integration_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func bootstrapPlatform(testing.TB) (string, []string) { return "linux", nil }

func assertBootstrapFileAccess(t testing.TB, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	require.NoError(t, err, path)
	require.Equal(t, want, fi.Mode().Perm(), path)
}
