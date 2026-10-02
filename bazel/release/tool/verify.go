// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"bytes"
	"context"
	"debug/macho"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"
)

// Asset groups that a release directory must contain (`verify --expect`).
const (
	groupMacOS = "macos" // cucinactl, cucina-worker-agent and cucina-hostd for darwin/arm64
	groupPkg   = "pkg"   // the macOS host package and its manifests
	groupLinux = "linux" // cucinactl and cucina-worker-agent for Linux/Windows, the Helm chart
)

const (
	pkgID         = "ai.sloper.cucina.host"
	credHelper    = "cucina-credential-helper"
	controllerBin = "/usr/local/bin/cucina-controller"
	docDir        = "/usr/share/doc/cucina"
	nonrootUser   = "65532" // distroless :nonroot
)

var archiveDocs = []string{"LICENSE.md", "THIRD_PARTY_NOTICES.md"}

// pkgCompanions are the files scripts/make-manifest.sh writes next to the package (ADR 0753).
var pkgCompanions = []string{".pkg", ".plist", ".json", ".pkg.sha256", ".install-enterprise-application.plist", ".ddm-package.json"}

type verifier struct {
	dir        string
	bi         BuildInfo
	assets     map[string]bool
	seen       map[string]bool
	oci        map[string]string
	helm       string
	native     bool
	signerSHA1 string
	requireSig bool
	tmp        string
	errs       []error
	notes      []string
}

func (v *verifier) failf(format string, a ...any) { v.errs = append(v.errs, fmt.Errorf(format, a...)) }
func (v *verifier) notef(format string, a ...any) {
	v.notes = append(v.notes, fmt.Sprintf(format, a...))
}

func (v *verifier) asset(name string) string { return filepath.Join(v.dir, assetsDir, name) }

func (v *verifier) read(name string) ([]byte, bool) {
	if !v.assets[name] {
		return nil, false
	}
	v.seen[name] = true
	data, err := os.ReadFile(v.asset(name))
	if err != nil {
		v.failf("%s: %v", name, err)
		return nil, false
	}
	return data, true
}

func hostPlatform() Platform { return Platform{runtime.GOOS, runtime.GOARCH} }

// runNative runs a binary of the host platform and returns its trimmed stdout.
func (v *verifier) runNative(path string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = []string{"HOME=" + v.tmp, "PATH=/usr/bin:/bin", "NO_COLOR=1", "CUCINA_CONFIG_DIR=" + filepath.Join(v.tmp, "config")}
	cmd.Dir = v.tmp
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", filepath.Base(path), strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

func (v *verifier) writeExec(name string, data []byte) (string, error) {
	dir, err := os.MkdirTemp(v.tmp, "bin-")
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, name)
	return p, os.WriteFile(p, data, 0o755)
}

