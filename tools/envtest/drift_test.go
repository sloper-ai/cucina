// SPDX-License-Identifier: FSL-1.1-ALv2

package envtest_test

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Guards ADR 0112 / R-BUILD-1: Go and Bazel use exactly the checksum-pinned,
// reproducibly relocated upstream closure and its reviewed lifecycle patch.
func TestGeneratedEnvtestIsCurrent(t *testing.T) {
	archive := os.Getenv("ENVTEST_SOURCE_ARCHIVE")
	if archive == "" {
		cache, err := exec.Command("go", "env", "GOMODCACHE").Output()
		if err != nil {
			t.Fatal(err)
		}
		archive = filepath.Join(strings.TrimSpace(string(cache)), "cache", "download", "sigs.k8s.io", "controller-runtime", "@v", "v0.24.1.zip")
	}
	manifest := os.Getenv("ENVTEST_SOURCE_MANIFEST")
	if manifest == "" {
		manifest = "upstream.json"
	}
	patches := os.Getenv("ENVTEST_SOURCE_PATCHES")
	if patches == "" {
		patches = "lifecycle.json"
	}
	generated := os.Getenv("ENVTEST_GENERATED_FILE")
	if generated == "" {
		generated = "../../internal/envtest/server.go"
	}
	// Bazel supplies the local checkout directly; neither a runfile copy nor a
	// Windows manifest entry identifies the complete source-tree inventory.
	source := os.Getenv("ENVTEST_SOURCE_ROOT")
	if source == "" {
		source = filepath.Dir(generated)
	}
	verify := func(out string) ([]byte, error) {
		args := []string{"--check", "--archive", archive, "--manifest", manifest, "--patches", patches, "--out", out}
		binary := os.Getenv("ENVTEST_SYNC_BIN")
		if binary == "" {
			binary = "go"
			args = append([]string{"run", "./cmd/sync"}, args...)
		}
		return exec.Command(binary, args...).CombinedOutput()
	}
	output, err := verify(source)
	if err != nil {
		t.Fatalf("generated envtest drift: %v\n%s", err, output)
	}
	// The inventory, not just known file contents, is part of the public gate:
	// native Go compiles extra sources that an explicit Bazel srcs list omits.
	for _, extra := range []string{"obsolete.go", "internal/newpackage/obsolete_windows.go"} {
		t.Run(extra, func(t *testing.T) {
			copyRoot := t.TempDir()
			if err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				rel, err := filepath.Rel(source, path)
				if err != nil {
					return err
				}
				dest := filepath.Join(copyRoot, rel)
				if entry.IsDir() {
					return os.MkdirAll(dest, 0o755)
				}
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				return os.WriteFile(dest, data, 0o644)
			}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(copyRoot, extra)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("package obsolete\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if output, err := verify(copyRoot); err == nil {
				t.Fatalf("generator accepted undeclared source %s: %s", extra, output)
			}
		})
	}
}
