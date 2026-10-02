// SPDX-License-Identifier: FSL-1.1-ALv2

package bazelrun_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/test/e2e/bazelrun"
)

// Guards the real Windows baseline exit37: rc-tokenization must not turn
// C:\e2e\repository-cache into relative C:e2erepository-cache. All Windows
// filesystem values use forward slashes; values containing spaces stay one token.
func TestWindowsUserRCPaths(t *testing.T) {
	rc := (bazelrun.UserRC{OS: "windows", RepositoryCache: `C:\e2e\repository-cache`, OutputUserRoot: `C:\Bazel Root`, VC: `C:\Program Files\Microsoft Visual Studio\2026\BuildTools\VC`, VCFullVersion: "14.50.35717", WinSDKFullVersion: "10.0.26100.0"}).Render()
	require.Contains(t, rc, "common --repository_cache=C:/e2e/repository-cache\n")
	require.Contains(t, rc, "startup '--output_user_root=C:/Bazel Root'\n")
	require.Contains(t, rc, "common '--repo_env=BAZEL_VC=C:/Program Files/Microsoft Visual Studio/2026/BuildTools/VC'\n")
	require.NotContains(t, rc, `\`, "Windows rc paths must not rely on backslash tokenizer behavior")
	require.NotContains(t, rc, "--action_env=TMP")
	require.NotContains(t, rc, "--action_env=TEMP")
	require.Equal(t, 1, strings.Count(rc, "--repository_cache="))
}
