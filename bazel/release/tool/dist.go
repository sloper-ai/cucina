// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// A dist directory holds a set of release assets under their published names (assets/) and
// metadata that is not published as an asset (meta/: buildinfo.json, the Homebrew formula).
const (
	assetsDir     = "assets"
	metaDir       = "meta"
	buildInfoName = "buildinfo.json"
	sumsName      = "SHA256SUMS"
	formulaName   = "cucinactl.rb"
	imagesName    = "images.json" // image name -> OCI index digest (what the publish job pushes)
)

// Platform is an os-arch pair as used in asset names.
type Platform struct{ OS, Arch string }

func (p Platform) String() string { return p.OS + "-" + p.Arch }

// Release asset names (ADR 0151). The macOS package names follow ADR 0753.
var (
	cliPlatforms   = []Platform{{"darwin", "arm64"}, {"linux", "amd64"}, {"linux", "arm64"}, {"windows", "amd64"}}
	agentPlatforms = cliPlatforms
	// Formula platforms: Homebrew on macOS (Apple silicon) and Linux.
	formulaPlatforms = []Platform{{"darwin", "arm64"}, {"linux", "amd64"}, {"linux", "arm64"}}
)

// CLIArchiveName is the cucinactl archive of a platform.
func CLIArchiveName(version string, p Platform) string {
	if p.OS == "windows" {
		return fmt.Sprintf("cucinactl-%s-%s.zip", version, p)
	}
	return fmt.Sprintf("cucinactl-%s-%s.tar.gz", version, p)
}

// AgentName is the cucina-worker-agent binary of a platform.
func AgentName(version string, p Platform) string {
	if p.OS == "windows" {
		return fmt.Sprintf("cucina-worker-agent-%s-%s.exe", version, p)
	}
	return fmt.Sprintf("cucina-worker-agent-%s-%s", version, p)
}

// HostdName is the cucina-hostd binary (darwin/arm64 only).
func HostdName(version string) string { return "cucina-hostd-" + version + "-darwin-arm64" }

// ChartName is the Helm chart package.
func ChartName(version string) string { return "cucina-" + version + ".tgz" }

// PkgBase is the base name of the macOS host package and its manifests (ADR 0753).
func PkgBase(core string) string { return "cucina-host-" + strings.ReplaceAll(core, ".", "-") }

// SHA256SUMS ------------------------------------------------------------------------------

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// WriteSums returns `sha256sum`-compatible lines ("<hex>  <name>") for the given files.
func WriteSums(dir string, names []string) ([]byte, error) {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	var b bytes.Buffer
	for _, n := range sorted {
		sum, err := sha256File(filepath.Join(dir, n))
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "%s  %s\n", sum, n)
	}
	return b.Bytes(), nil
}

var sumLineRE = regexp.MustCompile(`^([0-9a-f]{64})  ([^/\\]+)$`)

// ParseSums parses SHA256SUMS lines into name -> hex digest.
func ParseSums(data []byte) (map[string]string, error) {
	out := map[string]string{}
	for i, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		m := sumLineRE.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("%s line %d: malformed %q", sumsName, i+1, line)
		}
		if _, dup := out[m[2]]; dup {
			return nil, fmt.Errorf("%s: %s listed twice", sumsName, m[2])
		}
		out[m[2]] = m[1]
	}
	return out, nil
}

// Homebrew formula --------------------------------------------------------------------------

// RenderFormula fills the formula template with the version and per-platform URL/SHA-256
// (R-OPS-7 / R-CLI-1: the tap formula sloper-ai/homebrew-tap/Formula/cucinactl.rb).
func RenderFormula(template string, bi BuildInfo, sums map[string]string) (string, error) {
	repl := []string{"{{VERSION}}", bi.Version, "{{REPOSITORY}}", bi.Repository}
	for _, p := range formulaPlatforms {
		name := CLIArchiveName(bi.Version, p)
		sum, ok := sums[name]
		if !ok {
			return "", fmt.Errorf("formula: missing %s", name)
		}
		key := strings.ToUpper(strings.ReplaceAll(p.String(), "-", "_"))
		repl = append(repl, "{{URL_"+key+"}}", bi.ReleaseURL(name), "{{SHA256_"+key+"}}", sum)
	}
	out := strings.NewReplacer(repl...).Replace(template)
	if i := strings.Index(out, "{{"); i >= 0 {
		end := min(len(out), i+40)
		return "", fmt.Errorf("formula: unknown placeholder near %q", out[i:end])
	}
	return out, nil
}

// Copying -------------------------------------------------------------------------------------

func copyFile(src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, mode); err != nil {
		return err
	}
	return os.Chmod(dst, mode) // WriteFile keeps the mode of an existing file
}

func execMode(path string) (os.FileMode, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if st.Mode()&0o111 != 0 {
		return 0o755, nil
	}
	return 0o644, nil
}

