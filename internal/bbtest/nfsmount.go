// SPDX-License-Identifier: FSL-1.1-ALv2

package bbtest

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/sloper-ai/cucina/internal/ports"
)

const mountCleanupTimeout = 5 * time.Second

// NFSMount tracks the build-directory mount owned by one test worker.
// Construct it before starting the worker; clean it up after stopping it.
type NFSMount struct {
	exec ports.Exec
	path string
}

// TrackNFSMount prepares cleanup for a fresh, test-owned mount directory.
// It resolves symlinks BEFORE the worker mounts it: macOS's mount table uses
// /private/var where TempDir may use /var, and resolving a dead NFS root can
// hang. A preexisting mount is never claimed or removed by this helper.
func TrackNFSMount(ctx context.Context, exec ports.Exec, path string) (*NFSMount, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("NFS mount path must be absolute: %q", path)
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("resolve NFS mount directory before worker start: %w", err)
	}
	if canonical == filepath.Dir(canonical) {
		return nil, fmt.Errorf("NFS mount directory must not be a filesystem root: %q", canonical)
	}
	m := &NFSMount{exec: exec, path: canonical}
	ctx, cancel := context.WithTimeout(ctx, mountCleanupTimeout)
	defer cancel()
	kind, err := m.mountType(ctx)
	if err != nil {
		return nil, err
	}
	if kind != "" {
		return nil, fmt.Errorf("refusing to claim preexisting %s mount at %q", kind, canonical)
	}
	return m, nil
}

// Cleanup removes a remaining NFS mount after its worker has stopped. It only
// reads the mount table, never the possibly stale mount path. The worker has
// already had its graceful-unmount opportunity, so force is needed if its NFS
// server has exited. Commands share a bounded deadline; success means the
// exact mount is gone, not just that umount returned zero.
func (m *NFSMount) Cleanup(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, mountCleanupTimeout)
	defer cancel()
	kind, err := m.mountType(ctx)
	if err != nil || kind == "" {
		return err
	}
	if kind != "nfs" {
		return fmt.Errorf("refusing to unmount unexpected %s filesystem at %q", kind, m.path)
	}
	out, err := m.exec.Run(ctx, ports.Command{Path: "/sbin/umount", Args: []string{"-f", m.path}})
	if err != nil {
		return fmt.Errorf("force unmount test NFS directory %q: %w", m.path, err)
	}
	if out.ExitCode != 0 {
		return fmt.Errorf("force unmount test NFS directory %q: exit %d: %s", m.path, out.ExitCode, out.Stderr)
	}
	kind, err = m.mountType(ctx)
	if err != nil {
		return err
	}
	if kind != "" {
		return fmt.Errorf("test mount %q remains after unmount", m.path)
	}
	return nil
}

func (m *NFSMount) mountType(ctx context.Context) (string, error) {
	out, err := m.exec.Run(ctx, ports.Command{Path: "/sbin/mount"})
	if err != nil {
		return "", fmt.Errorf("read mount table for %q: %w", m.path, err)
	}
	if out.ExitCode != 0 {
		return "", fmt.Errorf("read mount table for %q: exit %d: %s", m.path, out.ExitCode, out.Stderr)
	}
	for line := range strings.SplitSeq(string(out.Stdout), "\n") {
		// macOS: source on /absolute/path (nfs, options...). Use the final
		// option delimiter so spaces and parentheses in the path are safe;
		// suffix matching includes the delimiter, never another path's prefix.
		i := strings.LastIndex(line, " (")
		if i < 0 || !strings.HasSuffix(line[:i], " on "+m.path) || !strings.HasSuffix(line, ")") {
			continue
		}
		kind, _, _ := strings.Cut(line[i+2:len(line)-1], ",")
		return strings.TrimSpace(kind), nil
	}
	return "", nil
}
