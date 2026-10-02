// SPDX-License-Identifier: FSL-1.1-ALv2

package scenarios

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/sloper-ai/cucina/test/e2e/bazelrun"
	"github.com/sloper-ai/cucina/test/e2e/collect/execlog"
	"github.com/sloper-ai/cucina/test/e2e/harness"
	"github.com/sloper-ai/cucina/test/e2e/infra"
	"github.com/sloper-ai/cucina/test/e2e/nfr"
	"github.com/sloper-ai/cucina/test/e2e/remote"
)

// ---------------------------------------------------------------- platforms/targets.json

// Targets is platforms/targets.json, schema 1 (docs/cross-compilation.md
// "Schema"; owned by the cross tooling). Unknown fields are ignored: the
// schema only grows within version 1.
type Targets struct {
	SchemaVersion     int                `json:"schemaVersion"`
	HermeticLLVM      HermeticLLVM       `json:"hermeticLlvm"`
	ExcludedPlatforms []ExcludedPlatform `json:"excludedPlatforms"`
	ExecPlatforms     []ExecPlatform     `json:"execPlatforms"`
	Targets           []Target           `json:"targets"`
}

// HermeticLLVM pins the `llvm` Bazel module whose platforms the targets use.
type HermeticLLVM struct {
	Module      string `json:"module"`
	Version     string `json:"version"`
	LLVMVersion string `json:"llvmVersion"`
}

// ExcludedPlatform is an OS/CPU pair that is neither a target nor an exec
// platform.
type ExcludedPlatform struct {
	OS     string `json:"os"`
	CPU    string `json:"cpu"`
	Reason string `json:"reason"`
}

// ExecPlatform is one compile exec platform: a compile-capable pool runner
// whose exec_properties are exactly that runner's properties.
type ExecPlatform struct {
	Name        string   `json:"name"` // the pool name; `--exec-pool` value
	Label       string   `json:"label"`
	Pool        string   `json:"pool"`
	Runner      string   `json:"runner"`
	OS          string   `json:"os"`
	CPU         string   `json:"cpu"`
	Constraints []string `json:"constraints"`
	// Flags are required whenever this platform compiles (the macOS SDK
	// version); `cucinactl bazelrc` emits them.
	Flags []string `json:"flags"`
}

// Target is one hermetic-llvm target row of the R-XPLAT-1 matrix.
type Target struct {
	Name     string   `json:"name"`
	Aliases  []string `json:"aliases"`
	Platform string   `json:"platform"`
	OS       string   `json:"os"`
	CPU      string   `json:"cpu"`
	Libc     string   `json:"libc"` // null → ""
	ABI      string   `json:"abi"`  // null → ""; "msvc" needs the EULA flags
	// ExecPlatforms are the allowed compile exec platforms, default first.
	ExecPlatforms []string  `json:"execPlatforms"`
	Test          *TestSpec `json:"test"`
	TestMode      string    `json:"testMode"` // native | qemu-user | none
	TestSkip      string    `json:"testSkip"` // incompatible | not-applicable
	Coverage      string    `json:"coverage"`
	// TestTimeoutScale multiplies Bazel's test timeouts under emulation
	// (absent: 1).
	TestTimeoutScale *float64 `json:"testTimeoutScale,omitempty"`
	Notes            string   `json:"notes"`
	Excluded         bool     `json:"excluded"`
	Reason           string   `json:"reason"`
}

// TestSpec is where a target's tests run: the twin test exec platform and
// the pool runner whose properties it carries.
type TestSpec struct {
	Label  string `json:"label"`
	Pool   string `json:"pool"`
	Runner string `json:"runner"`
	Mode   string `json:"mode"` // native | qemu-user
}

// Campaign coverage (R-XPLAT-1) and test-step semantics of a target row.
const (
	CoverageFull          = "full"                  // //absl/... build + test
	CoverageSmoke         = "build-full-test-smoke" // full build, smoke tests (MUST), scaled timeouts
	CoverageExample       = "build-example"         // build-only example (no OS)
	TestSkipNotApplicable = "not-applicable"
)

func parseTargets(b []byte) (*Targets, error) {
	var t Targets
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("platforms/targets.json: %w", err)
	}
	if err := t.validate(); err != nil {
		return nil, fmt.Errorf("platforms/targets.json: %w", err)
	}
	return &t, nil
}

func loadTargets(env *harness.Env) (*Targets, error) {
	b, err := os.ReadFile(filepath.Join(env.RepoDir, "platforms", "targets.json"))
	if err != nil {
		return nil, harness.Skip("platforms/targets.json is not available: %v", err)
	}
	return parseTargets(b)
}

// validate checks what the matrix runner relies on; a schema change it does
// not understand fails loudly instead of measuring the wrong thing.
func (t *Targets) validate() error {
	if t.SchemaVersion != 1 {
		return fmt.Errorf("schemaVersion %d, want 1", t.SchemaVersion)
	}
	if t.HermeticLLVM.Version == "" {
		return fmt.Errorf("hermeticLlvm.version is empty")
	}
	for _, e := range t.ExecPlatforms {
		if e.Name == "" || e.Label == "" || e.Pool == "" || e.Runner == "" {
			return fmt.Errorf("exec platform %+v is incomplete", e)
		}
	}
	for _, tg := range t.Targets {
		if tg.Excluded {
			if tg.Reason == "" {
				return fmt.Errorf("excluded target %s has no reason", tg.Name)
			}
			continue
		}
		if tg.Platform == "" || len(tg.ExecPlatforms) == 0 {
			return fmt.Errorf("target %s: no platform or exec platforms", tg.Name)
		}
		for _, n := range tg.ExecPlatforms {
			if _, ok := t.execPlatform(n); !ok {
				return fmt.Errorf("target %s: unknown exec platform %q", tg.Name, n)
			}
		}
		buildOnly := tg.TestSkip == TestSkipNotApplicable
		switch tg.Coverage {
		case CoverageFull, CoverageSmoke:
			if buildOnly || tg.Test == nil {
				return fmt.Errorf("target %s: coverage %s needs a test placement", tg.Name, tg.Coverage)
			}
		case CoverageExample:
			if !buildOnly || tg.Test != nil {
				return fmt.Errorf("target %s: coverage %s is build-only (testSkip %s, test null)", tg.Name, tg.Coverage, TestSkipNotApplicable)
			}
		default:
			return fmt.Errorf("target %s: unknown coverage %q", tg.Name, tg.Coverage)
		}
		if tg.Test != nil && (tg.Test.Label == "" || tg.Test.Pool == "" || tg.Test.Runner == "") {
			return fmt.Errorf("target %s: incomplete test placement %+v", tg.Name, *tg.Test)
		}
		if s := tg.TestTimeoutScale; s != nil && *s <= 0 {
			return fmt.Errorf("target %s: testTimeoutScale %v", tg.Name, *s)
		}
	}
	return nil
}

