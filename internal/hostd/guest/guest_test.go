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
	shell := fixtureShell(t)
	for _, tc := range []struct {
		name       string
		worker     guest.Console
		attack     string
		modelModes bool
	}{
		{name: "root worker", worker: guest.Console{User: "root"}},
		{name: "modeled guest permissions", worker: guest.Console{User: "root"}, modelModes: true},
		{name: "unprivileged worker", worker: builder},
		{name: "log directory symlink", attack: "directory-symlink"},
		{name: "worker log symlink", attack: "worker-symlink"},
		{name: "runner log symlink", attack: "runner-symlink"},
		{name: "worker log hardlink", attack: "worker-hardlink"},
		{name: "runner log hardlink", attack: "runner-hardlink"},
		{name: "non-file worker log", attack: "worker-directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			modelModes := tc.modelModes || runtime.GOOS == "windows"
			root := filepath.Join(t.TempDir(), "guest root")
			require.NoError(t, os.Mkdir(root, 0o755))
			logs := filepath.Join(root, "logs")
			outside := filepath.Join(root, "outside")
			require.NoError(t, os.Mkdir(outside, 0o755))
			target := filepath.Join(outside, "protected")
			require.NoError(t, os.WriteFile(target, []byte("must not change\n"), 0o600))
			require.NoError(t, os.WriteFile(target+".mode", []byte("0600\n"), 0o600))
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
			// Execute the real guest script without sudo. POSIX ownership/ACLs
			// are modeled; on Windows so are mode bits, which NTFS cannot
			// represent. Links/content remain actual filesystem operations.
			require.Equal(t, []string{"-n", "--", "/bin/sh", "-c"}, cmd.Args[:4])
			// Host temporary paths can contain Windows separators or spaces;
			// neither belongs in the guest's POSIX command syntax.
			script := activationTools + strings.ReplaceAll(cmd.Args[4], "/var/log/cucina", "./logs")
			run := func() error {
				sh := exec.CommandContext(t.Context(), shell, "-c", script)
				sh.Dir = root
				sh.Env = append(os.Environ(), "TEST_HOST_OS="+runtime.GOOS, "TEST_MODEL_MODES="+strconv.FormatBool(modelModes))
				if runtime.GOOS == "windows" {
					bin := filepath.Dir(shell)
					sh.Env = append(sh.Env, "PATH="+strings.Join([]string{bin, filepath.Join(bin, "..", "usr", "bin"), os.Getenv("PATH")}, string(os.PathListSeparator)))
				}
				out, runErr := sh.CombinedOutput()
				if runErr != nil {
					var exit *exec.ExitError
					require.ErrorAs(t, runErr, &exit, "fixture shell must actually execute; spawn errors are not security rejections")
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
				assertGuestMode(t, target, 0o600, modelModes)
				return
			}
			require.NoError(t, err)
			assertLogOwnership(t, logs, "0:0", 0o755, modelModes)
			for name, owner := range map[string]string{
				"bb_worker.log": strconv.Itoa(tc.worker.UID) + ":" + strconv.Itoa(tc.worker.GID),
				"bb_runner.log": "600:20",
			} {
				path := filepath.Join(logs, name)
				assertLogOwnership(t, path, owner, 0o644, modelModes)
				require.NoError(t, os.WriteFile(path, []byte("existing log\n"), 0o644))
				require.NoError(t, os.WriteFile(path+".acl", []byte("builder:write\n"), 0o600))
			}
			require.NoError(t, run(), "activation is idempotent and must preserve logs")
			assertLogOwnership(t, logs, "0:0", 0o755, modelModes)
			for _, name := range []string{"bb_worker.log", "bb_runner.log"} {
				require.NoFileExists(t, filepath.Join(logs, name)+".acl", "a legacy ACL must not preserve action write access")
				b, readErr := os.ReadFile(filepath.Join(logs, name))
				require.NoError(t, readErr)
				require.Equal(t, "existing log\n", string(b))
			}
		})
	}
}

func assertLogOwnership(t *testing.T, path, owner string, mode os.FileMode, modelModes bool) {
	t.Helper()
	b, err := os.ReadFile(path + ".owner")
	require.NoError(t, err)
	require.Equal(t, owner+"\n", string(b))
	require.NoFileExists(t, path+".acl", "mode bits alone do not revoke an action-writable macOS ACL")
	assertGuestMode(t, path, mode, modelModes)
}

func assertGuestMode(t *testing.T, path string, mode os.FileMode, modeled bool) {
	t.Helper()
	if modeled {
		b, err := os.ReadFile(path + ".mode")
		require.NoError(t, err)
		value, err := strconv.ParseUint(strings.TrimSpace(string(b)), 8, 32)
		require.NoError(t, err)
		require.Equal(t, mode, os.FileMode(value))
		return
	}
	st, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, mode, st.Mode().Perm())
}

// fixtureShell locates the host interpreter of the guest POSIX script. A
// missing shell is a fixture error, never an accepted security rejection.
// On Windows, Git's native MSYS shell is used; WSL bash is not a substitute.
func fixtureShell(t *testing.T) string {
	t.Helper()
	if configured := os.Getenv("BAZEL_SH"); configured != "" {
		shell, err := exec.LookPath(configured)
		require.NoError(t, err, "configured POSIX fixture shell is unavailable")
		if runtime.GOOS == "windows" {
			name := strings.ToLower(filepath.ToSlash(shell))
			wsl := strings.HasSuffix(name, "/wsl.exe") || (strings.HasSuffix(name, "/bash.exe") && (strings.Contains(name, "/system32/") || strings.Contains(name, "/sysnative/")))
			require.False(t, wsl, "BAZEL_SH must name a native POSIX shell, not WSL")
		}
		return shell
	}
	if runtime.GOOS != "windows" {
		shell, err := exec.LookPath("/bin/sh")
		require.NoError(t, err)
		return shell
	}
	git, err := exec.LookPath("git.exe")
	require.NoError(t, err, "guest fixture requires BAZEL_SH or Git for Windows")
	root := filepath.Dir(git)
	for range 3 { // Git/cmd, Git/bin, or Git/mingw64/bin
		candidate := filepath.Join(root, "usr", "bin", "sh.exe")
		if st, err := os.Stat(candidate); err == nil && st.Mode().IsRegular() {
			return candidate
		}
		root = filepath.Dir(root)
	}
	t.Fatal("guest fixture requires native Git-for-Windows sh.exe; set BAZEL_SH (WSL is not supported)")
	return ""
}

// This filesystem-backed fake models uid/gid and ACL metadata. On Windows it
// also records the guest's POSIX mode bits, not NTFS's coarse host modes. The
// same mode model has a table row on POSIX hosts. Real file/link operations and
// script control flow are unchanged; native POSIX modes are still checked on
// Darwin/Linux, and Darwin executes real ACL removal as well.
const activationTools = `
mdutil() { :; }
launchctl() { :; }
chown() { printf '%s\n' "$1" > "$2.owner"; }
chmod() {
  if [ "$1" = -N ]; then
    rm -f "$2.acl"
    if [ "$TEST_HOST_OS" = darwin ]; then command chmod "$@"; fi
  else
    printf '%s\n' "$1" > "$2.mode"
    if [ "$TEST_MODEL_MODES" != true ]; then command chmod "$@"; fi
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
  if { [ "$TEST_HOST_OS" = linux ] || [ "$TEST_HOST_OS" = windows ]; } && [ "$1" = -f ] && [ "$2" = %l ]; then
    command stat -c %h "$3"
  else
    command stat "$@"
  fi
}
`
