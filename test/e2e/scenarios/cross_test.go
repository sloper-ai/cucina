// SPDX-License-Identifier: FSL-1.1-ALv2

package scenarios

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/test/e2e/bazelrun"
	"github.com/sloper-ai/cucina/test/e2e/remote"
)

// The catalog under test: `bazel test` passes the runfiles paths
// (runfiles_env in BUILD.bazel); `go test` reads the source tree.
var (
	targetsJSON = fileOr("E2E_TARGETS_JSON", "../../../platforms/targets.json")
	poolsJSON   = fileOr("E2E_POOLS_JSON", "../../../platforms/pools.json")
)

func fileOr(env, fallback string) string {
	if p := os.Getenv(env); p != "" {
		return p
	}
	return fallback
}

func catalog(t *testing.T) (*Targets, RunnerProps) {
	t.Helper()
	b, err := os.ReadFile(targetsJSON)
	require.NoError(t, err)
	targets, err := parseTargets(b)
	require.NoError(t, err)
	b, err = os.ReadFile(poolsJSON)
	require.NoError(t, err)
	props, err := parsePools(b)
	require.NoError(t, err)
	return targets, props
}

func byName(cfgs []Config) map[string]Config {
	out := map[string]Config{}
	for _, c := range cfgs {
		out[c.Target.Name] = c
	}
	return out
}

// Guards the T16 matrix planning against the real platforms/targets.json
// (R-XPLAT-1): one configuration per in-scope row, the coverage of each row
// turned into invocations (full suite; full build + smoke tests with qemu's
// scaled timeouts; build-only examples whose test step is "not applicable"),
// excluded rows rejected with their reason, and the expected runners
// resolved to exact pools.json property sets — macOS-target tests on the
// Xcode runner (ADR 0903), emulated tests on the qemu runners.
func TestCrossMatrixPlanFollowsTargetsJSON(t *testing.T) {
	targets, props := catalog(t)
	cfgs, err := plan(targets, LinuxLane, ScopeCoverage, nil)
	require.NoError(t, err)
	inScope := 0
	for _, tg := range targets.Targets {
		if tg.Excluded {
			_, err := planConfig(tg, LinuxLane, "", ScopeCoverage)
			require.ErrorContains(t, err, tg.Reason)
			continue
		}
		inScope++
	}
	require.Len(t, cfgs, inScope)
	require.GreaterOrEqual(t, inScope, 17)

	for _, cfg := range cfgs {
		tg := cfg.Target
		require.Equal(t, tg.Name+"@linux-client", cfg.Name)
		require.Equal(t, tg.ExecPlatforms[0], cfg.ExecPool, "default compile pool first (UC25)")
		want, err := props.expectation(targets, cfg)
		require.NoError(t, err, cfg.Name)
		require.NotEmpty(t, want.Compile, cfg.Name)
		switch tg.Coverage {
		case CoverageFull:
			require.Equal(t, []Step{{Name: "test", Command: "test", Patterns: bazelrun.AbseilTargets}}, cfg.Steps, cfg.Name)
			require.Equal(t, TestStepFull, cfg.TestStep)
		case CoverageSmoke:
			require.Len(t, cfg.Steps, 2, cfg.Name)
			require.Equal(t, Step{Name: "build", Command: "build", Patterns: bazelrun.AbseilTargets}, cfg.Steps[0])
			require.Equal(t, smokePatterns, cfg.Steps[1].Patterns)
			require.Equal(t, "qemu-user", tg.Test.Mode, cfg.Name)
			require.Equal(t, []string{"--test_timeout=600,3000,9000,36000"}, cfg.Steps[1].Flags, cfg.Name)
			require.Equal(t, "qemu", want.Test["cucina-emulation"], cfg.Name)
			require.Equal(t, TestStepSmoke, cfg.TestStep)
		case CoverageExample:
			require.Equal(t, []Step{{Name: "build", Command: "build", Patterns: bazelrun.ExamplePatterns}}, cfg.Steps, cfg.Name)
			require.Equal(t, TestStepNA, cfg.TestStep)
			require.Nil(t, want.Test, cfg.Name)
		}
		if cfg.TestStep != TestStepNA {
			require.Equal(t, props[tg.Test.Pool][tg.Test.Runner], want.Test, cfg.Name)
		}
	}

	m := byName(cfgs)
	for _, name := range []string{"x86_64-linux-gnu", "aarch64-linux-gnu", "x86_64-windows-msvc", "x86_64-windows-gnu", "riscv64-linux-gnu", "s390x-linux-musl", "armv7-linux-gnueabihf", "wasm32-unknown-unknown", "bpfel"} {
		require.Contains(t, m, name)
	}
	mac, err := props.expectation(targets, m["aarch64-apple-darwin"])
	require.NoError(t, err)
	require.Equal(t, props["macos-arm64-xcode27.0"]["xcode"], mac.Test, "macOS-target tests run on the Xcode runner (ADR 0903)")
	require.NotEqual(t, props["macos-arm64-xcode27.0"]["generic"], mac.Test)
	require.Equal(t, mac.Compile, mac.Test)
	win, err := props.expectation(targets, m["x86_64-windows-msvc"])
	require.NoError(t, err)
	require.Equal(t, map[string]string{"OSFamily": "linux", "ISA": "x86-64"}, win.Compile, "Windows targets compile on Linux by default")
	require.Equal(t, map[string]string{"OSFamily": "windows", "ISA": "x86-64"}, win.Test)
}