// listFiles returns the regular files directly inside dir.
func listFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			return nil, fmt.Errorf("%s: unexpected directory %s", dir, e.Name())
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// mergeFile copies src to dst, accepting an existing identical file (same asset from two
// builds) and rejecting a different one.
func mergeFile(src, dst string) error {
	mode, err := execMode(src)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		a, errA := sha256File(src)
		b, errB := sha256File(dst)
		if err := errors.Join(errA, errB); err != nil {
			return err
		}
		if a != b {
			return fmt.Errorf("%s differs between inputs", filepath.Base(dst))
		}
		return nil
	}
	return copyFile(src, dst, mode)
}

func runDist(args []string, _ io.Writer) error {
	fs := newFlags("dist")
	buildinfo := fs.String("buildinfo", "", "build info JSON")
	out := fs.String("out", "", "output dist directory")
	var files, trees, metas, images multiFlag
	fs.Var(&files, "file", "TEMPLATE=PATH: an asset under its published name (repeated)")
	fs.Var(&images, "image", "NAME=LAYOUT: record the index digest of an OCI layout in meta/images.json (repeated)")
	fs.Var(&trees, "tree", "directory whose files are assets under their own names (repeated)")
	fs.Var(&metas, "meta", "NAME=PATH: a metadata file (repeated)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := required(fs, "buildinfo", "out"); err != nil {
		return err
	}
	bi, err := LoadBuildInfo(*buildinfo)
	if err != nil {
		return err
	}
	fileKV, err := pairs(files)
	if err != nil {
		return err
	}
	metaKV, err := pairs(metas)
	if err != nil {
		return err
	}
	assets := filepath.Join(*out, assetsDir)
	for _, d := range []string{assets, filepath.Join(*out, metaDir)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	for _, kv := range fileKV {
		name := bi.Expand(kv[0])
		if strings.ContainsAny(name, "/\\{}") {
			return fmt.Errorf("bad asset name %q", name)
		}
		if err := mergeFile(kv[1], filepath.Join(assets, name)); err != nil {
			return err
		}
	}
	for _, dir := range trees {
		names, err := listFiles(dir)
		if err != nil {
			return err
		}
		for _, n := range names {
			if err := mergeFile(filepath.Join(dir, n), filepath.Join(assets, n)); err != nil {
				return err
			}
		}
	}
	for _, kv := range metaKV {
		if err := mergeFile(kv[1], filepath.Join(*out, metaDir, kv[0])); err != nil {
			return err
		}
	}
	imageKV, err := pairs(images)
	if err != nil {
		return err
	}
	if len(imageKV) > 0 {
		digests := map[string]string{}
		for _, kv := range imageKV {
			if digests[kv[0]], err = OCIIndexDigest(kv[1]); err != nil {
				return err
			}
		}
		if err := writeJSON(filepath.Join(*out, metaDir, imagesName), digests); err != nil {
			return err
		}
	}
	return writeJSON(filepath.Join(*out, metaDir, buildInfoName), bi)
}

func runFinalize(args []string, _ io.Writer) error {
	fs := newFlags("finalize")
	out := fs.String("out", "", "output release directory")
	formula := fs.String("formula-template", "", "Homebrew formula template (release/homebrew/cucinactl.rb.tmpl)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := required(fs, "out"); err != nil {
		return err
	}
	ins := fs.Args()
	if len(ins) == 0 {
		return errors.New("no input dist directories")
	}
	for _, sub := range []string{assetsDir, metaDir} {
		if err := os.MkdirAll(filepath.Join(*out, sub), 0o755); err != nil {
			return err
		}
	}
	var bi BuildInfo
	for i, in := range ins {
		b, err := LoadBuildInfo(filepath.Join(in, metaDir, buildInfoName))
		if err != nil {
			return err
		}
		if i > 0 && b != bi {
			return fmt.Errorf("%s was built as %+v, %s as %+v: refusing to mix builds", ins[0], bi, in, b)
		}
		bi = b
		for _, sub := range []string{assetsDir, metaDir} {
			names, err := listFiles(filepath.Join(in, sub))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			for _, n := range names {
				if n == sumsName || (sub == metaDir && n == buildInfoName) {
					continue
				}
				if err := mergeFile(filepath.Join(in, sub, n), filepath.Join(*out, sub, n)); err != nil {
					return err
				}
			}
		}
	}
	assets := filepath.Join(*out, assetsDir)
	names, err := listFiles(assets)
	if err != nil {
		return err
	}
	sums, err := WriteSums(assets, names)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(assets, sumsName), sums, 0o644); err != nil {
		return err
	}
	if *formula != "" {
		tmpl, err := os.ReadFile(*formula)
		if err != nil {
			return err
		}
		parsed, err := ParseSums(sums)
		if err != nil {
			return err
		}
		rb, err := RenderFormula(string(tmpl), bi, parsed)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(*out, metaDir), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(*out, metaDir, formulaName), []byte(rb), 0o644); err != nil {
			return err
		}
	}
	return writeJSON(filepath.Join(*out, metaDir, buildInfoName), bi)
}
