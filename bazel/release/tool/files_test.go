// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"crypto/sha256"
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

// Guards R-OPS-7 / ADR 0110: base provenance binds the exact manifest bytes,
// whether the builder supplies a legacy OCI layout or a rules_img manifest file.
func TestImageMetadataBindsBaseManifest(t *testing.T) {
	root := t.TempDir()
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a","size":2},"layers":[]}`)
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(manifest))
	manifestFile := filepath.Join(root, "base.json")
	require.NoError(t, os.WriteFile(manifestFile, manifest, 0o644))
	layout := filepath.Join(root, "layout")
	require.NoError(t, os.Mkdir(layout, 0o755))
	index := fmt.Sprintf(`{"schemaVersion":2,"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":%q,"size":%d}]}`, digest, len(manifest))
	require.NoError(t, os.WriteFile(filepath.Join(layout, "index.json"), []byte(index), 0o644))
	buildinfo := filepath.Join(root, "buildinfo.json")
	require.NoError(t, invokeRelease("buildinfo", "--out-json", buildinfo, "--out-env", filepath.Join(root, "buildinfo.env")))
	for _, tc := range []struct{ flag, path string }{
		{"--base-layout", layout},
		{"--base-manifest", manifestFile},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			out := t.TempDir()
			labels := filepath.Join(out, "labels.txt")
			require.NoError(t, invokeRelease("oci-meta", "--buildinfo", buildinfo, "--title", "controller",
				"--description", "Cucina controller", "--base-name", "example.invalid/base", tc.flag, tc.path,
				"--out-labels", labels, "--out-created", filepath.Join(out, "created.txt"), "--out-tags", filepath.Join(out, "tags.txt")))
			data, err := os.ReadFile(labels)
			require.NoError(t, err)
			require.Contains(t, string(data), "org.opencontainers.image.base.digest="+digest+"\n")
		})
	}
}

// Guards R-BUILD-3 / ADR 0110: the exported directory retains the historical
// single-index envelope without changing the rules_img index or payload bytes.
func TestImageLayoutPreservesIndexDigest(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "flat")
	// Bazel's sandbox presents input tree files as symlinks to read-only outputs.
	inputFile := func(path string, data []byte) {
		actual := filepath.Join(root, "source", path)
		require.NoError(t, os.MkdirAll(filepath.Dir(actual), 0o755))
		require.NoError(t, os.WriteFile(actual, data, 0o444))
		link := filepath.Join(input, path)
		require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o755))
		require.NoError(t, os.Symlink(actual, link))
	}
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[]}`)
	manifestHash := fmt.Sprintf("%x", sha256.Sum256(manifest))
	inputFile(filepath.Join("blobs", "sha256", manifestHash), manifest)
	index := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:%s","size":%d}]}`, manifestHash, len(manifest)))
	inputFile("index.json", index)
	layoutVersion := []byte(`{"imageLayoutVersion":"1.0.0"}`)
	inputFile("oci-layout", layoutVersion)
	output := filepath.Join(root, "wrapped")
	require.NoError(t, invokeRelease("wrap-oci", "--layout", input, "--out", output))
	got, err := OCIIndexDigest(output)
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("sha256:%x", sha256.Sum256(index)), got)
	for _, tc := range []struct {
		path string
		want []byte
	}{
		{filepath.Join("blobs", "sha256", strings.TrimPrefix(got, "sha256:")), index},
		{filepath.Join("blobs", "sha256", manifestHash), manifest},
		{"oci-layout", layoutVersion},
	} {
		data, err := os.ReadFile(filepath.Join(output, tc.path))
		require.NoError(t, err)
		require.Equal(t, tc.want, data, tc.path)
		info, err := os.Lstat(filepath.Join(output, tc.path))
		require.NoError(t, err)
		require.Zero(t, info.Mode()&os.ModeSymlink, "export must not depend on the input tree")
	}
	original, err := os.ReadFile(filepath.Join(input, "index.json"))
	require.NoError(t, err)
	require.Equal(t, index, original, "export must not modify immutable input blobs")
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