// Guards T17 (UC25, xplatcheck -exec-pools): x86_64-linux-gnu with each
// non-default compile pool first; the tests stay on linux-x86-64/native.
func TestExecCoverageRows(t *testing.T) {
	targets, props := catalog(t)
	cfgs, err := execCoverage(targets, LinuxLane)
	require.NoError(t, err)
	tg, _ := targets.target("x86_64-linux-gnu")
	require.Len(t, cfgs, len(tg.ExecPlatforms)-1)
	for i, cfg := range cfgs {
		require.Equal(t, tg.ExecPlatforms[i+1], cfg.ExecPool)
		require.Equal(t, "x86_64-linux-gnu@linux-client@exec-"+cfg.ExecPool, cfg.Name)
		require.Equal(t, TestStepSmoke, cfg.TestStep)
		want, err := props.expectation(targets, cfg)
		require.NoError(t, err)
		ep, _ := targets.execPlatform(cfg.ExecPool)
		require.Equal(t, props[ep.Pool][ep.Runner], want.Compile)
		require.Equal(t, props["linux-x86-64"]["native"], want.Test)
	}
	_, err = planConfig(tg, LinuxLane, "no-such-pool", ScopeSmoke)
	require.ErrorContains(t, err, "cannot compile")
}

// goodRC renders what docs/cross-compilation.md documents for a
// configuration (the shape of `cucinactl bazelrc --cross`).
func goodRC(t *testing.T, targets *Targets, cfg Config, clientOS string) string {
	t.Helper()
	ep, ok := targets.execPlatform(cfg.ExecPool)
	require.True(t, ok)
	execs := []string{ep.Label}
	var flags []string
	for _, n := range cfg.Target.ExecPlatforms {
		e, _ := targets.execPlatform(n)
		if n != ep.Name {
			execs = append(execs, e.Label)
		}
		flags = append(flags, e.Flags...)
	}
	if cfg.Target.Test != nil {
		execs = append(execs, cfg.Target.Test.Label)
	}
	lines := []string{
		"# Generated by cucinactl",
		"startup --experimental_remote_repo_contents_cache",
		"build --remote_executor=grpcs://cucina.example.test:443",
		"build --remote_cache=grpcs://cucina.example.test:443",
		"build --remote_instance_name=main",
		"build --credential_helper=cucina.example.test=/opt/cucinactl",
		"build --extra_execution_platforms=" + strings.Join(execs, ","),
		"build --host_platform=" + ep.Label,
		"build --platforms=" + cfg.Target.Platform,
		"build --experimental_platform_in_output_dir",
	}
	for _, f := range flags {
		lines = append(lines, "build "+f)
	}
	if cfg.Target.ABI == "msvc" {
		lines = append(lines, "build --repo_env=BAZEL_MSVC_RUNTIME_VISUAL_STUDIO_EULA=1", "build --repo_env=BAZEL_WINDOWS_SDK_EULA=1")
	}
	if cfg.Target.OS == "windows" && cfg.Target.Test != nil {
		lines = append(lines, `build '--test_env=SYSTEMROOT=C:\Windows'`,
			`build '--test_env=PATH=C:\Windows\System32;C:\Windows;C:\Windows\System32\Wbem;C:\Windows\System32\WindowsPowerShell\v1.0'`)
	}
	if f := testTimeouts(cfg.Target.TestTimeoutScale); f != "" {
		lines = append(lines, "build "+f)
	}
	if clientOS == remote.Windows {
		lines = append(lines, "startup --windows_enable_symlinks", "build --action_env=PATH=/bin:/usr/bin:/usr/local/bin",
			"build --host_action_env=PATH=/bin:/usr/bin:/usr/local/bin", "build --enable_runfiles")
	}
	lines = append(lines, "build --remote_cache_compression", "build --remote_download_outputs=toplevel", "build --jobs=200",
		"build --remote_retries=10", "build --remote_retry_max_delay=30s", "build --grpc_keepalive_time=30s", "build --noremote_local_fallback")
	return strings.Join(lines, "\n") + "\n"
}

