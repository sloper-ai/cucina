// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build unix

package bbtest_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/bbtest"
	"github.com/sloper-ai/cucina/internal/fakes"
	"github.com/sloper-ai/cucina/internal/ports"
)

// Guards: CI 37009866843 — a stopped worker's NFS mount is removed by its
// canonical path before TempDir cleanup, without unmounting other tests' mounts
// or hiding a failed/hung cleanup. No real mounts or subprocesses are created.
func TestNFSMountCleanup(t *testing.T) {
	for _, tc := range []struct {
		name         string
		mounted      bool
		preexisting  bool
		listFails    bool
		unmountCode  int
		unmountHangs bool
		staysMounted bool
		otherType    bool
		wantError    bool
	}{
		{name: "stale mount through a symlink", mounted: true},
		{name: "worker already unmounted"},
		{name: "preexisting mount is not ours", mounted: true, preexisting: true, wantError: true},
		{name: "mount table failure is not absence", mounted: true, listFails: true, wantError: true},
		{name: "unmount failure is reported", mounted: true, unmountCode: 1, wantError: true},
		{name: "unmount deadline", mounted: true, unmountHangs: true, wantError: true},
		{name: "successful command must remove mount", mounted: true, staysMounted: true, wantError: true},
		{name: "another filesystem is not ours", mounted: true, otherType: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			real := filepath.Join(root, "real")
			require.NoError(t, os.MkdirAll(filepath.Join(real, "build root", "build"), 0o700))
			alias := filepath.Join(root, "alias")
			require.NoError(t, os.Symlink(real, alias))
			path := filepath.Join(alias, "build root", "build")
			canonical, err := filepath.EvalSymlinks(path)
			require.NoError(t, err)

			synctest.Test(t, func(t *testing.T) {
				exec := fakes.NewExec(fakes.NewClock(time.Time{}), fakes.NewRand(1))
				// Include a path sharing the prefix and a separate test's mount.
				// The cleanup contract permits removing only the exact owned path.
				unrelated := map[string]string{canonical + "-other": "nfs", "/another-test/build": "nfs"}
				mounts := map[string]string{}
				for path, kind := range unrelated {
					mounts[path] = kind
				}
				if tc.preexisting {
					mounts[canonical] = "nfs"
				}
				listFails := false
				exec.Handle("/sbin/mount", func(context.Context, ports.Command) (ports.ExecResult, error) {
					if listFails {
						return ports.ExecResult{ExitCode: 1}, nil
					}
					var lines []string
					for path, kind := range mounts {
						lines = append(lines, fmt.Sprintf("bb_worker:/ on %s (%s, nodev, nosuid)\n", path, kind))
					}
					slices.Sort(lines)
					return ports.ExecResult{Stdout: []byte(strings.Join(lines, ""))}, nil
				})
				exec.Handle("/sbin/umount", func(ctx context.Context, c ports.Command) (ports.ExecResult, error) {
					if tc.unmountHangs {
						<-ctx.Done()
						return ports.ExecResult{}, ctx.Err()
					}
					if len(c.Args) != 2 || c.Args[0] != "-f" {
						return ports.ExecResult{}, errors.New("expected a single forced-unmount target")
					}
					if tc.unmountCode == 0 && !tc.staysMounted {
						delete(mounts, c.Args[1])
					}
					return ports.ExecResult{ExitCode: tc.unmountCode}, nil
				})

				mount, err := bbtest.TrackNFSMount(context.Background(), exec, path)
				if tc.preexisting {
					require.Error(t, err)
					require.Equal(t, "nfs", mounts[canonical])
					return
				}
				require.NoError(t, err)
				// Once mounted, path traversal may hang. Resolving the old alias
				// at teardown is too late; cleanup must retain the pre-mount path.
				require.NoError(t, os.Remove(alias))
				if tc.mounted {
					mounts[canonical] = "nfs"
					if tc.otherType {
						mounts[canonical] = "apfs"
					}
				}
				listFails = tc.listFails
				err = mount.Cleanup(context.Background())
				if tc.wantError {
					require.Error(t, err)
					require.Contains(t, mounts, canonical, "failed cleanup must not claim the mount was removed")
					if tc.unmountHangs {
						require.ErrorIs(t, err, context.DeadlineExceeded)
					}
				} else {
					require.NoError(t, err)
					require.NotContains(t, mounts, canonical, "test-owned NFS mount must not survive cleanup")
					require.NoError(t, mount.Cleanup(context.Background()), "cleanup is idempotent")
				}
				for path, kind := range unrelated {
					require.Equal(t, kind, mounts[path], "unrelated mount changed")
				}
			})
		})
	}
}
