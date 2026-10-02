// SPDX-License-Identifier: FSL-1.1-ALv2

package guest_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/hostd/guest"
)

// Guards: R-SEC-5 — repeated guest activation must not let build actions replace
// a privileged daemon's log or make provisioning follow a planted link.
func TestActivationProtectsDaemonLogs(t *testing.T) {
	builder := guest.Console{User: "builder", UID: 600, GID: 20}
	for _, tc := range []struct {
		name   string
		worker guest.Console
		attack string
	}{
		{name: "root worker", worker: guest.Console{User: "root"}},
		{name: "unprivileged worker", worker: builder},
		{name: "log directory symlink", attack: "directory-symlink"},
		{name: "worker log symlink", attack: "worker-symlink"},
		{name: "runner log symlink", attack: "runner-symlink"},
		{name: "worker log hardlink", attack: "worker-hardlink"},
		{name: "runner log hardlink", attack: "runner-hardlink"},
		{name: "non-file worker log", attack: "worker-directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			logs := filepath.Join(root, "logs")
			outside := filepath.Join(root, "outside")
			require.NoError(t, os.Mkdir(outside, 0o755))
			target := filepath.Join(outside, "protected")
			require.NoError(t, os.WriteFile(target, []byte("must not change\n"), 0o600))
			if tc.attack == "directory-symlink" {
				require.NoError(t, os.Symlink(outside, logs))
			} else {
				require.NoError(t, os.Mkdir(logs, 0o777))
				// Model the pre-fix image's action-writable log parent.
				require.NoError(t, os.Chmod(logs, 0o777))
				require.NoError(t, os.WriteFile(logs+".owner", []byte("600:20\n"), 0o600))
				require.NoError(t, os.WriteFile(logs+".acl", []byte("builder:add_file\n"), 0o600))
				name := "bb_worker.log"
				if strings.HasPrefix(tc.attack, "runner-") {
					name = "bb_runner.log"
				}
				path := filepath.Join(logs, name)
				switch {
				case strings.HasSuffix(tc.attack, "-symlink"):
					require.NoError(t, os.Symlink(target, path))
				case strings.HasSuffix(tc.attack, "-hardlink"):
					require.NoError(t, os.Link(target, path))
				case tc.attack == "worker-directory":
					require.NoError(t, os.Mkdir(path, 0o755))
				}
			}
			cmd, err := guest.ActivateCmd(builder, tc.worker, nil)
			require.NoError(t, err)
			// Run the public provisioning command without sudo or touching host
			// paths. Only ownership, launchd and Spotlight are faked; shell file
			// operations (including symlinks/hardlinks) run against a private tree.
			require.Equal(t, []string{"-n", "--", "/bin/sh", "-c"}, cmd.Args[:4])
			script := activationTools + strings.ReplaceAll(cmd.Args[4], "/var/log/cucina", logs)
			run := func() error {
				sh := exec.CommandContext(t.Context(), "/bin/sh", "-c", script)
				sh.Env = append(os.Environ(), "TEST_HOST_OS="+runtime.GOOS)
				out, runErr := sh.CombinedOutput()
				if runErr != nil {
					t.Logf("activation: %s", out)
				}
				return runErr
			}
			err = run()
			if tc.attack != "" {
				require.Error(t, err, "activation must refuse unsafe log paths before launchd opens them")
				b, readErr := os.ReadFile(target)
				require.NoError(t, readErr)
				require.Equal(t, "must not change\n", string(b))
				st, statErr := os.Stat(target)
				require.NoError(t, statErr)
				require.EqualValues(t, 0o600, st.Mode().Perm())
				return
			}
			require.NoError(t, err)
			assertLogOwnership(t, logs, "0:0", 0o755)
			for name, owner := range map[string]string{
				"bb_worker.log": strconv.Itoa(tc.worker.UID) + ":" + strconv.Itoa(tc.worker.GID),
				"bb_runner.log": "600:20",
			} {
				path := filepath.Join(logs, name)
				assertLogOwnership(t, path, owner, 0o644)
				require.NoError(t, os.WriteFile(path, []byte("existing log\n"), 0o644))
				require.NoError(t, os.WriteFile(path+".acl", []byte("builder:write\n"), 0o600))
			}
			require.NoError(t, run(), "activation is idempotent and must preserve logs")
			assertLogOwnership(t, logs, "0:0", 0o755)
			for _, name := range []string{"bb_worker.log", "bb_runner.log"} {
				require.NoFileExists(t, filepath.Join(logs, name)+".acl", "a legacy ACL must not preserve action write access")
				b, readErr := os.ReadFile(filepath.Join(logs, name))
				require.NoError(t, readErr)
				require.Equal(t, "existing log\n", string(b))
			}
		})
	}
}

func assertLogOwnership(t *testing.T, path, owner string, mode os.FileMode) {
	t.Helper()
	b, err := os.ReadFile(path + ".owner")
	require.NoError(t, err)
	require.Equal(t, owner+"\n", string(b))
	require.NoFileExists(t, path+".acl", "mode bits alone do not revoke an action-writable macOS ACL")
	st, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, mode, st.Mode().Perm())
}

// This filesystem-backed fake models uid/gid and ACL metadata: actual chown to
// root would need privileges. File/link operations and mode changes are real;
// Darwin also executes the real ACL removal. It does not interpret the script.
const activationTools = `
mdutil() { :; }
launchctl() { :; }
chown() { printf '%s\n' "$1" > "$2.owner"; }
chmod() {
  if [ "$1" = -N ]; then
    rm -f "$2.acl"
    if [ "$TEST_HOST_OS" = darwin ]; then command chmod "$@"; fi
  else
    command chmod "$@"
  fi
}
install() {
  o=0; g=0; m=0755
  while [ "$#" -gt 1 ]; do
    case "$1" in
      -d) shift;;
      -o) o=$2; shift 2;;
      -g) g=$2; shift 2;;
      -m) m=$2; shift 2;;
      *) return 1;;
    esac
  done
  mkdir -p "$1" && chown "$o:$g" "$1" && chmod "$m" "$1"
}
stat() {
  if [ "$TEST_HOST_OS" = linux ] && [ "$1" = -f ] && [ "$2" = %l ]; then
    command stat -c %h "$3"
  else
    command stat "$@"
  fi
}
`