// checkCLIArchive: cucinactl plus the cucina-credential-helper alias (hard link in tar.gz,
// copy in zip; R-AUTH-8), the licence documents, the right binary format and the version.
func (v *verifier) checkCLIArchive(p Platform) {
	name := CLIArchiveName(v.bi.Version, p)
	data, ok := v.read(name)
	if !ok {
		return
	}
	entries, err := ReadArchive(name, data)
	if err != nil {
		v.failf("%s: %v", name, err)
		return
	}
	top := strings.TrimSuffix(strings.TrimSuffix(name, ".zip"), ".tar.gz")
	exe, helper := "cucinactl", credHelper
	if p.OS == "windows" {
		exe, helper = exe+".exe", helper+".exe"
	}
	want := map[string]bool{top + "/" + exe: true, top + "/" + helper: true}
	for _, d := range archiveDocs {
		want[top+"/"+d] = true
	}
	var exeData []byte
	var helperEntry *ArchiveEntry
	for i, e := range entries {
		switch {
		case e.Dir && strings.TrimSuffix(e.Name, "/") == top:
		case !want[e.Name]:
			v.failf("%s: unexpected entry %s", name, e.Name)
		case e.Name == top+"/"+exe:
			exeData = e.Data
			if e.Mode&0o111 == 0 {
				v.failf("%s: %s is not executable", name, exe)
			}
		case e.Name == top+"/"+helper:
			helperEntry = &entries[i]
		default:
			if e.Mode != 0o644 || len(e.Data) == 0 {
				v.failf("%s: %s must be a non-empty 0644 file", name, e.Name)
			}
		}
		delete(want, e.Name)
	}
	for missing := range want {
		v.failf("%s: missing %s", name, missing)
	}
	if exeData == nil || helperEntry == nil {
		return
	}
	switch {
	case p.OS == "windows" && !bytes.Equal(helperEntry.Data, exeData):
		v.failf("%s: %s is not a copy of %s", name, helper, exe)
	case p.OS != "windows" && helperEntry.HardLink != top+"/"+exe:
		v.failf("%s: %s must be a hard link to %s (got link %q)", name, helper, exe, helperEntry.HardLink)
	}
	if err := CheckBinary(exeData, p); err != nil {
		v.failf("%s: %s: %v", name, exe, err)
	}
	if !ContainsVersion(exeData, v.bi.Version) {
		v.failf("%s: %s does not embed version %s (stamping, ADR 0150)", name, exe, v.bi.Version)
	}
	if v.native && p == hostPlatform() {
		exePath, err := v.writeExec(exe, exeData)
		if err != nil {
			v.failf("%v", err)
			return
		}
		helperPath := filepath.Join(filepath.Dir(exePath), helper)
		if err := os.Link(exePath, helperPath); err != nil {
			v.failf("%v", err)
			return
		}
		v.expectOutput(name, exePath, []string{"--version"}, "cucinactl "+v.bi.Version)
		v.expectOutput(name, helperPath, []string{"--version"}, helper+" "+v.bi.Version)
	}
}

func (v *verifier) expectOutput(asset, path string, args []string, want string) {
	got, err := v.runNative(path, args...)
	switch {
	case err != nil:
		v.failf("%s: %v", asset, err)
	case got != want:
		v.failf("%s: `%s %s` printed %q, want %q", asset, filepath.Base(path), strings.Join(args, " "), got, want)
	default:
		v.notef("ran %s %s: %s", filepath.Base(path), strings.Join(args, " "), got)
	}
}

func (v *verifier) checkAgent(p Platform) {
	name := AgentName(v.bi.Version, p)
	data, ok := v.read(name)
	if !ok {
		return
	}
	if err := CheckBinary(data, p); err != nil {
		v.failf("%s: %v", name, err)
	}
	if !ContainsVersion(data, v.bi.Version) {
		v.failf("%s: does not embed version %s", name, v.bi.Version)
	}
	if v.native && p == hostPlatform() {
		if path, err := v.writeExec("cucina-worker-agent", data); err != nil {
			v.failf("%v", err)
		} else {
			v.expectOutput(name, path, []string{"version"}, fmt.Sprintf("cucina-worker-agent %s %s/%s", v.bi.Version, p.OS, p.Arch))
		}
	}
}

// checkHostd: darwin/arm64, built with cgo (managed preferences, R-MAC-10; keychain identity)
// and with an LC_UUID load command (Local Network privacy, R-MAC-1).
func (v *verifier) checkHostd() {
	name := HostdName(v.bi.Version)
	data, ok := v.read(name)
	if !ok {
		return
	}
	p := Platform{"darwin", "arm64"}
	if err := CheckBinary(data, p); err != nil {
		v.failf("%s: %v", name, err)
		return
	}
	if !ContainsVersion(data, v.bi.Version) {
		v.failf("%s: does not embed version %s", name, v.bi.Version)
	}
	f, err := macho.NewFile(bytes.NewReader(data))
	if err != nil {
		v.failf("%s: %v", name, err)
		return
	}
	syms, err := f.ImportedSymbols()
	if err != nil {
		v.failf("%s: %v", name, err)
	}
	for _, s := range []string{"_CFPreferencesCopyAppValue", "_SecItemCopyMatching"} {
		if !slices.Contains(syms, s) {
			v.failf("%s: does not import %s: built without cgo (managed preferences/keychain stubs)", name, s)
		}
	}
	hasUUID := false
	for _, l := range f.Loads {
		raw := l.Raw()
		if len(raw) >= 4 && binary.LittleEndian.Uint32(raw) == 0x1b { // LC_UUID
			hasUUID = true
		}
	}
	if !hasUUID {
		v.failf("%s: no LC_UUID load command (Local Network privacy, R-MAC-1)", name)
	}
	if v.native && p == hostPlatform() {
		if path, err := v.writeExec("cucina-hostd", data); err != nil {
			v.failf("%v", err)
		} else {
			v.expectOutput(name, path, []string{"version"}, v.bi.Version)
		}
	}
}