func (t *Targets) execPlatform(name string) (ExecPlatform, bool) {
	for _, e := range t.ExecPlatforms {
		if e.Name == name {
			return e, true
		}
	}
	return ExecPlatform{}, false
}

func (t *Targets) target(name string) (Target, bool) {
	for _, tg := range t.Targets {
		if tg.Name == name {
			return tg, true
		}
	}
	return Target{}, false
}

// RunnerProps maps pool → runner → exact REAPI platform properties
// (platforms/pools.json).
type RunnerProps map[string]map[string]map[string]string

func parsePools(b []byte) (RunnerProps, error) {
	var doc struct {
		Platforms []struct {
			Name    string `json:"name"`
			Runners []struct {
				Name       string            `json:"name"`
				Properties map[string]string `json:"properties"`
			} `json:"runners"`
		} `json:"platforms"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("platforms/pools.json: %w", err)
	}
	out := RunnerProps{}
	for _, p := range doc.Platforms {
		out[p.Name] = map[string]map[string]string{}
		for _, r := range p.Runners {
			out[p.Name][r.Name] = r.Properties
		}
	}
	return out, nil
}

func runnerProps(env *harness.Env) (RunnerProps, error) {
	b, err := os.ReadFile(filepath.Join(env.RepoDir, "platforms", "pools.json"))
	if err != nil {
		return nil, err
	}
	return parsePools(b)
}

// Expectation is where a configuration's actions must run (NFR-X1): the
// REAPI property sets of the compile pool's runner and the target's test
// runner. Buildbarn matches an action's entire property set to a queue, so
// these are the exec properties of the queues (and workers) that execute.
type Expectation struct {
	Compile, Test map[string]string
}

// expectation resolves a configuration's runners in pools.json. macOS-target
// tests run on the Xcode runner, not the generic one (ADR 0903), because
// targets.json places them there.
func (p RunnerProps) expectation(t *Targets, cfg Config) (Expectation, error) {
	ep, ok := t.execPlatform(cfg.ExecPool)
	if !ok {
		return Expectation{}, fmt.Errorf("unknown exec platform %q", cfg.ExecPool)
	}
	var e Expectation
	if e.Compile = p[ep.Pool][ep.Runner]; e.Compile == nil {
		return Expectation{}, fmt.Errorf("exec platform %s: runner %s/%s not in platforms/pools.json", ep.Name, ep.Pool, ep.Runner)
	}
	if ts := cfg.Target.Test; ts != nil {
		if e.Test = p[ts.Pool][ts.Runner]; e.Test == nil {
			return Expectation{}, fmt.Errorf("target %s: test runner %s/%s not in platforms/pools.json", cfg.Target.Name, ts.Pool, ts.Runner)
		}
	}
	return e, nil
}

// ---------------------------------------------------------------- planning

// Scope selects what a configuration builds and tests.
type Scope int

const (
	// ScopeCoverage follows the target's campaign coverage (R-XPLAT-1).
	ScopeCoverage Scope = iota
	// ScopeSmoke tests the smoke subset only (T17's exec-coverage rows).
	ScopeSmoke
)

// Test-step dispositions of a configuration.
const (
	TestStepFull  = "full suite"
	TestStepSmoke = "smoke subset"
	// TestStepNA: build-only targets (no OS to run tests on, UC27).
	TestStepNA = "not applicable"
)

// smokePatterns is the smoke subset of build-full-test-smoke targets
// (R-XPLAT-1: MUST for riscv64, s390x and armv7 under qemu-user; the full
// suite is SHOULD).
var smokePatterns = []string{"//absl/base/...", "//absl/strings/...", "//absl/numeric/..."}

// Step is one Bazel invocation of a configuration.
type Step struct {
	Name     string   `json:"name"` // build | test
	Command  string   `json:"command"`
	Flags    []string `json:"flags,omitempty"`
	Patterns []string `json:"patterns"`
}

// Config is one (client host, compile exec platform, target) combination.
type Config struct {
	Name     string // <target>@<client>[@exec-<pool>]
	Client   Lane
	Target   Target
	ExecPool string // compile exec platform; the target's default (first) unless chosen
	Steps    []Step
	TestStep string
}

// testTimeouts scales Bazel's default test timeouts (60, 300, 900, 3600 s)
// by testTimeoutScale (qemu-user runners: 10 → 600,3000,9000,36000).
func testTimeouts(scale *float64) string {
	if scale == nil || *scale == 1 {
		return ""
	}
	parts := make([]string, 4)
	for i, s := range []float64{60, 300, 900, 3600} {
		parts[i] = strconv.Itoa(int(math.Ceil(s * *scale)))
	}
	return "--test_timeout=" + strings.Join(parts, ",")
}

func configName(tg Target, client Lane, execPool string) string {
	n := tg.Name + "@" + client.Host
	if len(tg.ExecPlatforms) > 0 && execPool != tg.ExecPlatforms[0] {
		n += "@exec-" + execPool
	}
	return n
}

// planConfig turns a target row into the invocations of one configuration.
// Excluded targets are rejected with their reason (UC27).
func planConfig(tg Target, client Lane, execPool string, scope Scope) (Config, error) {
	if tg.Excluded {
		return Config{}, fmt.Errorf("target %s is excluded: %s", tg.Name, tg.Reason)
	}
	if len(tg.ExecPlatforms) == 0 {
		return Config{}, fmt.Errorf("target %s has no exec platforms", tg.Name)
	}
	if execPool == "" {
		execPool = tg.ExecPlatforms[0]
	} else if !slices.Contains(tg.ExecPlatforms, execPool) {
		return Config{}, fmt.Errorf("pool %s cannot compile %s (allowed: %s)", execPool, tg.Name, strings.Join(tg.ExecPlatforms, ", "))
	}
	cfg := Config{Name: configName(tg, client, execPool), Client: client, Target: tg, ExecPool: execPool}
	var timeouts []string
	if f := testTimeouts(tg.TestTimeoutScale); f != "" {
		timeouts = []string{f}
	}
	switch {
	case tg.TestSkip == TestSkipNotApplicable || tg.Test == nil:
		cfg.Steps = []Step{{Name: "build", Command: "build", Patterns: bazelrun.ExamplePatterns}}
		cfg.TestStep = TestStepNA
	case scope == ScopeSmoke:
		cfg.Steps = []Step{{Name: "test", Command: "test", Flags: timeouts, Patterns: smokePatterns}}
		cfg.TestStep = TestStepSmoke
	case tg.Coverage == CoverageSmoke:
		cfg.Steps = []Step{
			{Name: "build", Command: "build", Patterns: bazelrun.AbseilTargets},
			{Name: "test", Command: "test", Flags: timeouts, Patterns: smokePatterns},
		}
		cfg.TestStep = TestStepSmoke
	case tg.Coverage == CoverageFull:
		cfg.Steps = []Step{{Name: "build", Command: "build", Patterns: bazelrun.AbseilTargets}, {Name: "test", Command: "test", Flags: timeouts, Patterns: bazelrun.AbseilTargets}}
		cfg.TestStep = TestStepFull
	default:
		return Config{}, fmt.Errorf("target %s: coverage %q", tg.Name, tg.Coverage)
	}
	return cfg, nil
}

// plan returns one configuration per in-scope target accepted by filter.
func plan(t *Targets, client Lane, scope Scope, filter func(Target) bool) ([]Config, error) {
	var out []Config
	for _, tg := range t.Targets {
		if tg.Excluded || (filter != nil && !filter(tg)) {
			continue
		}
		cfg, err := planConfig(tg, client, "", scope)
		if err != nil {
			return nil, err
		}
		out = append(out, cfg)
	}
	return out, nil
}

// ---------------------------------------------------------------- the .bazelrc of a configuration

// rcTokens splits one .bazelrc line like Bazel's rc tokenizer: whitespace
// separates, quotes group, a backslash outside single quotes escapes, an
// unquoted '#' at a token start begins a comment.
func rcTokens(line string) []string {
	var toks []string
	var cur strings.Builder
	in, esc := false, false
	var quote rune
	for _, r := range line {
		switch {
		case esc:
			cur.WriteRune(r)
			esc, in = false, true
		case quote != 0:
			switch {
			case r == quote:
				quote = 0
			case r == '\\' && quote == '"':
				esc = true
			default:
				cur.WriteRune(r)
			}
		case r == '\\':
			esc, in = true, true
		case r == '\'' || r == '"':
			quote, in = r, true
		case r == '#' && !in:
			return toks
		case unicode.IsSpace(r):
			if in {
				toks = append(toks, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if in {
		toks = append(toks, cur.String())
	}
	return toks
}

// rcFlags returns every flag of a .bazelrc fragment in order (the command
// word of each line dropped).
func rcFlags(rc string) []string {
	var out []string
	for _, line := range strings.Split(rc, "\n") {
		if toks := rcTokens(line); len(toks) > 1 {
			out = append(out, toks[1:]...)
		}
	}
	return out
}

// canonicalLabel strips the apparent/canonical repository spelling
// ("@@cucina_platforms+//exec:x" == "@cucina_platforms//exec:x").
func canonicalLabel(l string) string {
	l = strings.TrimPrefix(strings.TrimPrefix(l, "@@"), "@")
	repo, rest, ok := strings.Cut(l, "//")
	if !ok {
		return l
	}
	return strings.TrimSuffix(repo, "+") + "//" + rest
}

// Documented lines of docs/cross-compilation.md the harness checks for (and
// appends when `cucinactl bazelrc --cross` lacks them).
var (
	msvcEULA = []string{
		"--repo_env=BAZEL_MSVC_RUNTIME_VISUAL_STUDIO_EULA=1",
		"--repo_env=BAZEL_WINDOWS_SDK_EULA=1",
	}
	windowsTestEnv = map[string]string{
		"--test_env=SYSTEMROOT=": `C:\Windows`,
		"--test_env=PATH=":       `C:\Windows\System32;C:\Windows;C:\Windows\System32\Wbem;C:\Windows\System32\WindowsPowerShell\v1.0`,
	}
	// windowsClientCross: Windows clients of cross configurations send the
	// action environment of Linux/macOS clients (cache sharing).
	windowsClientCross = []string{
		"--action_env=PATH=/bin:/usr/bin:/usr/local/bin",
		"--host_action_env=PATH=/bin:/usr/bin:/usr/local/bin",
		"--enable_runfiles",
	}
)

// rcReport is the contract check of one `cucinactl bazelrc --cross` output.
type rcReport struct {
	// Fatal: the platform placement is wrong, the configuration is not run.
	Fatal bool
	// Issues are contract violations (the configuration fails).
	Issues []string
	// Fix are the documented lines the harness appends so the configuration
	// still runs as documented and its measurements stay meaningful.
	Fix []string
}

func (r *rcReport) miss(issue string, fix ...string) {
	r.Issues = append(r.Issues, issue)
	r.Fix = append(r.Fix, fix...)
}

// rcContract checks cucinactl's cross configuration against targets.json and
// docs/cross-compilation.md: --platforms, the exec platform list (chosen
// compile platform first, the others in targets.json order, then the twin of
// runnable targets), --host_platform, the chosen platform's flags, MSVC EULA
// flags exactly for abi msvc, the Windows test environment for Windows
// targets from every client OS, scaled test timeouts, the Windows-client
// lines and the §10.2 client flags.
func rcContract(t *Targets, cfg Config, clientOS, rc string) rcReport {
	var r rcReport
	flags := rcFlags(rc)
	has := func(f string) bool { return slices.Contains(flags, f) }
	last := func(prefix string) (string, bool) {
		for i := len(flags) - 1; i >= 0; i-- {
			if v, ok := strings.CutPrefix(flags[i], prefix); ok {
				return v, true
			}
		}
		return "", false
	}
	tg := cfg.Target
	ep, ok := t.execPlatform(cfg.ExecPool)
	if !ok {
		return rcReport{Fatal: true, Issues: []string{"unknown exec platform " + cfg.ExecPool}}
	}

	want := []string{ep.Label}
	for _, n := range tg.ExecPlatforms {
		if e, _ := t.execPlatform(n); n != ep.Name {
			want = append(want, e.Label)
		}
	}
	if tg.Test != nil {
		want = append(want, tg.Test.Label)
	}
	if v, _ := last("--platforms="); canonicalLabel(v) != canonicalLabel(tg.Platform) {
		r.Fatal, r.Issues = true, append(r.Issues, fmt.Sprintf("--platforms=%q, want %s", v, tg.Platform))
	}
	if v, _ := last("--host_platform="); canonicalLabel(v) != canonicalLabel(ep.Label) {
		r.Fatal, r.Issues = true, append(r.Issues, fmt.Sprintf("--host_platform=%q, want %s", v, ep.Label))
	}
	v, _ := last("--extra_execution_platforms=")
	got := strings.Split(v, ",")
	same := len(got) == len(want)
	for i := 0; same && i < len(want); i++ {
		same = canonicalLabel(got[i]) == canonicalLabel(want[i])
	}
	if !same {
		r.Fatal, r.Issues = true, append(r.Issues, fmt.Sprintf("--extra_execution_platforms=%s, want %s", v, strings.Join(want, ",")))
	}
	if !slices.ContainsFunc(flags, func(f string) bool { return strings.HasPrefix(f, "--remote_executor=grpcs://") }) {
		r.Fatal, r.Issues = true, append(r.Issues, "no --remote_executor=grpcs://")
	}
	if r.Fatal {
		return r
	}

	for _, f := range ep.Flags {
		if !has(f) {
			r.miss("missing "+f+" (flags of exec platform "+ep.Name+")", "common "+f)
		}
	}
	if !has("--experimental_platform_in_output_dir") {
		r.miss("missing --experimental_platform_in_output_dir", "common --experimental_platform_in_output_dir")
	}
	for _, f := range msvcEULA {
		switch {
		case tg.ABI == "msvc" && !has(f):
			r.miss("missing "+f+" (abi msvc)", "common "+f)
		case tg.ABI != "msvc" && has(f):
			r.Issues = append(r.Issues, f+" for a non-MSVC target")
		}
	}
	if tg.OS == "windows" && tg.Test != nil {
		for _, prefix := range []string{"--test_env=SYSTEMROOT=", "--test_env=PATH="} {
			if _, ok := last(prefix); !ok {
				r.miss("missing "+prefix+"… (Windows test environment, R-XPLAT-4)", "common "+bazelrun.RCQuote(prefix+windowsTestEnv[prefix]))
			}
		}
	}
	if want := testTimeouts(tg.TestTimeoutScale); want != "" {
		if !has(want) {
			r.miss("missing "+want+" (testTimeoutScale)", "common "+want)
		}
	} else if _, ok := last("--test_timeout="); ok {
		r.Issues = append(r.Issues, "--test_timeout for a target without testTimeoutScale")
	}
	if clientOS == remote.Windows {
		for _, f := range windowsClientCross {
			if !has(f) {
				r.miss("missing "+f+" (Windows client, cross configuration)", "common "+f)
			}
		}
		if !has("--windows_enable_symlinks") {
			r.miss("missing startup --windows_enable_symlinks (Windows client)", "startup --windows_enable_symlinks")
		}
	}
	for _, alts := range clientFlags {
		if !slices.ContainsFunc(flags, func(f string) bool {
			return slices.ContainsFunc(alts, func(a string) bool { return strings.HasPrefix(f, a) })
		}) {
			r.Issues = append(r.Issues, "missing §10.2 flag "+alts[0])
		}
	}
	for _, f := range forbiddenFlags {
		if slices.ContainsFunc(flags, func(x string) bool { return strings.HasPrefix(x, f) }) {
			r.Issues = append(r.Issues, "forbidden flag "+f)
		}
	}
	return r
}

// dialectConfig is the C++ dialect configuration of abseil.bazelrc for the
// target's compiler driver: hermetic-llvm drives MSVC targets with clang-cl.
func dialectConfig(tg Target) string {
	if tg.ABI == "msvc" {
		return "cross-clang-cl"
	}
	return "cross-gnu"
}

// needsTestOverlay: Windows tests from a non-Windows client need the
// @bazel_tools overlay with tw.exe/xml.exe (R-XPLAT-4, ADR 0904).
func needsTestOverlay(cfg Config, clientOS string) bool {
	return cfg.Target.OS == "windows" && cfg.TestStep != TestStepNA && clientOS != remote.Windows
}

// composeRC is cucinactl's output plus the harness's lines: the Abseil
// configuration, the overlay line and the documented lines cucinactl lacked.
func composeRC(cucinactl string, cfg Config, overlay string, fix []string) (string, []string) {
	added := []string{"common --config=cross", "common --config=" + dialectConfig(cfg.Target)}
	if overlay != "" {
		added = append(added, overlay)
	}
	added = append(added, fix...)
	return strings.TrimRight(cucinactl, "\n") + "\n\n# --- Added by the Cucina e2e harness (test/e2e/scenarios/cross.go) ---\n" +
		strings.Join(added, "\n") + "\n", added
}

// ---------------------------------------------------------------- offline pre-check and overlay

// xplatcheckWorkdir is the pre-check's scratch workspace (docs/cross-compilation.md).
const xplatcheckWorkdir = "/tmp/xplatcheck"

var xplatcheckRun struct {
	sync.Once
	summary string
	err     error
}

// xplatPrecheck runs the offline resolution check of the matrix once per
// harness process, in the dev Mac checkout, before a cross scenario spends
// money: `bazel run //tools/xplat/cmd/xplatcheck -- -workdir /tmp/xplatcheck
// -exec-pools` (aquery only, no cluster) checks every row's placement of
// compile, link and test actions with cucinactl's flags.
func xplatPrecheck(c *harness.Context) error {
	xplatcheckRun.Do(func() {
		_ = c.Step("offline pre-check: xplatcheck -exec-pools", func() error {
			xplatcheckRun.summary, xplatcheckRun.err = runXplatcheck(c)
			return xplatcheckRun.err
		})
	})
	c.Check(harness.CheckResult{Name: "offline pre-check: every configuration resolves as specified (xplatcheck -exec-pools)", Kind: "precheck",
		Pass: xplatcheckRun.err == nil, Detail: xplatcheckRun.summary})
	if xplatcheckRun.err != nil {
		return harness.Fail("offline pre-check failed: %v", xplatcheckRun.err)
	}
	return nil
}

func runXplatcheck(c *harness.Context) (string, error) {
	bz := "bazel"
	if c.Env.DevMac != nil && c.Env.DevMac.Bazel != "" {
		bz = c.Env.DevMac.Bazel
	}
	ctx, cancel := context.WithTimeout(c, time.Hour)
	defer cancel()
	logPath := filepath.Join(c.Dir(), "xplatcheck.log")
	f, err := os.Create(logPath)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, bz, "run", "//tools/xplat/cmd/xplatcheck", "--", "-workdir", xplatcheckWorkdir, "-exec-pools", "-bazel", bz)
	cmd.Dir, cmd.Stdout, cmd.Stderr = c.Env.RepoDir, f, f
	runErr := cmd.Run()
	_ = f.Close()
	b, err := os.ReadFile(logPath)
	if err != nil {
		return "", err
	}
	summary, failures := xplatcheckSummary(string(b))
	if runErr != nil {
		return summary, fmt.Errorf("%s: %w%s", summary, runErr, failures)
	}
	return summary, nil
}

// xplatcheckSummary returns xplatcheck's verdict line and its failed rows.
func xplatcheckSummary(out string) (summary, failures string) {
	var failed []string
	for _, l := range strings.Split(out, "\n") {
		switch l = strings.TrimSpace(l); {
		case strings.HasPrefix(l, "FAIL "), strings.HasPrefix(l, "ERROR "):
			f := strings.Fields(l)
			failed = append(failed, strings.Join(f[:min(2, len(f))], " "))
		case strings.HasSuffix(l, "resolve as specified"), strings.HasSuffix(l, "configurations failed"):
			summary = l
		}
	}
	if summary == "" {
		summary = lastLine(out)
	}
	if len(failed) > 0 {
		failures = ": " + strings.Join(failed, ", ")
	}
	return summary, failures
}

// windowsTestOverlayScript builds the @bazel_tools overlay (cross tooling).
const windowsTestOverlayScript = "tools/xplat/windows-test-overlay.sh"

// runWindowsTestOverlay runs windows-test-overlay.sh on a non-Windows client
// in its Abseil checkout with its Bazel and returns the line to add.
func runWindowsTestOverlay(c *harness.Context, lr *laneRun) (string, error) {
	h := lr.host
	if h.OS() == remote.Windows {
		return "", fmt.Errorf("a Windows client needs no @bazel_tools overlay")
	}
	script := filepath.Join(c.Env.RepoDir, filepath.FromSlash(windowsTestOverlayScript))
	if _, err := os.Stat(script); err != nil {
		return "", err
	}
	dst := script
	if h.Name() != infra.DevMac {
		dst = hjoin(h, h.WorkDir(), "windows-test-overlay.sh")
		if err := h.Put(c, script, dst); err != nil {
			return "", err
		}
	}
	bz := bazelBinary(c.Env, h)
	if bz == "" {
		bz = "bazel"
	}
	out := hjoin(h, h.WorkDir(), "bazel_tools_overlay")
	res, err := h.Run(c, fmt.Sprintf("cd %s && BAZEL=%s bash %s %s", quoteFor(h, lr.ws), quoteFor(h, bz), quoteFor(h, dst), quoteFor(h, out)), remote.Opts{User: lr.user})
	if err != nil {
		return "", err
	}
	if err := res.Err(); err != nil {
		return "", fmt.Errorf("%s: %w", windowsTestOverlayScript, err)
	}
	return parseOverlayLine(string(res.Stdout))
}

// parseOverlayLine re-renders the `--override_repository=bazel_tools=<dir>`
// line windows-test-overlay.sh prints last (quoted for the rc tokenizer).
func parseOverlayLine(stdout string) (string, error) {
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if dir, ok := strings.CutPrefix(strings.TrimSpace(lines[i]), "common --override_repository=bazel_tools="); ok && dir != "" {
			return "common " + bazelrun.RCQuote("--override_repository=bazel_tools="+dir), nil
		}
	}
	return "", fmt.Errorf("%s printed no --override_repository=bazel_tools line", windowsTestOverlayScript)
}

