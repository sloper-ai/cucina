// SPDX-License-Identifier: FSL-1.1-ALv2

package porttest

import (
	"context"
	"io/fs"
	"path"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/ports"
)

// ------------------------------------------------------------------- Clock

// ClockHarness is a clock plus the means to move it.
type ClockHarness struct {
	Clock ports.Clock
	// Advance moves time forward by d: Clock.Advance for a manual clock,
	// time.Sleep for a system clock inside a synctest bubble.
	Advance func(d time.Duration)
}

// RunClock checks the ports.Clock contract: Now never goes backwards, After
// fires once its duration elapsed and not before, Sleep honours cancellation.
// With synctestBubble every check runs inside testing/synctest (for clocks
// backed by package time) and newClock is called inside the bubble.
func RunClock(t *testing.T, synctestBubble bool, newClock func(t *testing.T) ClockHarness) {
	run := func(name string, f func(t *testing.T, h ClockHarness)) {
		t.Run(name, func(t *testing.T) {
			if synctestBubble {
				synctest.Test(t, func(t *testing.T) { f(t, newClock(t)) })
				return
			}
			f(t, newClock(t))
		})
	}
	fired := func(_ ClockHarness, ch <-chan time.Time) bool {
		if synctestBubble {
			synctest.Wait()
		}
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}
	run("NowIsMonotonic", func(t *testing.T, h ClockHarness) {
		a := h.Clock.Now()
		h.Advance(time.Second)
		b := h.Clock.Now()
		require.False(t, b.Before(a), "Now went backwards: %s then %s", a, b)
		require.GreaterOrEqual(t, b.Sub(a), time.Second)
	})
	run("AfterFiresOnlyWhenDue", func(t *testing.T, h ClockHarness) {
		ch := h.Clock.After(10 * time.Second)
		h.Advance(9 * time.Second)
		require.False(t, fired(h, ch), "After(10s) fired after 9s")
		h.Advance(time.Second)
		require.True(t, fired(h, ch), "After(10s) did not fire after 10s")
	})
	run("AfterZeroFiresImmediately", func(t *testing.T, h ClockHarness) {
		require.True(t, fired(h, h.Clock.After(0)))
	})
	run("SleepHonoursCancellation", func(t *testing.T, h ClockHarness) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := h.Clock.Sleep(ctx, time.Hour)
		require.ErrorIs(t, err, context.Canceled)
	})
}

// ---------------------------------------------------------------------- FS

// RunFS checks the ports.FS contract below root (an empty directory).
func RunFS(t *testing.T, newFS func(t *testing.T) (ports.FS, string)) {
	t.Run("WriteReadRoundTrip", func(t *testing.T) {
		f, root := newFS(t)
		p := path.Join(root, "a.json")
		require.NoError(t, f.WriteFileAtomic(p, []byte("one"), 0o600))
		require.NoError(t, f.WriteFileAtomic(p, []byte("two"), 0o600))
		got, err := f.ReadFile(p)
		require.NoError(t, err)
		require.Equal(t, "two", string(got))
	})
	t.Run("MissingFileIsNotExist", func(t *testing.T) {
		f, root := newFS(t)
		_, err := f.ReadFile(path.Join(root, "missing"))
		require.ErrorIs(t, err, fs.ErrNotExist)
		ok, err := f.Exists(path.Join(root, "missing"))
		require.NoError(t, err)
		require.False(t, ok)
	})
	t.Run("WriteNeedsParentDirectory", func(t *testing.T) {
		f, root := newFS(t)
		p := path.Join(root, "x", "y", "z.txt")
		require.Error(t, f.WriteFileAtomic(p, []byte("z"), 0o644))
		require.NoError(t, f.MkdirAll(path.Join(root, "x", "y"), 0o755))
		require.NoError(t, f.WriteFileAtomic(p, []byte("z"), 0o644))
	})
	t.Run("RenameAndList", func(t *testing.T) {
		f, root := newFS(t)
		require.NoError(t, f.WriteFileAtomic(path.Join(root, "b"), []byte("b"), 0o644))
		require.NoError(t, f.WriteFileAtomic(path.Join(root, "a"), []byte("a"), 0o644))
		require.NoError(t, f.Rename(path.Join(root, "b"), path.Join(root, "c")))
		names, err := f.ListDir(root)
		require.NoError(t, err)
		require.Equal(t, []string{"a", "c"}, names)
	})
	t.Run("RemoveAndRemoveAll", func(t *testing.T) {
		f, root := newFS(t)
		d := path.Join(root, "d")
		require.NoError(t, f.MkdirAll(path.Join(d, "e"), 0o755))
		require.NoError(t, f.WriteFileAtomic(path.Join(d, "e", "f"), []byte("f"), 0o644))
		require.Error(t, f.Remove(d), "removing a non-empty directory must fail")
		require.NoError(t, f.RemoveAll(d))
		ok, err := f.Exists(d)
		require.NoError(t, err)
		require.False(t, ok)
		require.NoError(t, f.RemoveAll(d), "RemoveAll of a missing path is not an error")
	})
	t.Run("DiskUsageReportsFreeSpace", func(t *testing.T) {
		f, root := newFS(t)
		_, free, err := f.DiskUsage(root)
		require.NoError(t, err)
		require.Positive(t, free)
	})
}

// ------------------------------------------------------------- SecretStore

// RunSecretStore checks the ports.SecretStore contract. Keys are unique per
// run so the suite can run against a shared real keychain.
func RunSecretStore(t *testing.T, newStore func(t *testing.T) ports.SecretStore) {
	ctx := context.Background()
	t.Run("MissingKeyIsNotFound", func(t *testing.T) {
		s := newStore(t)
		_, err := s.Get(ctx, uniq(t, "porttest-missing"))
		require.ErrorIs(t, err, ports.ErrNotFound)
	})
	t.Run("PutGetOverwriteDelete", func(t *testing.T) {
		s := newStore(t)
		k := uniq(t, "porttest-key")
		t.Cleanup(func() { _ = s.Delete(ctx, k) })
		require.NoError(t, s.Put(ctx, k, []byte("v1")))
		require.NoError(t, s.Put(ctx, k, []byte("v2")))
		got, err := s.Get(ctx, k)
		require.NoError(t, err)
		require.Equal(t, "v2", string(got))
		require.NoError(t, s.Delete(ctx, k))
		_, err = s.Get(ctx, k)
		require.ErrorIs(t, err, ports.ErrNotFound, "after Delete")
		require.NoError(t, s.Delete(ctx, k), "deleting a missing key is not an error")
	})
}
