// SPDX-License-Identifier: FSL-1.1-ALv2

// Command xplatcheck is the offline resolution check of the cross-compilation matrix (R-XPLAT-2,
// the offline equivalent of NFR-X1). It writes a scratch workspace that depends on
// @cucina_platforms (this repository's bazel/platforms) and on Abseil, then, for every target row
// of platforms/targets.json, runs `bazel aquery` with the flags `cucinactl bazelrc --cross` emits
// and checks where Bazel resolves each action:
//
//   - every CppCompile/CppLink action runs on the selected compile exec platform;
//   - every TestRunner action runs on a platform whose exec_properties are exactly the target's
//     test runner (its test_on_<P> twin, or the compile exec platform when that satisfies the
//     target's constraints and advertises the same runner);
//   - build-only targets (wasm, BPF) have no test action.
//
// Nothing executes: aquery only analyses, so it runs on any developer machine without a cluster.
//
//	bazel run //tools/xplat/cmd/xplatcheck -- -workdir /tmp/xplatcheck [-targets a,b] [-exec-pools]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sloper-ai/cucina/tools/xplat"
)

func main() {
	repo := flag.String("repo", os.Getenv("BUILD_WORKSPACE_DIRECTORY"), "Cucina repository root")
	workdir := flag.String("workdir", "", "scratch workspace directory (created; required)")
	bazel := flag.String("bazel", "bazelisk", "Bazel launcher")
	outputBase := flag.String("output_base", "", "Bazel --output_base for the scratch workspace")
	only := flag.String("targets", "", "comma-separated target names (default: every in-scope target)")
	execPools := flag.Bool("exec-pools", false, "also check every non-default compile exec platform of x86_64-linux-gnu (UC25)")
	startup := flag.String("startup", "", "extra startup options, space separated (e.g. --output_user_root=...)")
	extra := flag.String("flags", "", "extra command options, space separated (e.g. --repository_cache=...)")
	flag.Parse()
	if *repo == "" || *workdir == "" {
		fmt.Fprintln(os.Stderr, "xplatcheck: -repo and -workdir are required")
		os.Exit(2)
	}
	c, err := xplat.Load(filepath.Join(*repo, "platforms/pools.json"), filepath.Join(*repo, "platforms/targets.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "xplatcheck: %v\n", err)
		os.Exit(1)
	}
	if err := writeWorkspace(*repo, *workdir); err != nil {
		fmt.Fprintf(os.Stderr, "xplatcheck: %v\n", err)
		os.Exit(1)
	}

	type row struct {
		target xplat.Target
		exec   xplat.ExecPlatform
	}
	var rows []row
	want := map[string]bool{}
	for _, n := range strings.Split(*only, ",") {
		if n != "" {
			want[n] = true
		}
	}
	for _, t := range c.InScope() {
		if len(want) > 0 && !want[t.Name] {
			continue
		}
		e, _ := c.ExecPlatform(t.ExecPlatforms[0])
		rows = append(rows, row{t, e})
		if *execPools && t.Name == "x86_64-linux-gnu" {
			for _, name := range t.ExecPlatforms[1:] {
				e, _ := c.ExecPlatform(name)
				rows = append(rows, row{t, e})
			}
		}
	}

	failed := 0
	for _, r := range rows {
		problems, summary, err := check(c, *bazel, *startup, *extra, *outputBase, *workdir, r.target, r.exec)
		status := "PASS"
		if err != nil {
			status, problems = "ERROR", append(problems, err.Error())
		} else if len(problems) > 0 {
			status = "FAIL"
		}
		if status != "PASS" {
			failed++
		}
		fmt.Printf("%-5s %-24s compile=%-28s %s\n", status, r.target.Name, r.exec.Name, summary)
		for _, p := range problems {
			fmt.Printf("      - %s\n", p)
		}
	}
	if failed > 0 {
		fmt.Printf("%d of %d configurations failed\n", failed, len(rows))
		os.Exit(1)
	}
	fmt.Printf("all %d configurations resolve as specified\n", len(rows))
}

// crossFlags returns the configuration `cucinactl bazelrc --cross --target t --exec-pool e` emits
// (platform flags only): the allowed compile exec platforms with e first, then t's twin.
func crossFlags(c *xplat.Catalog, t xplat.Target, e xplat.ExecPlatform) []string {
	execs := []string{e.Label}
	for _, name := range t.ExecPlatforms {
		if other, _ := c.ExecPlatform(name); other.Label != e.Label {
			execs = append(execs, other.Label)
		}
	}
	if t.Test != nil {
		execs = append(execs, t.Test.Label)
	}
	flags := []string{
		"--platforms=" + t.Platform,
		"--extra_execution_platforms=" + strings.Join(execs, ","),
		"--host_platform=" + e.Label,
	}
	flags = append(flags, e.Flags...)
	if t.ABI != nil && *t.ABI == "msvc" {
		flags = append(flags, "--repo_env=BAZEL_MSVC_RUNTIME_VISUAL_STUDIO_EULA=1", "--repo_env=BAZEL_WINDOWS_SDK_EULA=1")
	}
	return flags
}

type aqueryOutput struct {
	Actions []struct {
		Mnemonic          string `json:"mnemonic"`
		TargetID          int    `json:"targetId"`
		ExecutionPlatform string `json:"executionPlatform"`
	} `json:"actions"`
	Targets []struct {
		ID    int    `json:"id"`
		Label string `json:"label"`
	} `json:"targets"`
}

func check(c *xplat.Catalog, bazel, startup, extra, outputBase, dir string, t xplat.Target, e xplat.ExecPlatform) ([]string, string, error) {
	query := `mnemonic("CppCompile|CppLink|TestRunner", //:lib + //:lib_test)`
	if t.Test == nil {
		query = `mnemonic("CppCompile|CppArchive", //:freestanding)`
	}
	var args []string
	if outputBase != "" {
		args = append(args, "--output_base="+outputBase)
	}
	args = append(args, strings.Fields(startup)...)
	args = append(args, "aquery", "--output=jsonproto", "--noshow_progress")
	args = append(args, strings.Fields(extra)...)
	args = append(args, crossFlags(c, t, e)...)
	args = append(args, query)
	cmd := exec.Command(bazel, args...)
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, "", fmt.Errorf("bazel aquery: %w\n%s", err, tail(stderr.String(), 15))
	}
	var aq aqueryOutput
	if err := json.Unmarshal(out, &aq); err != nil {
		return nil, "", fmt.Errorf("parsing aquery output: %w", err)
	}

	// Platforms that run the test runner's exact property set.
	testOK := map[string]bool{}
	if t.Test != nil {
		r, _ := c.Runner(t.Test.Pool, t.Test.Runner)
		testOK[canonical(t.Test.Label)] = true
		for _, x := range c.Targets.ExecPlatforms {
			xr, _ := c.Runner(x.Pool, x.Runner)
			if equal(xr.Properties, r.Properties) {
				testOK[canonical(x.Label)] = true
			}
		}
	}

	var problems []string
	counts := map[string]map[string]int{}
	tests := 0
	for _, a := range aq.Actions {
		p := canonical(a.ExecutionPlatform)
		if counts[a.Mnemonic] == nil {
			counts[a.Mnemonic] = map[string]int{}
		}
		counts[a.Mnemonic][short(p)]++
		switch a.Mnemonic {
		case "TestRunner":
			tests++
			if !testOK[p] {
				problems = append(problems, fmt.Sprintf("TestRunner on %s, want the %s/%s runner (%s)", p, t.Test.Pool, t.Test.Runner, t.Test.Label))
			}
		default:
			if p != canonical(e.Label) {
				problems = append(problems, fmt.Sprintf("%s on %s, want %s", a.Mnemonic, p, e.Label))
			}
		}
	}
	if t.Test != nil && tests == 0 {
		problems = append(problems, "no TestRunner action")
	}
	if t.Test == nil && tests > 0 {
		problems = append(problems, "build-only target has a TestRunner action")
	}
	if len(aq.Actions) == 0 {
		problems = append(problems, "no actions")
	}
	return problems, summarize(counts), nil
}