// ---------------------------------------------------------------- the matrix runner

// StepResult is one invocation of a configuration.
type StepResult struct {
	Name             string        `json:"name"`
	ExitCode         int           `json:"exitCode"`
	Wall             time.Duration `json:"wall"`
	Spawns           int           `json:"spawns"`
	RemoteExecutions int           `json:"remoteExecutions"`
	RemoteCacheHits  int           `json:"remoteCacheHits"`
}

// ConfigResult is the per-configuration record (NFR-X1/X2/X3/X4/X5).
type ConfigResult struct {
	Config     string `json:"config"`
	Target     string `json:"target"`
	Client     string `json:"client"`
	ExecPool   string `json:"execPool"`
	Coverage   string `json:"coverage"`
	TestStep   string `json:"testStep"`
	TestRunner string `json:"testRunner,omitempty"` // pool/runner (mode)
	// Expected REAPI property sets (NFR-X1).
	CompileProps map[string]string `json:"compileProps"`
	TestProps    map[string]string `json:"testProps,omitempty"`
	ExitCode     int               `json:"exitCode"`
	Wall         time.Duration     `json:"wall"`
	Steps        []StepResult      `json:"steps,omitempty"`
	Routing      *execlog.Routing  `json:"routing,omitempty"`
	Tests        map[string]string `json:"tests,omitempty"`
	RemoteRatio  float64           `json:"remoteRatio"`
	HitRatio     float64           `json:"hitRatio"`
	// NetworkBytesSent is the client's upload (BEP), summed over steps (NFR-X5).
	NetworkBytesSent int64 `json:"networkBytesSent"`
	// RCIssues are `cucinactl bazelrc --cross` contract violations; RCAdded
	// the lines the harness appended (configuration, overlay, fixes).
	RCIssues []string `json:"rcIssues,omitempty"`
	RCAdded  []string `json:"rcAdded,omitempty"`
	Error    string   `json:"error,omitempty"`
}