func lookup(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

func (v *verifier) checkChart(imageDigests map[string]string) {
	name := ChartName(v.bi.Version)
	data, ok := v.read(name)
	if !ok {
		return
	}
	chart, values, err := ReadChartPackage(data)
	if err != nil {
		v.failf("%s: %v", name, err)
		return
	}
	for _, k := range []string{"version", "appVersion"} {
		if got := fmt.Sprint(chart[k]); got != v.bi.Version {
			v.failf("%s: Chart.yaml %s is %q, want %q", name, k, got, v.bi.Version)
		}
	}
	if chart["name"] != "cucina" || chart["apiVersion"] != "v2" {
		v.failf("%s: Chart.yaml name/apiVersion are %v/%v", name, chart["name"], chart["apiVersion"])
	}
	if got := fmt.Sprint(lookup(values, "images", "controller", "tag")); got != v.bi.Version {
		v.failf("%s: values images.controller.tag is %q, want %q", name, got, v.bi.Version)
	}
	wantRepo := "ghcr.io/" + v.bi.Owner() + "/cucina-controller"
	if got := fmt.Sprint(lookup(values, "images", "controller", "repository")); got != wantRepo {
		v.failf("%s: values images.controller.repository is %q, want %q", name, got, wantRepo)
	}
	digest := fmt.Sprint(lookup(values, "images", "controller", "digest"))
	if want, ok := imageDigests["cucina-controller"]; ok && digest != want {
		v.failf("%s: values images.controller.digest is %q, but the built image is %s", name, digest, want)
	} else if !strings.HasPrefix(digest, "sha256:") {
		v.failf("%s: values images.controller.digest %q is not pinned", name, digest)
	}
	if v.helm != "" {
		helmTmp := filepath.Join(v.tmp, "helm")
		cmd := exec.Command(v.helm, "lint", v.asset(name))
		cmd.Env = append(os.Environ(), "HELM_CACHE_HOME="+helmTmp+"/cache", "HELM_CONFIG_HOME="+helmTmp+"/config", "HELM_DATA_HOME="+helmTmp+"/data")
		if out, err := cmd.CombinedOutput(); err != nil {
			v.failf("%s: helm lint: %v\n%s", name, err, out)
		} else {
			v.notef("helm lint %s: ok", name)
		}
	}
}

type pkgJSON struct {
	Version  string `json:"version"`
	SHA256   string `json:"sha256"`
	URL      string `json:"url"`
	BundleID string `json:"bundle_id"`
	Size     int64  `json:"size"`
}

// checkPkg: the product version is the numeric core (ADR 0753) and the manifests describe
// exactly this file at its release URL (R-MAC-8).
func (v *verifier) checkPkg() {
	base := PkgBase(v.bi.Core)
	data, ok := v.read(base + ".pkg")
	if !ok {
		return
	}
	for _, ext := range pkgCompanions[1:] {
		if !v.assets[base+ext] {
			v.failf("%s.pkg: missing companion %s%s", base, base, ext)
		}
		v.seen[base+ext] = true
	}
	pkg, err := ReadPkg(data)
	if err != nil {
		v.failf("%s.pkg: %v", base, err)
		return
	}
	product, refs, err := PkgVersions(pkg.Distribution, pkgID)
	if err != nil {
		v.failf("%s.pkg: %v", base, err)
	}
	if product != v.bi.Core || len(refs) == 0 || slices.ContainsFunc(refs, func(r string) bool { return r != v.bi.Core }) {
		v.failf("%s.pkg: product version %q, pkg-ref versions %v, want %s", base, product, refs, v.bi.Core)
	}
	switch {
	case v.signerSHA1 != "" && !strings.EqualFold(pkg.SignerSHA1, v.signerSHA1):
		v.failf("%s.pkg: signed by certificate %q, want %s", base, pkg.SignerSHA1, v.signerSHA1)
	case v.requireSig && pkg.SignerSHA1 == "":
		v.failf("%s.pkg: unsigned (R-MAC-9: MDM needs a signature the device can verify)", base)
	case pkg.SignerSHA1 == "":
		v.notef("%s.pkg: unsigned build", base)
	}
	sum, err := sha256File(v.asset(base + ".pkg"))
	if err != nil {
		v.failf("%v", err)
		return
	}
	var meta pkgJSON
	if raw, ok := v.read(base + ".json"); ok {
		if err := json.Unmarshal(raw, &meta); err != nil {
			v.failf("%s.json: %v", base, err)
		}
		if meta.Version != v.bi.Core || meta.SHA256 != sum || meta.BundleID != pkgID || meta.URL != v.bi.ReleaseURL(base+".pkg") || meta.Size != int64(len(data)) {
			v.failf("%s.json: %+v does not describe %s.pkg (version %s, sha256 %s, URL %s)", base, meta, base, v.bi.Core, sum, v.bi.ReleaseURL(base+".pkg"))
		}
	}
	if raw, ok := v.read(base + ".pkg.sha256"); ok && strings.TrimSpace(string(raw)) != sum+"  "+base+".pkg" {
		v.failf("%s.pkg.sha256: %q", base, strings.TrimSpace(string(raw)))
	}
	if raw, ok := v.read(base + ".plist"); ok {
		for _, s := range []string{v.bi.ReleaseURL(base + ".pkg"), sum, "<string>" + v.bi.Core + "</string>", pkgID, "software-package"} {
			if !bytes.Contains(raw, []byte(s)) {
				v.failf("%s.plist: manifest lacks %q", base, s)
			}
		}
	}
}

// checkImage: a linux/amd64 + linux/arm64 index on distroless nonroot with the OCI labels,
// the licence notices layer (R-ARTIFACT) and the stamped binary.
func (v *verifier) checkImage(name, layout string) string {
	want := []string{controllerBin, docDir + "/LICENSE.md", docDir + "/THIRD_PARTY_NOTICES.md"}
	digest, images, err := ReadImageLayout(layout, want)
	if err != nil {
		v.failf("image %s: %v", name, err)
		return ""
	}
	var platforms []string
	for _, img := range images {
		platforms = append(platforms, img.Platform)
		c := img.Config
		wantLabels := ImageLabels(v.bi, name, c.Config.Labels["org.opencontainers.image.description"], c.Config.Labels["org.opencontainers.image.base.name"], c.Config.Labels["org.opencontainers.image.base.digest"])
		for k, w := range wantLabels {
			if got := c.Config.Labels[k]; got != w {
				v.failf("image %s %s: label %s is %q, want %q", name, img.Platform, k, got, w)
			}
		}
		if c.Config.Labels["org.opencontainers.image.description"] == "" {
			v.failf("image %s %s: no description label", name, img.Platform)
		}
		if c.Created != v.bi.Created().Format(time.RFC3339) {
			v.failf("image %s %s: created %q, want the commit time %s", name, img.Platform, c.Created, v.bi.Created().Format(time.RFC3339))
		}
		if c.Config.User != nonrootUser {
			v.failf("image %s %s: user %q, want %s (distroless nonroot)", name, img.Platform, c.Config.User, nonrootUser)
		}
		if !slices.Equal(c.Config.Entrypoint, []string{controllerBin}) {
			v.failf("image %s %s: entrypoint %v, want [%s] (the chart passes the subcommand as args)", name, img.Platform, c.Config.Entrypoint, controllerBin)
		}
		for _, f := range want {
			if len(img.Files[f]) == 0 {
				v.failf("image %s %s: missing %s", name, img.Platform, f)
			}
		}
		bin := img.Files[controllerBin]
		_, arch, _ := strings.Cut(img.Platform, "/")
		p := Platform{"linux", arch}
		if bin != nil {
			if err := CheckBinary(bin, p); err != nil {
				v.failf("image %s %s: %v", name, img.Platform, err)
			}
			if !ContainsVersion(bin, v.bi.Version) {
				v.failf("image %s %s: cucina-controller does not embed version %s", name, img.Platform, v.bi.Version)
			}
			if v.native && p == hostPlatform() {
				if path, err := v.writeExec("cucina-controller", bin); err != nil {
					v.failf("%v", err)
				} else if out, err := v.runNative(path, "version"); err != nil {
					v.failf("image %s: %v", name, err)
				} else {
					var info struct{ Version, Commit string }
					if err := json.Unmarshal([]byte(out), &info); err != nil || info.Version != v.bi.Version || info.Commit != v.bi.Commit {
						v.failf("image %s: `cucina-controller version` printed %s, want version %s commit %q", name, out, v.bi.Version, v.bi.Commit)
					} else {
						v.notef("ran cucina-controller version (%s): %s", img.Platform, info.Version)
					}
				}
			}
		}
	}
	slices.Sort(platforms)
	if !slices.Equal(platforms, []string{"linux/amd64", "linux/arm64"}) {
		v.failf("image %s: platforms %v, want linux/amd64 + linux/arm64", name, platforms)
	}
	return digest
}

func (v *verifier) checkSums() {
	data, ok := v.read(sumsName)
	if !ok {
		return
	}
	sums, err := ParseSums(data)
	if err != nil {
		v.failf("%v", err)
		return
	}
	for name := range v.assets {
		if name == sumsName {
			continue
		}
		want, listed := sums[name]
		if !listed {
			v.failf("%s: %s is not listed", sumsName, name)
			continue
		}
		if got, err := sha256File(v.asset(name)); err != nil || got != want {
			v.failf("%s: %s does not match (%v)", sumsName, name, err)
		}
	}
	for name := range sums {
		if !v.assets[name] {
			v.failf("%s: lists %s, which is not in the release", sumsName, name)
		}
	}
}

func (v *verifier) checkFormula() {
	rb, err := os.ReadFile(filepath.Join(v.dir, metaDir, formulaName))
	if err != nil {
		v.failf("formula: %v", err)
		return
	}
	if !bytes.Contains(rb, []byte(`version "`+v.bi.Version+`"`)) {
		v.failf("formula: no version %q", v.bi.Version)
	}
	for _, p := range formulaPlatforms {
		name := CLIArchiveName(v.bi.Version, p)
		sum, err := sha256File(v.asset(name))
		if err != nil {
			v.failf("formula: %v", err)
			continue
		}
		block := regexp.MustCompile(`url "` + regexp.QuoteMeta(v.bi.ReleaseURL(name)) + `"\s+sha256 "` + sum + `"`)
		if !block.Match(rb) {
			v.failf("formula: no url/sha256 pair for %s", name)
		}
	}
}

func runVerify(args []string, stdout io.Writer) (err error) {
	fs := newFlags("verify")
	dir := fs.String("dir", "", "release or dist directory (assets/, meta/)")
	version := fs.String("version", "", "expected version (e.g. from the git tag)")
	final := fs.Bool("final", false, "a finalized release: SHA256SUMS and the Homebrew formula are required")
	helm := fs.String("helm", "", "helm binary for `helm lint` of the chart package")
	native := fs.Bool("exec-native", false, "run the binaries built for this host and check the versions they print")
	signer := fs.String("signer-sha1", "", "required SHA-1 of the macOS package signing certificate")
	requireSig := fs.Bool("require-signed", false, "the macOS package must be signed")
	var expects, ocis multiFlag
	fs.Var(&expects, "expect", "asset group that must be present: macos, pkg, linux (repeated)")
	fs.Var(&ocis, "oci", "NAME=LAYOUT: image to check (repeated)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := required(fs, "dir"); err != nil {
		return err
	}
	bi, err := LoadBuildInfo(filepath.Join(*dir, metaDir, buildInfoName))
	if err != nil {
		return err
	}
	if *version != "" && bi.Version != *version {
		return fmt.Errorf("built as version %s, expected %s", bi.Version, *version)
	}
	v := &verifier{dir: *dir, bi: bi, assets: map[string]bool{}, seen: map[string]bool{}, oci: map[string]string{},
		helm: *helm, native: *native, signerSHA1: *signer, requireSig: *requireSig}
	names, err := listFiles(filepath.Join(*dir, assetsDir))
	if err != nil {
		return err
	}
	for _, n := range names {
		v.assets[n] = true
	}
	if v.tmp, err = os.MkdirTemp("", "cucina-verify-"); err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(v.tmp) }()

	// Required assets per group.
	groups := map[string][]string{
		groupMacOS: {CLIArchiveName(bi.Version, cliPlatforms[0]), AgentName(bi.Version, agentPlatforms[0]), HostdName(bi.Version)},
		groupPkg:   {PkgBase(bi.Core) + ".pkg"},
		groupLinux: {ChartName(bi.Version)},
	}
	for _, p := range cliPlatforms[1:] {
		groups[groupLinux] = append(groups[groupLinux], CLIArchiveName(bi.Version, p), AgentName(bi.Version, p))
	}
	for _, g := range expects {
		names, ok := groups[g]
		if !ok {
			return fmt.Errorf("unknown --expect group %q", g)
		}
		for _, n := range names {
			if !v.assets[n] {
				v.failf("group %s: missing %s", g, n)
			}
		}
	}
	if *final && !v.assets[sumsName] {
		v.failf("missing %s", sumsName)
	}

	// Image digests recorded by the build (meta/images.json) and checked against layouts.
	imageDigests := map[string]string{}
	if raw, err := os.ReadFile(filepath.Join(*dir, metaDir, imagesName)); err == nil {
		if err := json.Unmarshal(raw, &imageDigests); err != nil {
			v.failf("%s: %v", imagesName, err)
		}
	}
	ociKV, err := pairs(ocis)
	if err != nil {
		return err
	}
	for _, kv := range ociKV {
		digest := v.checkImage(kv[0], kv[1])
		if want, ok := imageDigests[kv[0]]; ok && digest != "" && want != digest {
			v.failf("image %s: layout digest %s, but %s records %s", kv[0], digest, imagesName, want)
		}
		if digest != "" {
			imageDigests[kv[0]] = digest
			v.notef("image %s: %s", kv[0], digest)
		}
	}

	for _, p := range cliPlatforms {
		v.checkCLIArchive(p)
	}
	for _, p := range agentPlatforms {
		v.checkAgent(p)
	}
	v.checkHostd()
	v.checkChart(imageDigests)
	v.checkPkg()
	v.checkSums()
	if *final {
		v.checkFormula()
	}
	for _, n := range names {
		if !v.seen[n] && !strings.HasPrefix(n, "cucina-host-signer-") {
			v.failf("unexpected asset %s (not a known release artifact of version %s)", n, bi.Version)
		}
	}

	for _, n := range v.notes {
		_, _ = fmt.Fprintf(stdout, "note: %s\n", n)
	}
	if len(v.errs) > 0 {
		for _, e := range v.errs {
			_, _ = fmt.Fprintf(stdout, "FAIL: %v\n", e)
		}
		return fmt.Errorf("%d problem(s) in %s", len(v.errs), *dir)
	}
	_, err = fmt.Fprintf(stdout, "ok: %d assets of %s %s (commit %s) agree\n", len(names), bi.Repository, bi.Version, orNone(bi.Commit))
	return err
}

func orNone(s string) string {
	if s == "" {
		return "none (unstamped)"
	}
	return s
}
