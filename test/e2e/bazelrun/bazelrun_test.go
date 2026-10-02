// SPDX-License-Identifier: FSL-1.1-ALv2
//go:build !windows

package bazelrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/test/e2e/remote"
)

// fakeBazel stands in for the Bazel client: it writes real Bazel 9.2 fixture
// artifacts to the paths the collector flags name and exits 3 ("tests
// failed"), like `bazel test` would.
const fakeBazel = `#!/bin/sh
for a in "$@"; do
  case "$a" in
    shutdown) exit 0 ;;
    info) echo 4242; exit 0 ;;
    --build_event_binary_file=*) cp "$FIXTURES/bep.bin" "${a#*=}" ;;
    --execution_log_compact_file=*) cp "$FIXTURES/exec.log.zst" "${a#*=}" ;;
    --profile=*) cp "$FIXTURES/profile.json" "${a#*=}" ;;
    --memory_profile=*) echo "heap" > "${a#*=}" ;;
  esac
done
echo "INFO: fake bazel $*" >&2
[ -z "${LEAK:-}" ] || echo "LEAK=$LEAK" >&2
exit 3
`

// Integration tier (localhost): guards §10.4's collection pipeline — the
// collector flags reach Bazel, the artifacts come back over the host
// transport and parse, Bazel's exit code is reported, and Bazel runs with a
// minimal environment (the build event stream records --client_env, so a
// caller's variables must not leak into it).
func TestRunCollectsArtifacts(t *testing.T) {
	fixtures := t.TempDir()
	for src, dst := range map[string]string{
		"../collect/bep/testdata/local-cold.bin":      "bep.bin",
		"../collect/execlog/testdata/local-cold.zst":  "exec.log.zst",
		"../collect/profile/testdata/local-cold.json": "profile.json",
	} {
		b, err := os.ReadFile(src)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(fixtures, dst), b, 0o644))
	}
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "bazel"), []byte(fakeBazel), 0o755))
	t.Setenv("LEAK", "secret-from-the-harness")
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	h := remote.NewLocal("dev-mac", t.TempDir())
	ws := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	inv := Invocation{
		Name: "T1-test", Host: h, Workspace: ws, Command: "test", FreshServer: true, Collect: true,
		Args: []string{"--config=cucina", "--", "//absl/..."}, Env: map[string]string{"FIXTURES": fixtures},
		Poll: time.Millisecond,
	}
	out, err := Run(ctx, inv, filepath.Join(t.TempDir(), "artifacts"))
	require.NoError(t, err)
	require.Equal(t, 3, out.ExitCode)
	require.False(t, out.Succeeded())
	require.Equal(t, "4242", out.ServerPID)
	require.NotNil(t, out.BEP)
	require.Equal(t, 3, out.BEP.Runner("darwin-sandbox"))
	require.NotNil(t, out.ExecLog)
	require.Equal(t, 3, out.ExecLog.LocalExecutions)
	require.NotNil(t, out.Profile)
	require.Len(t, out.Profile.CriticalPath, 3)
	require.Contains(t, out.Log, "--execution_log_compact_file=")
	require.NotContains(t, out.Log, "secret-from-the-harness")
	// Collector flags precede the "--" target separator.
	full := strings.Join(inv.FullArgs(), " ")
	require.Less(t, strings.Index(full, "--profile="), strings.Index(full, " -- //absl/..."))
}
