// SPDX-License-Identifier: FSL-1.1-ALv2

package sys_test

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/hostd/privdrop"
	"github.com/sloper-ai/cucina/internal/hostd/sys"
	"github.com/sloper-ai/cucina/internal/ports"
)

const childArg = "cucina-sys-test-child"

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == childArg {
		_, _ = fmt.Fprint(os.Stdout, "child executed")
		os.Exit(23)
	}
	os.Exit(m.Run())
}

// Guards: R-BUILD-6 / Windows CI 37061500348 — portable FS consumers obtain
// real volume capacity; unsupported syscalls must not become fake zero metrics.
func TestDiskUsagePlatform(t *testing.T) {
	f := sys.FS{}
	root := t.TempDir()
	_, free, err := f.DiskUsage(root)
	require.NoError(t, err)
	require.Positive(t, free)
	_, _, err = f.DiskUsage(filepath.Join(root, "missing"))
	require.ErrorIs(t, err, fs.ErrNotExist)
}

// Guards: R-MAC-2 / R-BUILD-6 — the concrete hostd process adapter keeps its
// Unix execution semantics; Windows cannot silently ignore identity/detachment.
func TestExecPlatformBoundary(t *testing.T) {
	program, err := os.Executable()
	require.NoError(t, err)
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		require.ErrorIs(t, privdrop.DropAndExec(privdrop.TrampolineArgs{
			UID: 600, GID: 20, Argv: []string{program, childArg},
		}), errors.ErrUnsupported)
	}
	for _, start := range []bool{false, true} {
		t.Run(fmt.Sprintf("start=%t", start), func(t *testing.T) {
			e := &sys.Exec{}
			command := ports.Command{Path: program, Args: []string{childArg}, Env: os.Environ()}
			if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
				for _, mode := range []privdrop.Mode{privdrop.ModeNone, privdrop.ModeAsUser, privdrop.ModeSetuid} {
					e.Mode = mode
					for _, identity := range []*ports.RunAs{nil, {User: "worker", UID: 600, GID: 20}} {
						command.RunAs = identity
						if start {
							process, err := e.Start(t.Context(), command)
							require.ErrorIs(t, err, errors.ErrUnsupported)
							require.Nil(t, process)
						} else {
							_, err := e.Run(t.Context(), command)
							require.ErrorIs(t, err, errors.ErrUnsupported)
						}
					}
				}
				return
			}
			var result ports.ExecResult
			if start {
				process, err := e.Start(t.Context(), command)
				require.NoError(t, err)
				result, err = process.Wait(t.Context())
				require.NoError(t, err)
			} else {
				result, err = e.Run(t.Context(), command)
				require.NoError(t, err)
			}
			require.Equal(t, 23, result.ExitCode)
			require.Equal(t, "child executed", string(result.Stdout))
		})
	}
}