func without(rc, needle string) string {
	var out []string
	for _, l := range strings.Split(rc, "\n") {
		if !strings.Contains(l, needle) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// Guards the contract between `cucinactl bazelrc --cross` and the matrix
// runner: a documented configuration passes; a wrong placement is fatal (the
// configuration is not run); missing documented lines are issues that the
// harness appends so the run stays as documented.
func TestCrossRCContract(t *testing.T) {
	targets, _ := catalog(t)
	cfgs, err := plan(targets, LinuxLane, ScopeCoverage, nil)
	require.NoError(t, err)
	winCfgs, err := plan(targets, WindowsLane, ScopeCoverage, nil)
	require.NoError(t, err)
	t17, err := execCoverage(targets, LinuxLane)
	require.NoError(t, err)
	for _, cfg := range append(append(cfgs, t17...), winCfgs...) {
		clientOS := remote.Linux
		if cfg.Client.Host == WindowsLane.Host {
			clientOS = remote.Windows
		}
		rep := rcContract(targets, cfg, clientOS, goodRC(t, targets, cfg, clientOS))
		require.False(t, rep.Fatal, cfg.Name)
		require.Empty(t, rep.Issues, cfg.Name)
		require.Empty(t, rep.Fix, cfg.Name)
	}

	m := byName(cfgs)
	msvc := m["x86_64-windows-msvc"]
	rc := goodRC(t, targets, msvc, remote.Linux)
	// Canonical repository spellings are the same labels.
	rep := rcContract(targets, msvc, remote.Linux, strings.ReplaceAll(rc, "@cucina_platforms//", "@@cucina_platforms+//"))
	require.Empty(t, rep.Issues)

	rep = rcContract(targets, msvc, remote.Linux, without(rc, "SYSTEMROOT"))
	require.False(t, rep.Fatal)
	require.Len(t, rep.Issues, 1)
	require.Equal(t, []string{`common '--test_env=SYSTEMROOT=C:\Windows'`}, rep.Fix)

	rep = rcContract(targets, msvc, remote.Linux, without(rc, "EULA"))
	require.Len(t, rep.Fix, 2)

	rep = rcContract(targets, msvc, remote.Linux, strings.Replace(rc, msvc.Target.Platform, m["x86_64-windows-gnu"].Target.Platform, 1))
	require.True(t, rep.Fatal, "a wrong --platforms is fatal")

	gnu := m["x86_64-linux-gnu"]
	grc := goodRC(t, targets, gnu, remote.Linux)
	ep, _ := targets.execPlatform(gnu.ExecPool)
	swapped := strings.Replace(grc, "--extra_execution_platforms="+ep.Label+",", "--extra_execution_platforms=", 1)
	swapped = strings.Replace(swapped, gnu.Target.Test.Label, ep.Label+","+gnu.Target.Test.Label, 1)
	require.True(t, rcContract(targets, gnu, remote.Linux, swapped).Fatal, "the chosen compile platform must come first")
	require.True(t, rcContract(targets, gnu, remote.Linux, without(grc, "--remote_executor")).Fatal)
	rep = rcContract(targets, gnu, remote.Linux, grc+"build --repo_env=BAZEL_WINDOWS_SDK_EULA=1\n")
	require.Equal(t, []string{"--repo_env=BAZEL_WINDOWS_SDK_EULA=1 for a non-MSVC target"}, rep.Issues)
	require.Empty(t, rep.Fix)

	rv := m["riscv64-linux-gnu"]
	rep = rcContract(targets, rv, remote.Linux, without(goodRC(t, targets, rv, remote.Linux), "--test_timeout"))
	require.Equal(t, []string{"common --test_timeout=600,3000,9000,36000"}, rep.Fix)

	onWin := byName(winCfgs)["x86_64-linux-gnu"]
	rep = rcContract(targets, onWin, remote.Windows, goodRC(t, targets, onWin, remote.Linux))
	require.False(t, rep.Fatal)
	require.Len(t, rep.Fix, 4, "the Windows-client lines of a cross configuration")

	macPool := t17[len(t17)-1]
	require.Equal(t, "macos-arm64-xcode27.0", macPool.ExecPool)
	rep = rcContract(targets, macPool, remote.Linux, without(goodRC(t, targets, macPool, remote.Linux), "sdk_version"))
	require.Equal(t, []string{"common --@cucina_platforms//apple:sdk_version=27.0"}, rep.Fix)
}

// Guards what the harness appends: the Abseil configuration with the C++
// dialect of the target's driver (clang-cl for MSVC targets), and the
// @bazel_tools overlay line only for Windows tests from non-Windows clients.
func TestComposeRCAndOverlay(t *testing.T) {
	targets, _ := catalog(t)
	cfgs, err := plan(targets, LinuxLane, ScopeCoverage, nil)
	require.NoError(t, err)
	m := byName(cfgs)
	msvc, gnu, wasm := m["x86_64-windows-msvc"], m["x86_64-windows-gnu"], m["wasm32-unknown-unknown"]
	require.True(t, needsTestOverlay(msvc, remote.Linux))
	require.True(t, needsTestOverlay(gnu, remote.Darwin))
	require.False(t, needsTestOverlay(gnu, remote.Windows), "Windows Bazel embeds tw.exe")
	require.False(t, needsTestOverlay(m["x86_64-linux-gnu"], remote.Linux))
	require.False(t, needsTestOverlay(wasm, remote.Linux))

	line, err := parseOverlayLine("# Windows tests from this Linux client (R-XPLAT-4); add to user.bazelrc:\ncommon --override_repository=bazel_tools=/home/u/e2e/bazel_tools_overlay\n")
	require.NoError(t, err)
	require.Equal(t, "common --override_repository=bazel_tools=/home/u/e2e/bazel_tools_overlay", line)
	line, err = parseOverlayLine("common --override_repository=bazel_tools=/Users/a b/overlay")
	require.NoError(t, err)
	require.Equal(t, "common '--override_repository=bazel_tools=/Users/a b/overlay'", line)
	_, err = parseOverlayLine("windows-test-overlay: no pinned Windows release")
	require.Error(t, err)

	rc, added := composeRC("build --x\n", msvc, line, []string{"common --y"})
	require.Equal(t, []string{"common --config=cross", "common --config=cross-clang-cl", line, "common --y"}, added)
	require.True(t, strings.HasPrefix(rc, "build --x\n\n# --- Added by the Cucina e2e harness"))
	_, added = composeRC("", gnu, "", nil)
	require.Equal(t, []string{"common --config=cross", "common --config=cross-gnu"}, added)
}

// Guards the .bazelrc reading used by the contract check: Bazel's tokenizer
// semantics for quotes, backslashes and comments.
func TestRCTokens(t *testing.T) {
	for line, want := range map[string][]string{
		`build '--test_env=SYSTEMROOT=C:\Windows'`: {"build", `--test_env=SYSTEMROOT=C:\Windows`},
		`common --repository_cache=C:\x`:          {"common", "--repository_cache=C:x"},
		`common "--a=b\\c d"`:                      {"common", `--a=b\c d`},
		`build --x # trailing comment`:             {"build", "--x"},
		`# a comment`:                              nil,
		`build --a=x#y`:                            {"build", "--a=x#y"},
	} {
		require.Equal(t, want, rcTokens(line), line)
	}
}

func TestTestTimeouts(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	require.Empty(t, testTimeouts(nil))
	require.Empty(t, testTimeouts(f(1)))
	require.Equal(t, "--test_timeout=600,3000,9000,36000", testTimeouts(f(10)))
	require.Equal(t, "--test_timeout=150,750,2250,9000", testTimeouts(f(2.5)))
}

// Guards the reading of the offline pre-check's verdict.
func TestXplatcheckSummary(t *testing.T) {
	ok := "INFO: Running command line: bazel-bin/tools/xplat/cmd/xplatcheck/xplatcheck_/xplatcheck\n" +
		"PASS  x86_64-linux-gnu         compile=linux-x86-64                 CppCompile→linux-x86-64-native×2\n" +
		"all 20 configurations resolve as specified\n"
	s, f := xplatcheckSummary(ok)
	require.Equal(t, "all 20 configurations resolve as specified", s)
	require.Empty(t, f)
	bad := "FAIL  aarch64-apple-darwin     compile=macos-arm64-xcode27.0   TestRunner→macos-arm64-xcode27.0-generic×1\n" +
		"      - TestRunner on cucina_platforms//test:x, want the macos-arm64-xcode27.0/xcode runner\n" +
		"ERROR bpfel\n" +
		"2 of 20 configurations failed\n"
	s, f = xplatcheckSummary(bad)
	require.Equal(t, "2 of 20 configurations failed", s)
	require.Equal(t, ": FAIL aarch64-apple-darwin, ERROR bpfel", f)
}

// Guards the schema checks: drift the runner does not understand fails
// loudly instead of measuring the wrong thing.
func TestTargetsValidation(t *testing.T) {
	base := `{"schemaVersion":1,"hermeticLlvm":{"module":"llvm","version":"0.8.24"},
"execPlatforms":[{"name":"linux-x86-64","label":"@cucina_platforms//exec:linux-x86-64-native","pool":"linux-x86-64","runner":"native"}],
"targets":[%s]}`
	row := func(s string) error {
		_, err := parseTargets([]byte(fmt.Sprintf(base, s)))
		return err
	}
	test := `"test":{"label":"@cucina_platforms//test:t","pool":"linux-x86-64","runner":"native","mode":"native"}`
	require.NoError(t, row(`{"name":"a","platform":"@llvm//platforms:a","execPlatforms":["linux-x86-64"],"coverage":"full","testSkip":"incompatible",`+test+`}`))
	require.NoError(t, row(`{"name":"x","excluded":true,"reason":"excluded by the user","execPlatforms":[]}`))
	require.ErrorContains(t, row(`{"name":"x","excluded":true,"execPlatforms":[]}`), "no reason")
	require.ErrorContains(t, row(`{"name":"a","platform":"p","execPlatforms":["linux-x86-64"],"coverage":"everything","testSkip":"incompatible",`+test+`}`), "unknown coverage")
	require.ErrorContains(t, row(`{"name":"a","platform":"p","execPlatforms":["linux-x86-64"],"coverage":"full","testSkip":"incompatible","test":null}`), "needs a test placement")
	require.ErrorContains(t, row(`{"name":"a","platform":"p","execPlatforms":["linux-x86-64"],"coverage":"build-example","testSkip":"not-applicable",`+test+`}`), "build-only")
	require.ErrorContains(t, row(`{"name":"a","platform":"p","execPlatforms":["mac"],"coverage":"full","testSkip":"incompatible",`+test+`}`), "unknown exec platform")
	_, err := parseTargets([]byte(`{"schemaVersion":2}`))
	require.ErrorContains(t, err, "schemaVersion 2")
}