func newConfigResult(cfg Config) ConfigResult {
	r := ConfigResult{Config: cfg.Name, Target: cfg.Target.Name, Client: cfg.Client.Host, ExecPool: cfg.ExecPool,
		Coverage: cfg.Target.Coverage, TestStep: cfg.TestStep}
	if ts := cfg.Target.Test; ts != nil {
		r.TestRunner = ts.Pool + "/" + ts.Runner + " (" + ts.Mode + ")"
	}
	return r
}

func safeName(config string) string { return strings.NewReplacer("@", "-", ".", "_").Replace(config) }

// matrixLane is one client host of the matrix, opened once.
type matrixLane struct {
	open    sync.Once
	lr      *laneRun
	err     error
	overlay struct {
		sync.Once
		line string
		err  error
	}
}

// matrix runs configurations in concurrent batches, each in its own output
// base on its client, and records one ConfigResult per configuration.
type matrix struct {
	c       *harness.Context
	targets *Targets
	props   RunnerProps
	mu      sync.Mutex
	lanes   map[string]*matrixLane
}

func newMatrix(c *harness.Context) (*matrix, error) {
	t, err := loadTargets(c.Env)
	if err != nil {
		return nil, err
	}
	props, err := runnerProps(c.Env)
	if err != nil {
		return nil, err
	}
	return &matrix{c: c, targets: t, props: props, lanes: map[string]*matrixLane{}}, nil
}