// canonical strips the apparent/canonical repository spelling: "@@cucina_platforms+//exec:x" and
// "@cucina_platforms//exec:x" are the same platform.
func canonical(label string) string {
	label = strings.TrimPrefix(label, "@@")
	label = strings.TrimPrefix(label, "@")
	repo, rest, ok := strings.Cut(label, "//")
	if !ok {
		return label
	}
	return strings.TrimSuffix(repo, "+") + "//" + rest
}

func short(label string) string {
	if i := strings.LastIndex(label, ":"); i >= 0 {
		return label[i+1:]
	}
	return label
}

func equal(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func summarize(counts map[string]map[string]int) string {
	var parts []string
	for m, byPlatform := range counts {
		var ps []string
		for p, n := range byPlatform {
			ps = append(ps, fmt.Sprintf("%s×%d", p, n))
		}
		sort.Strings(ps)
		parts = append(parts, m+"→"+strings.Join(ps, "+"))
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func writeWorkspace(repo, dir string) error {
	segment, err := os.ReadFile(filepath.Join(repo, "tools/xplat", xplat.ModuleSegment))
	if err != nil {
		return err
	}
	module := "module(name = \"xplatcheck\")\n\n" +
		"bazel_dep(name = \"abseil-cpp\", version = \"20260817.0\")\n" +
		"bazel_dep(name = \"apple_support\", version = \"2.9.1\")\n" +
		"bazel_dep(name = \"llvm\", version = \"0.8.24\")\n" +
		"bazel_dep(name = \"platforms\", version = \"1.1.0\")\n" +
		"bazel_dep(name = \"rules_cc\", version = \"0.2.25\")\n\n" +
		strings.Replace(string(segment), `path = "bazel/platforms"`, fmt.Sprintf("path = %q", filepath.Join(repo, "bazel/platforms")), 1)
	files := map[string]string{
		".bazelversion": "9.2.0\n",
		"MODULE.bazel":  module,
		".bazelrc": strings.Join([]string{
			"common --experimental_platform_in_output_dir",
			"common --noexperimental_use_platforms_in_output_dir_legacy_heuristic",
			"common --xcode_version_config=//:xcode_disabled",
			"common --repo_env=BAZEL_DO_NOT_DETECT_CPP_TOOLCHAIN=1",
			"common --repo_env=BAZEL_NO_APPLE_CPP_TOOLCHAIN=1",
			"common --@rules_cc//cc/toolchains/args/archiver_flags:use_libtool_on_macos=False",
			"",
		}, "\n"),
		"BUILD.bazel": `load("@apple_support//xcode:xcode_config.bzl", "xcode_config")
load("@rules_cc//cc:cc_library.bzl", "cc_library")
load("@rules_cc//cc:cc_test.bzl", "cc_test")

xcode_config(name = "xcode_disabled")

# An Abseil-style library and its test.
cc_library(name = "lib", srcs = ["lib.cc"], hdrs = ["lib.h"], deps = ["@abseil-cpp//absl/strings"])

cc_test(name = "lib_test", srcs = ["lib_test.cc"], deps = [":lib"])

# For targets without an OS (wasm, BPF): no libc, no C++ runtime.
cc_library(name = "freestanding", srcs = ["freestanding.c"])
`,
		"lib.h":          "#pragma once\n#include <string>\nstd::string Greet(const std::string& who);\n",
		"lib.cc":         "#include \"lib.h\"\n#include \"absl/strings/str_cat.h\"\nstd::string Greet(const std::string& who) { return absl::StrCat(\"hello \", who); }\n",
		"lib_test.cc":    "#include \"lib.h\"\nint main() { return Greet(\"x\") == \"hello x\" ? 0 : 1; }\n",
		"freestanding.c": "int add(int a, int b) { return a + b; }\n",
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}
