// SPDX-License-Identifier: FSL-1.1-ALv2

package bbtest

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Pinned Buildbarn releases (ADR 0001). SHA-256 sums of the darwin_arm64 assets
// are recorded in docs/dev/buildbarn.md; Bazel fetches them as @bb_release.
const (
	RemoteExecutionRelease = "20260930T173749Z-1a3be95" // bb_scheduler, bb_worker, bb_runner
	StorageRelease         = "20260930T153215Z-086b011" // bb_storage
)

// Environment variables naming the binaries.
const (
	EnvStorage   = "BB_STORAGE"
	EnvScheduler = "BB_SCHEDULER"
	EnvWorker    = "BB_WORKER"
	EnvRunner    = "BB_RUNNER"
)

var binaryRelease = map[string]struct{ name, release string }{
	EnvStorage:   {"bb_storage", StorageRelease},
	EnvScheduler: {"bb_scheduler", RemoteExecutionRelease},
	EnvWorker:    {"bb_worker", RemoteExecutionRelease},
	EnvRunner:    {"bb_runner", RemoteExecutionRelease},
}

// Binary returns the path of the pinned binary named by env (EnvStorage, …).
// Without it, `go test` skips the test with instructions; under Bazel
// (TEST_SRCDIR set) a missing binary is a failure, not a skip.
func Binary(t testing.TB, env string) string {
	t.Helper()
	if p := os.Getenv(env); p != "" {
		if r, ok := resolve(p); ok {
			return r
		}
		t.Fatalf("%s=%q does not name an existing file", env, p)
	}
	if b, ok := binaryRelease[env]; ok {
		if dev := os.Getenv("CUCINA_DEV_STORAGE"); dev != "" {
			p := filepath.Join(dev, "bb-release", b.release, b.name+"."+runtime.GOOS+"_"+runtime.GOARCH)
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	if os.Getenv("TEST_SRCDIR") != "" {
		t.Fatalf("%s is not set; the Bazel target must provide the pinned binary (data + env)", env)
	}
	t.Skipf("%s is not set: point it at the pinned Buildbarn binary (docs/dev/buildbarn.md, \"Running the boot tests\")", env)
	return ""
}

// resolve accepts absolute paths, paths relative to the working directory
// (Bazel $(rootpath)) and runfiles-relative paths (Bazel $(rlocationpath)).
func resolve(p string) (string, bool) {
	candidates := []string{p}
	if !filepath.IsAbs(p) {
		for _, root := range []string{os.Getenv("RUNFILES_DIR"), os.Getenv("TEST_SRCDIR")} {
			if root != "" {
				candidates = append(candidates, filepath.Join(root, p))
			}
		}
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			abs, err := filepath.Abs(c)
			if err != nil {
				return "", false
			}
			return abs, true
		}
	}
	return "", false
}