// lane opens a client for cross configurations: its Abseil checkout gets
// the hermetic-llvm overlay whatever the client's native lane uses.
func (m *matrix) lane(l Lane) (*matrixLane, error) {
	m.mu.Lock()
	ml, ok := m.lanes[l.Host]
	if !ok {
		ml = &matrixLane{}
		m.lanes[l.Host] = ml
	}
	m.mu.Unlock()
	ml.open.Do(func() {
		l.Toolchain = bazelrun.HermeticLLVM
		ml.lr, ml.err = openLane(m.c, l)
	})
	return ml, ml.err
}

// windowsTestOverlay runs the overlay script once per non-Windows client,
// before its first Windows-target configuration.
func (m *matrix) windowsTestOverlay(ml *matrixLane) (string, error) {
	ml.overlay.Do(func() {
		ml.overlay.err = m.c.Step(ml.lr.lane.Name+": @bazel_tools overlay for Windows tests (windows-test-overlay.sh)", func() error {
			var err error
			ml.overlay.line, err = runWindowsTestOverlay(m.c, ml.lr)
			return err
		})
	})
	return ml.overlay.line, ml.overlay.err
}

// crossRC returns `cucinactl bazelrc --cross --target <t> [--exec-pool <p>]`
// (the pool only when it is not the target's default).
func (m *matrix) crossRC(lr *laneRun, cfg Config) (string, error) {
	args := "bazelrc --ci --disk-cache=none --helper-path " + argFor(lr.host, credentialHelper(lr.host)) + " --cross --target " + cfg.Target.Name
	if cfg.ExecPool != cfg.Target.ExecPlatforms[0] {
		args += " --exec-pool " + cfg.ExecPool
	}
	res, err := m.cucinactl(lr, args)
	if err != nil {
		return "", err
	}
	return string(res.Stdout), res.Err()
}

