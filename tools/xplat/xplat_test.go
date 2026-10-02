// SPDX-License-Identifier: FSL-1.1-ALv2

package xplat

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Files under test. `bazel test` passes their runfiles paths through the environment (runfiles_env in
// BUILD.bazel: the module files come from the @cucina_platforms repository); `go test` reads the
// source tree, relative to this package directory.
var (
	poolsPath     = file("XPLAT_POOLS_JSON", "../../platforms/pools.json")
	targetsPath   = file("XPLAT_TARGETS_JSON", "../../platforms/targets.json")
	execBuildPath = file("XPLAT_EXEC_BUILD", "../../bazel/platforms/"+ExecBuildFile)
	testBuildPath = file("XPLAT_TEST_BUILD", "../../bazel/platforms/"+TestBuildFile)
	bazelrcPath   = file("XPLAT_BAZELRC", "../../bazel/platforms/"+BazelrcFile)
	segmentPath   = file("XPLAT_SEGMENT", ModuleSegment)
)

const excludedLabels = `macos_x86_64|windows_aarch64|x86_64-apple-darwin|aarch64-windows`

func file(env, fallback string) string {
	if p := os.Getenv(env); p != "" {
		return p
	}
	return fallback
}

func load(t *testing.T) *Catalog {
	t.Helper()
	c, err := Load(poolsPath, targetsPath)
	require.NoError(t, err)
	return c
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

// R-XPLAT-1: the catalog is hermetic-llvm's support matrix minus the user's exclusions.
func TestCatalogIsTheRXplat1Matrix(t *testing.T) {
	c := load(t)
	var inScope, excluded []string
	for _, tg := range c.Targets.Targets {
		if tg.Excluded {
			excluded = append(excluded, tg.Platform)
		} else {
			inScope = append(inScope, tg.Platform)
		}
	}
	require.ElementsMatch(t, []string{
		"@llvm//platforms:macos_aarch64",
		"@llvm//platforms:linux_x86_64_gnu.2.28", "@llvm//platforms:linux_x86_64_musl",
		"@llvm//platforms:linux_aarch64_gnu.2.28", "@llvm//platforms:linux_aarch64_musl",
		"@llvm//platforms:linux_riscv64_gnu.2.33", "@llvm//platforms:linux_riscv64_musl",
		"@llvm//platforms:linux_s390x_gnu.2.28", "@llvm//platforms:linux_s390x_musl",
		"@llvm//platforms:linux_armv7_gnu.2.28", "@llvm//platforms:linux_armv7_musl",
		"@llvm//platforms:windows_x86_64_msvc", "@llvm//platforms:windows_x86_64",
		"@llvm//platforms:none_wasm32", "@llvm//platforms:none_wasm64",
		"@llvm//platforms:none_bpfeb", "@llvm//platforms:none_bpfel",
	}, inScope)
	require.ElementsMatch(t, []string{
		"@llvm//platforms:macos_x86_64",
		"@llvm//platforms:windows_aarch64", "@llvm//platforms:windows_aarch64_msvc",
	}, excluded)
	for _, tg := range c.InScope() {
		first, _ := c.ExecPlatform(tg.ExecPlatforms[0])
		want := "linux"
		if tg.OS == "macos" {
			want = "macos" // user decision: macOS targets compile on macOS
		}
		require.Equal(t, want, first.OS, "default compile exec platform of %s", tg.Name)
	}
}

// One generated platform() block.
type platformBlock struct {
	constraints []string
	properties  map[string]string
	parents     []string
}

var (
	blockRE  = regexp.MustCompile(`(?ms)^platform\(\n(.*?)^\)$`)
	nameRE   = regexp.MustCompile(`(?m)^    name = "([^"]+)",$`)
	listRE   = func(attr string) *regexp.Regexp { return regexp.MustCompile(`(?ms)^    ` + attr + ` = \[(.*?)\],$`) }
	dictRE   = regexp.MustCompile(`(?ms)^    exec_properties = \{(.*?)\},$`)
	stringRE = regexp.MustCompile(`"([^"]*)"`)
	entryRE  = regexp.MustCompile(`"([^"]*)": "([^"]*)"`)
)

// parsePlatforms reads platform() targets from a generated BUILD file, independently of the
// generator, so the test checks the checked-in files rather than the code that wrote them.
func parsePlatforms(t *testing.T, build string) map[string]platformBlock {
	t.Helper()
	out := map[string]platformBlock{}
	for _, m := range blockRE.FindAllStringSubmatch(build, -1) {
		body := m[1]
		name := nameRE.FindStringSubmatch(body)
		require.NotNil(t, name, "platform without a name:\n%s", body)
		b := platformBlock{properties: map[string]string{}}
		strs := func(re *regexp.Regexp) []string {
			var l []string
			if sm := re.FindStringSubmatch(body); sm != nil {
				for _, s := range stringRE.FindAllStringSubmatch(sm[1], -1) {
					l = append(l, s[1])
				}
			}
			return l
		}
		b.constraints = strs(listRE("constraint_values"))
		b.parents = strs(listRE("parents"))
		if sm := dictRE.FindStringSubmatch(body); sm != nil {
			for _, e := range entryRE.FindAllStringSubmatch(sm[1], -1) {
				b.properties[e[1]] = e[2]
			}
		}
		require.NotContains(t, out, name[1], "duplicate platform")
		out[name[1]] = b
	}
	return out
}

// R-TEST-6 "Cross-platform", R-XPLAT-2, D11: for every matrix row the checked-in Bazel platforms,
// the runner property sets that the chart predeclares queues for, and the runners agree.
func TestGeneratedPlatformsMatchRunnersAndQueues(t *testing.T) {
	c := load(t)

	// The chart renders predeclaredPlatformQueues from charts/cucina/files/platforms.json, a copy of
	// pools.json that the chart's own test keeps equal: one queue per runner property set of every
	// pool platform. Exact property sets are the queue keys.
	queues := map[string]bool{}
	for _, p := range c.Pools.Platforms {
		for _, r := range p.Runners {
			queues[p.Name+"/"+propertyKey(r.Properties)] = true
		}
	}

	execs := parsePlatforms(t, read(t, execBuildPath))
	require.Len(t, execs, len(c.Targets.ExecPlatforms), "one compile exec platform per catalog entry")
	for _, e := range c.Targets.ExecPlatforms {
		name := strings.TrimPrefix(e.Label, execPackage+":")
		got, ok := execs[name]
		require.True(t, ok, "missing compile exec platform %s", e.Label)
		r, _ := c.Runner(e.Pool, e.Runner)
		require.Equal(t, r.Properties, got.properties, "%s: exec_properties must equal runner %s/%s", e.Label, e.Pool, e.Runner)
		require.True(t, queues[e.Pool+"/"+propertyKey(got.properties)], "%s: no predeclared queue for %v", e.Label, got.properties)
		require.Subset(t, got.constraints, append([]string{"@platforms//os:" + e.OS, "@platforms//cpu:" + e.CPU}, e.Constraints...))
	}

	tests := parsePlatforms(t, read(t, testBuildPath))
	require.Len(t, tests, len(c.Runnable()), "one test exec platform per runnable target")
	for _, tg := range c.Runnable() {
		got, ok := tests["test_on_"+tg.PlatformName()]
		require.True(t, ok, "%s: missing %s", tg.Name, tg.Test.Label)
		require.Equal(t, []string{tg.Platform}, got.parents, "%s: the twin inherits the target platform", tg.Name)
		r, _ := c.Runner(tg.Test.Pool, tg.Test.Runner)
		require.Equal(t, r.Properties, got.properties, "%s: test exec_properties must equal runner %s/%s", tg.Name, tg.Test.Pool, tg.Test.Runner)
		require.True(t, queues[tg.Test.Pool+"/"+propertyKey(got.properties)], "%s: no predeclared queue", tg.Name)
	}

	// Build-only and excluded targets have no test platform; excluded platforms appear nowhere.
	excluded := regexp.MustCompile(excludedLabels)
	for _, f := range []string{execBuildPath, testBuildPath, bazelrcPath, segmentPath} {
		require.Empty(t, excluded.FindAllString(read(t, f), -1), "%s mentions an excluded platform", f)
	}
	for _, tg := range c.InScope() {
		if tg.Test == nil {
			require.NotContains(t, tests, "test_on_"+tg.PlatformName(), "%s is build-only", tg.Name)
		}
	}
}

// R-BUILD-4, R-XPLAT-2c, R-XPLAT-8: exec-platform lists are complete and ordered, and macOS
// targets can only compile on macOS (no other exec platform has a toolchain for them).
func TestExecutionListsAndToolchainPairs(t *testing.T) {
	c := load(t)
	rc := read(t, bazelrcPath)
	lists := map[string][]string{}
	for _, m := range regexp.MustCompile(`(?m)^build:(\S+) --extra_execution_platforms=(\S+)$`).FindAllStringSubmatch(rc, -1) {
		lists[m[1]] = strings.Split(m[2], ",")
	}
	all := []string{}
	for _, e := range c.Targets.ExecPlatforms {
		all = append(all, e.Label)
	}
	for _, tg := range c.Runnable() {
		all = append(all, tg.Test.Label)
	}
	require.ElementsMatch(t, all, lists["cucina"], ":cucina must list every exec and test platform")
	require.ElementsMatch(t, all, lists["cucina-macos"], ":cucina-macos must list every exec and test platform")
	require.Equal(t, c.Targets.ExecPlatforms[0].Label, lists["cucina"][0], "default exec order")
	mac, _ := c.ExecPlatform("macos-arm64-xcode27.0")
	require.Equal(t, mac.Label, lists["cucina-macos"][0], "macOS first in :cucina-macos")

	seg := read(t, segmentPath)
	pairs := regexp.MustCompile(`"@llvm//toolchain:([a-z0-9_]+)_to_([a-z0-9_]+)"`).FindAllStringSubmatch(seg, -1)
	require.NotEmpty(t, pairs)
	registered := map[string]bool{}
	for _, p := range pairs {
		exec, target := p[1], p[2]
		registered[exec+">"+target] = true
		if strings.HasPrefix(target, "macos") {
			require.True(t, strings.HasPrefix(exec, "macos"), "%s compiles for macOS on a non-macOS exec platform", p[0])
		}
	}
	for _, e := range c.Targets.ExecPlatforms {
		for _, tg := range c.InScope() {
			if tg.OS == "macos" && e.OS != "macos" {
				continue
			}
			name := e.OS + "_" + e.CPU + ">" + tg.OS + "_" + tg.CPU
			if tg.ABI != nil && *tg.ABI == "msvc" {
				name += "_msvc"
			}
			require.True(t, registered[name], "no toolchain registered for %s", name)
		}
	}
	require.Regexp(t, `override_repo\(\s*osx,\s*macos_sdk = "cucina_macos_exec_sdk",?\s*\)`, seg, "R-XPLAT-8: @macos_sdk must be the exec-side SDK")
}

// R-XPLAT-1 exclusions and the routing invariants: the generator rejects catalogs that break them.
func TestValidateRejectsInvalidCatalogs(t *testing.T) {
	poolsJSON, targetsJSON := []byte(read(t, poolsPath)), []byte(read(t, targetsPath))
	for _, tc := range []struct {
		name   string
		mutate func(p *Pools, tg *Targets)
		want   string
	}{
		{"macOS x86_64 pool", func(p *Pools, _ *Targets) {
			p.Platforms = append(p.Platforms, PoolPlatform{Name: "macos-x86", OS: "macos", Arch: "x86_64"})
		}, "pool macos-x86: macos/x86_64 is excluded"},
		{"Windows arm64 exec platform", func(_ *Pools, tg *Targets) {
			tg.ExecPlatforms[2].CPU = "aarch64"
		}, "windows/aarch64 is excluded as an exec platform"},
		{"excluded target made runnable", func(_ *Pools, tg *Targets) {
			i := slices.IndexFunc(tg.Targets, func(x Target) bool { return x.Name == "x86_64-apple-darwin" })
			tg.Targets[i].Excluded = false
		}, "is excluded (macOS x86_64"},
		{"macOS target compiled on Linux", func(_ *Pools, tg *Targets) {
			tg.Targets[0].ExecPlatforms = []string{"linux-x86-64"}
		}, "macOS targets compile on macOS exec platforms only"},
		{"compile on a qemu runner", func(_ *Pools, tg *Targets) {
			tg.ExecPlatforms[0].Runner, tg.ExecPlatforms[0].Label = "qemu-rv64g", execPackage+":linux-x86-64-qemu-rv64g"
		}, "is test-only"},
		{"test runner of the wrong ISA", func(_ *Pools, tg *Targets) {
			i := slices.IndexFunc(tg.Targets, func(x Target) bool { return x.Name == "aarch64-linux-gnu" })
			tg.Targets[i].Test.Pool = "linux-x86-64"
		}, `test runner ISA "x86-64", want "arm-a64"`},
		{"runnable target without a twin label", func(_ *Pools, tg *Targets) {
			tg.Targets[1].Test.Label = testPackage + ":somewhere_else"
		}, "want @cucina_platforms//test:test_on_linux_x86_64_gnu.2.28"},
		{"macOS exec platform without its SDK flag", func(_ *Pools, tg *Targets) {
			tg.ExecPlatforms[3].Flags = nil
		}, "need the flag --@cucina_platforms//apple:sdk_version=27.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var p Pools
			var tg Targets
			require.NoError(t, json.Unmarshal(poolsJSON, &p))
			require.NoError(t, json.Unmarshal(targetsJSON, &tg))
			tc.mutate(&p, &tg)
			pb, _ := json.Marshal(p)
			tb, _ := json.Marshal(tg)
			_, err := Parse(pb, tb)
			require.ErrorContains(t, err, tc.want)
		})
	}
}
