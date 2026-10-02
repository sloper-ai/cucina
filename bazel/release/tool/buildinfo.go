// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DevVersion is what unstamped builds report everywhere (ADR 0150): it is valid SemVer for the
// chart and images, and cannot be mistaken for a release.
const DevVersion = "0.0.0-dev"

// DefaultRepository is the GitHub repository releases are published from (R-OPS-7).
const DefaultRepository = "sloper-ai/cucina"

// Workspace status keys written by release/workspace-status.sh.
const (
	keyVersion    = "STABLE_CUCINA_VERSION"
	keyCommit     = "STABLE_CUCINA_COMMIT"
	keyDirty      = "STABLE_CUCINA_DIRTY"
	keyEpoch      = "STABLE_CUCINA_SOURCE_DATE_EPOCH"
	keyRepository = "STABLE_CUCINA_REPOSITORY"
)

var (
	commitRE     = regexp.MustCompile(`^([0-9a-f]{7,64})?$`)
	repositoryRE = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)
)

// semverRE is SemVer 2.0.0 without build metadata ("+" is not allowed in OCI tags).
var semverRE = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)` +
	`(?:-((?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?$`)

// BuildInfo is the version information every release artifact is stamped with.
type BuildInfo struct {
	Version    string `json:"version"`    // SemVer, e.g. 0.1.0 or 0.0.0-dryrun
	Core       string `json:"core"`       // MAJOR.MINOR.PATCH: the macOS package version (ADR 0753)
	Prerelease bool   `json:"prerelease"` // Version has a pre-release part
	Commit     string `json:"commit"`     // source commit ("" when unstamped)
	Epoch      int64  `json:"epoch"`      // SOURCE_DATE_EPOCH: commit time (0 when unstamped)
	Repository string `json:"repository"` // OWNER/REPO on GitHub
	Stamped    bool   `json:"stamped"`
	Dirty      bool   `json:"dirty"` // uncommitted source changes; never publish these builds
}

// ParseVersion validates a release version and returns its MAJOR.MINOR.PATCH core.
func ParseVersion(v string) (core string, prerelease bool, err error) {
	m := semverRE.FindStringSubmatch(v)
	if m == nil {
		return "", false, fmt.Errorf("version %q is not MAJOR.MINOR.PATCH[-PRERELEASE] (SemVer 2.0.0, no build metadata)", v)
	}
	return m[1] + "." + m[2] + "." + m[3], m[4] != "", nil
}

// CheckCLIVersion validates the public --version banner of either CLI personality.
func CheckCLIVersion(program, version, output string) error {
	want := strings.TrimSuffix(program, ".exe") + " " + version
	if strings.TrimSpace(output) != want {
		return fmt.Errorf("%s --version printed %q, want %q", program, output, want)
	}
	return nil
}

// NewBuildInfo validates the fields and derives Core and Prerelease.
func NewBuildInfo(version, commit string, epoch int64, repository string, stamped bool) (BuildInfo, error) {
	core, pre, err := ParseVersion(version)
	if err != nil {
		return BuildInfo{}, err
	}
	if repository == "" {
		repository = DefaultRepository
	}
	if !repositoryRE.MatchString(repository) {
		return BuildInfo{}, fmt.Errorf("repository %q is not OWNER/REPO", repository)
	}
	if !commitRE.MatchString(commit) {
		return BuildInfo{}, fmt.Errorf("commit %q is not a git object name", commit)
	}
	if epoch < 0 {
		return BuildInfo{}, fmt.Errorf("negative source date epoch %d", epoch)
	}
	return BuildInfo{
		Version: version, Core: core, Prerelease: pre, Commit: commit, Epoch: epoch,
		Repository: repository, Stamped: stamped, Dirty: !stamped,
	}, nil
}

// ReadStatus parses a Bazel workspace status file ("KEY VALUE" lines).
func ReadStatus(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	s := bufio.NewScanner(r)
	for s.Scan() {
		line := strings.TrimRight(s.Text(), "\r")
		if line == "" {
			continue
		}
		key, value, _ := strings.Cut(line, " ")
		out[key] = value
	}
	return out, s.Err()
}

// BuildInfoFromStatus reads the release keys of a stable status file.
func BuildInfoFromStatus(status map[string]string) (BuildInfo, error) {
	version, ok := status[keyVersion]
	if !ok {
		return BuildInfo{}, fmt.Errorf("stamped build without %s: pass --workspace_status_command=release/workspace-status.sh with --stamp", keyVersion)
	}
	var epoch int64
	if s := status[keyEpoch]; s != "" {
		var err error
		if epoch, err = strconv.ParseInt(s, 10, 64); err != nil {
			return BuildInfo{}, fmt.Errorf("%s: %w", keyEpoch, err)
		}
	}
	bi, err := NewBuildInfo(version, status[keyCommit], epoch, status[keyRepository], true)
	if err != nil {
		return BuildInfo{}, err
	}
	bi.Dirty, err = strconv.ParseBool(status[keyDirty])
	if err != nil {
		return BuildInfo{}, fmt.Errorf("%s must be true or false: %w", keyDirty, err)
	}
	return bi, nil
}

// LoadBuildInfo reads a JSON file written by `buildinfo`.
func LoadBuildInfo(path string) (BuildInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return BuildInfo{}, err
	}
	var bi BuildInfo
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&bi); err != nil {
		return BuildInfo{}, fmt.Errorf("%s: %w", path, err)
	}
	// Re-validate: the file may come from another job. Preserve the dirty bit; it is part
	// of the build identity used to reject mixed dists in finalize.
	validated, err := NewBuildInfo(bi.Version, bi.Commit, bi.Epoch, bi.Repository, bi.Stamped)
	validated.Dirty = bi.Dirty || !bi.Stamped
	return validated, err
}

// Created is the image/archive timestamp: the commit time, so rebuilds are reproducible.
func (b BuildInfo) Created() time.Time { return time.Unix(b.Epoch, 0).UTC() }

// Owner is the lower-cased repository owner (GHCR namespaces are lower case).
func (b BuildInfo) Owner() string {
	owner, _, _ := strings.Cut(b.Repository, "/")
	return strings.ToLower(owner)
}

// Tag is the git tag of the release.
func (b BuildInfo) Tag() string { return "v" + b.Version }

// ReleaseURL is where GitHub serves an asset of this release.
func (b BuildInfo) ReleaseURL(asset string) string {
	return "https://github.com/" + b.Repository + "/releases/download/" + b.Tag() + "/" + asset
}

// Expand fills {version}, {core} and {core_dashed} in a file-name template.
func (b BuildInfo) Expand(template string) string {
	return strings.NewReplacer(
		"{version}", b.Version,
		"{core}", b.Core,
		"{core_dashed}", strings.ReplaceAll(b.Core, ".", "-"),
	).Replace(template)
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func runBuildInfo(args []string, _ io.Writer) error {
	fs := newFlags("buildinfo")
	statusPath := fs.String("stable-status", "", "Bazel stable status file (stamped builds)")
	outJSON := fs.String("out-json", "", "output: build info JSON")
	outEnv := fs.String("out-env", "", "output: shell-sourceable variables (VERSION, CORE, ...)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := required(fs, "out-json"); err != nil {
		return err
	}
	bi, err := NewBuildInfo(DevVersion, "", 0, "", false)
	if err != nil {
		return err
	}
	if *statusPath != "" {
		f, err := os.Open(*statusPath)
		if err != nil {
			return err
		}
		status, err := ReadStatus(f)
		_ = f.Close()
		if err != nil {
			return err
		}
		if bi, err = BuildInfoFromStatus(status); err != nil {
			return err
		}
	}
	if err := writeJSON(*outJSON, bi); err != nil {
		return err
	}
	if *outEnv == "" {
		return nil
	}
	return os.WriteFile(*outEnv, []byte(bi.Env()), 0o644)
}

// Env renders the build info as shell assignments. Every value is validated (SemVer, hex,
// OWNER/REPO, digits), so single quotes are safe.
func (b BuildInfo) Env() string {
	var s strings.Builder
	for _, kv := range [][2]string{
		{"VERSION", b.Version}, {"CORE", b.Core}, {"PRERELEASE", strconv.FormatBool(b.Prerelease)},
		{"COMMIT", b.Commit}, {"EPOCH", strconv.FormatInt(b.Epoch, 10)}, {"REPOSITORY", b.Repository},
		{"TAG", b.Tag()}, {"RELEASE_URL", strings.TrimSuffix(b.ReleaseURL(""), "/")},
		{"DIRTY", strconv.FormatBool(b.Dirty)},
	} {
		fmt.Fprintf(&s, "%s='%s'\n", kv[0], kv[1])
	}
	return s.String()
}