func (m *matrix) cucinactl(lr *laneRun, args string) (remote.Result, error) {
	cli := hjoin(lr.host, lr.host.WorkDir(), "bin", "cucinactl")
	if lr.host.OS() == remote.Windows {
		cli += ".exe"
	}
	if lr.host.Name() == infra.DevMac {
		svc, err := infra.Of(m.c)
		if err != nil {
			return remote.Result{}, err
		}
		if cli, err = svc.CucinactlPath(); err != nil {
			return remote.Result{}, err
		}
	}
	return lr.host.Run(m.c, quoteFor(lr.host, cli)+" "+args, remote.Opts{User: lr.user})
}

// rejectExcluded checks UC27's fast failure on the client: `cucinactl
// bazelrc --cross` refuses every excluded target with the row's reason.
func (m *matrix) rejectExcluded(l Lane) error {
	ml, err := m.lane(l)
	if err != nil {
		return err
	}
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	for _, tg := range m.targets.Targets {
		if !tg.Excluded {
			continue
		}
		res, err := m.cucinactl(ml.lr, "bazelrc --cross --target "+tg.Name)
		if err != nil {
			return err
		}
		out := norm(string(res.Stdout) + " " + string(res.Stderr))
		m.c.Check(harness.CheckResult{Name: "cucinactl bazelrc --cross rejects excluded target " + tg.Name + " with its reason (UC27)", Kind: "cli",
			Pass: res.ExitCode != 0 && strings.Contains(out, norm(tg.Reason)), Detail: fmt.Sprintf("exit %d: %.300s", res.ExitCode, out)})
	}
	return nil
}

func (m *matrix) run(cfgs []Config, batch int) []ConfigResult {
	results := make([]ConfigResult, len(cfgs))
	sem := make(chan struct{}, max(1, batch))
	var wg sync.WaitGroup
	for i, cfg := range cfgs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = m.one(cfg)
		}()
	}
	wg.Wait()
	return results
}

func (m *matrix) one(cfg Config) ConfigResult {
	r := newConfigResult(cfg)
	want, err := m.props.expectation(m.targets, cfg)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	r.CompileProps, r.TestProps = want.Compile, want.Test
	ml, err := m.lane(cfg.Client)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	lr, h := ml.lr, ml.lr.host
	raw, err := m.crossRC(lr, cfg)
	if err != nil {
		r.Error = "cucinactl bazelrc --cross: " + err.Error()
		return r
	}
	rep := rcContract(m.targets, cfg, h.OS(), raw)
	r.RCIssues = rep.Issues
	if rep.Fatal {
		r.Error = "cucinactl bazelrc --cross: " + strings.Join(rep.Issues, "; ")
		return r
	}
	var overlay string
	if needsTestOverlay(cfg, h.OS()) {
		if overlay, err = m.windowsTestOverlay(ml); err != nil {
			r.Error = "@bazel_tools overlay: " + err.Error()
			return r
		}
	}
	rc, added := composeRC(raw, cfg, overlay, rep.Fix)
	r.RCAdded = added
	safe := safeName(cfg.Name)
	rcName := "cucina-" + safe + ".bazelrc"
	local := filepath.Join(m.c.Dir(), rcName)
	if err := os.WriteFile(local, []byte(rc), 0o644); err != nil {
		r.Error = err.Error()
		return r
	}
	if err := h.Put(m.c, local, hjoin(h, lr.ws, rcName)); err != nil {
		r.Error = err.Error()
		return r
	}
	base := bazelrun.Invocation{
		Host: h, Workspace: lr.ws, User: lr.user, Bazel: bazelBinary(m.c.Env, h),
		Startup: []string{"--bazelrc=" + hjoin(h, lr.ws, rcName), "--output_base=" + hjoin(h, h.WorkDir(), "ob", safe)},
	}
	if h.OS() == remote.Windows {
		// docs/cross-compilation.md (Windows client): Bazel takes the shell
		// from the exec platform only while BAZEL_SH is unset; the client
		// image sets it machine-wide for the native MSVC lane.
		base.Unset = []string{"BAZEL_SH"}
	}
	// A fresh output base: every spawn of the configuration reaches its
	// execution log (no local action-cache hits from an earlier attempt).
	clean := base
	clean.Name, clean.Command, clean.Args = m.c.Scenario.ID+"-"+safe+"-expunge", "clean", []string{"--expunge"}
	co, err := bazelrun.Run(m.c, clean, filepath.Join(m.c.Dir(), safe+"-expunge"))
	if err == nil {
		err = mustSucceed(co, "clean --expunge")
	}
	if err != nil {
		r.Error = err.Error()
		return r
	}
	var spawns, remoteExec, hits int
	for _, st := range cfg.Steps {
		inv := base
		inv.Name, inv.Command, inv.Collect = m.c.Scenario.ID+"-"+safe+"-"+st.Name, st.Command, true
		// Concurrent servers share the checkout: never race on MODULE.bazel.lock.
		inv.Args = append(append(append([]string{"--lockfile_mode=off"}, st.Flags...), "--"), st.Patterns...)
		o, err := bazelrun.Run(m.c, inv, filepath.Join(m.c.Dir(), safe+"-"+st.Name))
		if err != nil {
			r.Error = st.Name + ": " + err.Error()
			break
		}
		recordOutcome(m.c, "config."+safe+"."+st.Name, o)
		sr := StepResult{Name: st.Name, ExitCode: o.ExitCode, Wall: o.Wall}
		r.Wall += o.Wall
		if o.ExitCode != 0 && r.ExitCode == 0 {
			r.ExitCode = o.ExitCode
		}
		if s := o.ExecLog; s != nil {
			sr.Spawns, sr.RemoteExecutions, sr.RemoteCacheHits = s.Spawns, s.RemoteExecutions, s.RemoteCacheHits
			spawns, remoteExec, hits = spawns+s.Spawns, remoteExec+s.RemoteExecutions, hits+s.RemoteCacheHits
		}
		if b := o.BEP; b != nil {
			r.NetworkBytesSent += int64(b.NetworkBytesSent)
			if st.Command == "test" {
				if r.Tests == nil {
					r.Tests = map[string]string{}
				}
				for _, t := range b.Tests {
					r.Tests[t.Label] = t.Status
				}
			}
		}
		if o.ExecLogPath != "" {
			if l, err := execlog.ReadFile(o.ExecLogPath); err == nil {
				rt := l.RouteCheck(want.Compile, want.Test)
				if r.Routing == nil {
					r.Routing = &execlog.Routing{}
				}
				r.Routing.Merge(rt)
			} else {
				r.Error = "execution log evidence: " + err.Error()
			}
		} else {
			r.Error = "missing compact execution log evidence"
		}
		if st.Command == "test" && len(r.Tests) == 0 {
			r.Error = "test invocation produced no BEP test outcomes"
		}
		r.Steps = append(r.Steps, sr)
	}
	if spawns > 0 {
		r.RemoteRatio = float64(remoteExec+hits) / float64(spawns)
		r.HitRatio = float64(hits) / float64(spawns)
	}
	return r
}

