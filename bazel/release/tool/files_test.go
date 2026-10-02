// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Exercise the executable's public CLI, not the private command dispatcher. Bazel provides
// the built binary; go test uses go run with the same sources for IDE-mode development.
func invokeRelease(args ...string) error {
	bin := os.Getenv("CUCINA_RELEASE_BIN")
	if bin == "" {
		bin = "go"
		args = append([]string{"run", "."}, args...)
	}
	output, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("release command: %w: %s", err, output)
	}
	return nil
}

// R-OPS-7 / ADR 0150: a dirty source snapshot must never masquerade as the clean tagged
// commit, and this fact must survive the buildinfo JSON used by all packaging actions.
func TestBuildInfoRecordsDirtySources(t *testing.T) {
	root := t.TempDir()
	status := filepath.Join(root, "status")
	out := filepath.Join(root, "buildinfo.json")
	env := filepath.Join(root, "buildinfo.env")
	require.NoError(t, os.WriteFile(status, []byte("STABLE_CUCINA_VERSION 0.0.0-dryrun\n"+
		"STABLE_CUCINA_COMMIT abc1234\nSTABLE_CUCINA_DIRTY true\n"), 0o644))
	require.NoError(t, invokeRelease("buildinfo", "--stable-status", status, "--out-json", out, "--out-env", env))
	data, err := os.ReadFile(out)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(data, &fields))
	require.Equal(t, true, fields["dirty"])
	bi, err := LoadBuildInfo(out)
	require.NoError(t, err)
	require.Contains(t, bi.Env(), "DIRTY='true'")
}

// R-OPS-7 / R-CLI-1: the tap formula built from release/homebrew/cucinactl.rb.tmpl points every
// Homebrew platform at its own archive and checksum.
func TestRenderFormula(t *testing.T) {
	path := os.Getenv("FORMULA_TEMPLATE") // Bazel; `go test` runs in the package directory
	if path == "" {
		path = "../../../release/homebrew/cucinactl.rb.tmpl"
	}
	tmpl, err := os.ReadFile(path)
	require.NoError(t, err)
	bi, err := NewBuildInfo("0.3.1", "c0ffee1", 0, "sloper-ai/cucina", true)
	require.NoError(t, err)
	sums := map[string]string{}
	for i, p := range formulaPlatforms {
		sums[CLIArchiveName(bi.Version, p)] = strings.Repeat(string(rune('a'+i)), 64)
	}
	rb, err := RenderFormula(string(tmpl), bi, sums)
	require.NoError(t, err)
	require.Contains(t, rb, `version "0.3.1"`)
	for _, p := range formulaPlatforms {
		name := CLIArchiveName(bi.Version, p)
		require.Contains(t, rb, `url "https://github.com/sloper-ai/cucina/releases/download/v0.3.1/`+name+`"`)
		require.Regexp(t, `url "[^"]+/`+name+`"\s+sha256 "`+sums[name]+`"`, rb)
	}
	delete(sums, CLIArchiveName(bi.Version, formulaPlatforms[2]))
	_, err = RenderFormula(string(tmpl), bi, sums)
	require.Error(t, err)
}

// R-OPS-7: artifacts built on different runners are only combined when they come from the same
// stamped build, and SHA256SUMS covers every asset in `sha256sum -c` format.
func TestFinalizeMergesOneBuild(t *testing.T) {
	root := t.TempDir()
	mkdist := func(name, version, asset string) string {
		dir := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, assetsDir), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, assetsDir, asset), []byte(asset), 0o644))
		bi, err := NewBuildInfo(version, "c0ffee1", 1790000000, "", true)
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, metaDir), 0o755))
		require.NoError(t, writeJSON(filepath.Join(dir, metaDir, buildInfoName), bi))
		return dir
	}
	a := mkdist("macos", "0.1.0", "a.tar.gz")
	b := mkdist("linux", "0.1.0", "b.zip")
	out := filepath.Join(root, "out")
	require.NoError(t, invokeRelease("finalize", "--out", out, a, b))
	sums, err := os.ReadFile(filepath.Join(out, assetsDir, sumsName))
	require.NoError(t, err)
	parsed, err := ParseSums(sums)
	require.NoError(t, err)
	require.Len(t, parsed, 2)
	for _, name := range []string{"a.tar.gz", "b.zip"} {
		got, err := sha256File(filepath.Join(out, assetsDir, name))
		require.NoError(t, err)
		require.Equal(t, got, parsed[name], name)
	}
	require.Contains(t, string(sums), parsed["b.zip"]+"  b.zip\n")

	c := mkdist("other", "0.1.1", "c.zip")
	err = invokeRelease("finalize", "--out", filepath.Join(root, "out2"), a, c)
	require.ErrorContains(t, err, "refusing to mix builds")
}