// recordMatrix stores the plan, the per-configuration records and the NFR
// rows, and returns the failures.
func recordMatrix(c *harness.Context, results []ConfigResult, start time.Time) []string {
	c.Record("configurations", results)
	var failed []string
	for _, r := range results {
		switch {
		case r.Error != "":
			failed = append(failed, r.Config+": "+r.Error)
			continue
		case r.ExitCode == 3:
			failed = append(failed, fmt.Sprintf("%s: tests failed (bazel exit 3; outcomes in deviations/NFR-X2)", r.Config))
		case r.ExitCode != 0:
			failed = append(failed, fmt.Sprintf("%s: bazel exited %d", r.Config, r.ExitCode))
		}
		if len(r.RCIssues) > 0 {
			failed = append(failed, r.Config+": cucinactl bazelrc --cross: "+strings.Join(r.RCIssues, "; "))
		}
		if rt := r.Routing; rt != nil {
			addNFRs(c, nfr.Routing(r.Config, rt.CompileLinkOnPool, rt.CompileLinkTotal, rt.TestOnRunner, rt.TestTotal, r.TestStep != TestStepNA))
		}
		c.NFR(nfr.Reported("NFR-X4", r.Config, r.Wall.Seconds(), "s", "test step: "+r.TestStep))
	}
	c.NFR(nfr.Reported("NFR-X4", "whole matrix (concurrent batches)", c.Now().Sub(start).Seconds(), "s", fmt.Sprintf("%d configurations", len(results))))
	return failed
}

// planRecord is the readable plan of a matrix run.
func planRecord(cfgs []Config) []map[string]any {
	out := make([]map[string]any, len(cfgs))
	for i, cfg := range cfgs {
		runner := ""
		if ts := cfg.Target.Test; ts != nil {
			runner = ts.Pool + "/" + ts.Runner + " (" + ts.Mode + ")"
		}
		out[i] = map[string]any{"config": cfg.Name, "execPool": cfg.ExecPool, "coverage": cfg.Target.Coverage,
			"testStep": cfg.TestStep, "testRunner": runner, "steps": cfg.Steps}
	}
	return out
}

// runMatrix records the plan, runs the configurations while sampling the
// instance inventory, and records the results.
func runMatrix(c *harness.Context, m *matrix, cfgs []Config, batch int) ([]ConfigResult, []string, error) {
	c.Record("plan", planRecord(cfgs))
	start := c.Now()
	var results []ConfigResult
	if _, err := sampled(c, 15*time.Second, func() error {
		results = m.run(cfgs, batch)
		return nil
	}); err != nil {
		return nil, nil, err
	}
	return results, recordMatrix(c, results, start), nil
}

// ---------------------------------------------------------------- T16

func t16() *harness.Scenario {
	return &harness.Scenario{
		ID: "T16", Title: "Cross matrix from linux-client: every R-XPLAT-1 row, configurations in concurrent batches",
		Requires: []harness.Requirement{harness.RequiresAWS, harness.RequiresLinuxClient, harness.RequiresCrossMatrix, harness.RequiresCucinactl},
		Cost:     harness.CostHigh, EstimateUSD: 60, MaxInstances: 12, Essential: true, Timeout: 10 * time.Hour,
		DependsOn: []string{"T1"}, NFRs: []string{"NFR-X1", "NFR-X2", "NFR-X4", "NFR-X5"},
		Post: Guards,
		Run: func(c *harness.Context) error {
			if err := xplatPrecheck(c); err != nil {
				return err
			}
			m, err := newMatrix(c)
			if err != nil {
				return err
			}
			if err := m.rejectExcluded(LinuxLane); err != nil {
				return err
			}
			cfgs, err := plan(m.targets, LinuxLane, ScopeCoverage, nil)
			if err != nil {
				return err
			}
			results, failed, err := runMatrix(c, m, cfgs, 4)
			if err != nil {
				return err
			}
			outcomesVsBaseline(c, results)
			toolchainUploads(c, results)
			if len(failed) > 0 {
				return harness.Fail("%d configuration problems: %s", len(failed), strings.Join(failed, "; "))
			}
			return nil
		},
	}
}

// baselineLane is the local baseline whose outcomes a target's remote
// outcomes must equal (NFR-X2: wherever the target runs locally).
func baselineLane(target string) string {
	switch target {
	case "x86_64-linux-gnu", "x86_64-linux-musl":
		return "linux"
	case "x86_64-windows-msvc", "x86_64-windows-gnu":
		return "windows"
	case "aarch64-apple-darwin":
		return "macos"
	}
	return ""
}

// outcomesVsBaseline evaluates NFR-X2 for configurations whose target runs
// locally on a campaign client: the local baseline outcomes come from the
// baseline-<lane> scenario results (label → status). Root causes for
// deviations are recorded by the lead (docs/reports/issues.md) and
// re-attached by the report generator.
func outcomesVsBaseline(c *harness.Context, results []ConfigResult) {
	locals := map[string]map[string]string{}
	for lane, key := range map[string]string{"linux": "baseline.linux-client.tests", "windows": "baseline.windows-client.tests", "macos": "baseline.dev-mac.tests"} {
		if p, ok := c.Prior["baseline-"+lane]; ok {
			if raw, ok := p.Values[key]; ok {
				b, _ := json.Marshal(raw)
				var m map[string]string
				if json.Unmarshal(b, &m) == nil {
					locals[lane] = m
				}
			}
		}
	}
	for _, r := range results {
		local := locals[baselineLane(r.Target)]
		if local == nil || r.Tests == nil {
			continue
		}
		res, devs := nfr.Outcomes(r.Config, r.Tests, local, nil)
		c.NFR(res)
		if len(devs) > 0 {
			c.Record("deviations."+r.Config, devs)
		}
	}
}

// toolchainUploads evaluates NFR-X5 from the BEP network counters: after the
// first configuration of a toolchain, later configurations must not upload
// the toolchain again (bytes sent stay small).
func toolchainUploads(c *harness.Context, results []ConfigResult) {
	c.Check(harness.CheckResult{Name: "NFR-X5 per-toolchain-version upload attribution", Kind: "coverage", Skipped: "BEP totals do not identify toolchain/SDK/CRT blobs; per-version upload attribution is not implemented (ADR 1006). Totals below are diagnostics, not proof."})
	var sent []float64
	for _, r := range results {
		if r.Error == "" && r.NetworkBytesSent > 0 {
			sent = append(sent, float64(r.NetworkBytesSent))
		}
	}
	if len(sent) < 2 {
		return
	}
	sort.Float64s(sent)
	c.NFR(nfr.Reported("NFR-X5", "client upload bytes per configuration", sent[len(sent)/2]/1e6, "MB (median)",
		fmt.Sprintf("max %.0f MB; toolchains come from the repo contents cache and are uploaded once per toolchain version", sent[len(sent)-1]/1e6)))
}

// ---------------------------------------------------------------- T17

func t17() *harness.Scenario {
	return &harness.Scenario{
		ID: "T17", Title: "Exec coverage: x86_64-linux-gnu compiled on each non-default exec pool (Linux arm64, Windows x86_64, macOS arm64 Xcode); tests stay on linux-x86-64",
		Requires: []harness.Requirement{harness.RequiresAWS, harness.RequiresLinuxClient, harness.RequiresCrossMatrix, harness.RequiresCucinactl},
		Cost:     harness.CostMedium, EstimateUSD: 8, MaxInstances: 6, Essential: true, Timeout: 4 * time.Hour,
		NFRs: []string{"NFR-X1"}, Post: Guards,
		Run: func(c *harness.Context) error {
			if err := xplatPrecheck(c); err != nil {
				return err
			}
			m, err := newMatrix(c)
			if err != nil {
				return err
			}
			cfgs, err := execCoverage(m.targets, LinuxLane)
			if err != nil {
				return err
			}
			if _, failed, err := runMatrix(c, m, cfgs, 3); err != nil {
				return err
			} else if len(failed) > 0 {
				return harness.Fail("%s", strings.Join(failed, "; "))
			}
			return nil
		},
	}
}

// execCoverage is T17's rows (xplatcheck -exec-pools): x86_64-linux-gnu with
// each non-default compile exec platform first; the test placement does not
// change (UC25), so the smoke tests still run on linux-x86-64/native.
func execCoverage(t *Targets, client Lane) ([]Config, error) {
	tg, ok := t.target("x86_64-linux-gnu")
	if !ok || tg.Excluded || len(tg.ExecPlatforms) < 2 {
		return nil, harness.Fail("platforms/targets.json: x86_64-linux-gnu with non-default exec platforms is required")
	}
	var cfgs []Config
	for _, ep := range tg.ExecPlatforms[1:] {
		cfg, err := planConfig(tg, client, ep, ScopeSmoke)
		if err != nil {
			return nil, err
		}
		cfgs = append(cfgs, cfg)
	}
	return cfgs, nil
}

// ---------------------------------------------------------------- T18

func t18() *harness.Scenario {
	return &harness.Scenario{
		ID: "T18", Title: "Cross-host cache: rebuild two configurations from the dev Mac and windows-client after linux-client built them",
		Requires: []harness.Requirement{harness.RequiresAWS, harness.RequiresWindowsClient, harness.RequiresMacHost, harness.RequiresCrossMatrix, harness.RequiresCucinactl},
		Cost:     harness.CostMedium, EstimateUSD: 4, Essential: true, Timeout: 4 * time.Hour, DependsOn: []string{"T16"},
		NFRs: []string{"NFR-X3", "NFR-X1"}, Post: Guards,
		Run: func(c *harness.Context) error {
			if err := xplatPrecheck(c); err != nil {
				return err
			}
			m, err := newMatrix(c)
			if err != nil {
				return err
			}
			want := map[string]bool{"aarch64-apple-darwin": true, "x86_64-windows-msvc": true}
			var cfgs []Config
			for _, client := range []Lane{MacLane, WindowsLane} {
				cs, err := plan(m.targets, client, ScopeCoverage, func(t Target) bool { return want[t.Name] })
				if err != nil {
					return err
				}
				cfgs = append(cfgs, cs...)
			}
			results, failed, err := runMatrix(c, m, cfgs, 4)
			if err != nil {
				return err
			}
			for _, r := range results {
				if r.Error == "" {
					c.NFR(nfr.CacheHits("NFR-X3", r.Config, r.HitRatio))
				}
			}
			if len(failed) > 0 {
				return harness.Fail("%s", strings.Join(failed, "; "))
			}
			return nil
		},
	}
}

// ---------------------------------------------------------------- T19

func t19() *harness.Scenario {
	return &harness.Scenario{
		ID: "T19", Title: "Windows client cross pattern: a Linux target and a Windows MinGW target, compiled on Linux, tested on their runners (no @bazel_tools overlay)",
		Requires: []harness.Requirement{harness.RequiresAWS, harness.RequiresWindowsClient, harness.RequiresCrossMatrix, harness.RequiresCucinactl},
		Cost:     harness.CostMedium, EstimateUSD: 6, Essential: true, Timeout: 4 * time.Hour, DependsOn: []string{"T16"},
		NFRs: []string{"NFR-X1", "NFR-X2"}, Post: Guards,
		Run: func(c *harness.Context) error {
			if err := xplatPrecheck(c); err != nil {
				return err
			}
			m, err := newMatrix(c)
			if err != nil {
				return err
			}
			// The pattern of docs/cross-compilation.md (openai/codex#20585):
			// `cucinactl bazelrc --cross` on Windows, default compile pool
			// (Linux), tests on each target's runner; Windows Bazel embeds
			// tw.exe, so no overlay, and BAZEL_SH stays unset.
			want := map[string]bool{"x86_64-linux-gnu": true, "x86_64-windows-gnu": true}
			cfgs, err := plan(m.targets, WindowsLane, ScopeCoverage, func(t Target) bool { return want[t.Name] })
			if err != nil {
				return err
			}
			results, failed, err := runMatrix(c, m, cfgs, 2)
			if err != nil {
				return err
			}
			// Outcomes must match T16's for the same targets (NFR-X2); the
			// cache hits against T16 show the Windows-client lines at work.
			if p, ok := c.Prior["T16"]; ok {
				b, _ := json.Marshal(p.Values["configurations"])
				var prior []ConfigResult
				_ = json.Unmarshal(b, &prior)
				for _, r := range results {
					for _, o := range prior {
						if o.Target == r.Target && o.ExecPool == r.ExecPool && o.Tests != nil && r.Tests != nil {
							res, _ := nfr.Outcomes(r.Config+" vs T16", r.Tests, o.Tests, nil)
							c.NFR(res)
						}
					}
					if r.Error == "" {
						c.Metric("config."+safeName(r.Config)+".hit_ratio_after_t16", r.HitRatio, "ratio")
					}
				}
			}
			if len(failed) > 0 {
				return harness.Fail("%s", strings.Join(failed, "; "))
			}
			return nil
		},
	}
}
