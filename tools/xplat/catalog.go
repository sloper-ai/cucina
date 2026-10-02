// SPDX-License-Identifier: FSL-1.1-ALv2

// Package xplat reads the platform catalogs (platforms/pools.json, platforms/targets.json),
// validates the cross-compilation matrix and generates the @cucina_platforms Bazel module
// (bazel/platforms): compile exec platforms, test exec platforms ("twins"), the exec-platform
// lists for .bazelrc and the root-module segment that wires the exec-side Apple SDK and the
// supported hermetic-llvm toolchain pairs (R-XPLAT-1, R-XPLAT-2, R-XPLAT-8, R-BUILD-4).
package xplat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Pools is the subset of platforms/pools.json that the generator needs.
type Pools struct {
	SchemaVersion int            `json:"schemaVersion"`
	Platforms     []PoolPlatform `json:"platforms"`
}

// PoolPlatform is one pool platform (OS + ISA + toolchain image) with its runners.
type PoolPlatform struct {
	Name         string   `json:"name"`
	Provider     string   `json:"provider"`
	OS           string   `json:"os"`
	Arch         string   `json:"arch"`
	XcodeVersion string   `json:"xcodeVersion,omitempty"`
	Runners      []Runner `json:"runners"`
}

// Runner is one bb_runner platform: Buildbarn matches its entire property set exactly.
type Runner struct {
	Name       string            `json:"name"`
	Properties map[string]string `json:"properties"`
	Emulator   string            `json:"emulator,omitempty"`
	Generic    bool              `json:"generic,omitempty"`
}

// Emulated reports whether the runner executes foreign binaries under qemu-user.
func (r Runner) Emulated() bool { return r.Properties["cucina-emulation"] == "qemu" }

// Targets is platforms/targets.json (schema: docs/cross-compilation.md#schema).
type Targets struct {
	SchemaVersion     int                `json:"schemaVersion"`
	HermeticLLVM      HermeticLLVM       `json:"hermeticLlvm"`
	ExcludedPlatforms []ExcludedPlatform `json:"excludedPlatforms"`
	ExecPlatforms     []ExecPlatform     `json:"execPlatforms"`
	Targets           []Target           `json:"targets"`
}

// HermeticLLVM pins the llvm Bazel module whose platforms the targets use.
type HermeticLLVM struct {
	Module      string `json:"module"`
	Version     string `json:"version"`
	LLVMVersion string `json:"llvmVersion"`
}

// ExcludedPlatform is an OS/CPU pair that is neither a target nor an exec platform.
type ExcludedPlatform struct {
	OS     string `json:"os"`
	CPU    string `json:"cpu"`
	Reason string `json:"reason"`
}

// ExecPlatform is a compile exec platform: one compile-capable runner of a pool.
type ExecPlatform struct {
	Name        string   `json:"name"`
	Label       string   `json:"label"`
	Pool        string   `json:"pool"`
	Runner      string   `json:"runner"`
	OS          string   `json:"os"`
	CPU         string   `json:"cpu"`
	Constraints []string `json:"constraints"`
	Flags       []string `json:"flags"`
}

// Target is one hermetic-llvm target platform row.
type Target struct {
	Name             string         `json:"name"`
	Aliases          []string       `json:"aliases"`
	Platform         string         `json:"platform"`
	OS               string         `json:"os"`
	CPU              string         `json:"cpu"`
	Libc             *string        `json:"libc"`
	ABI              *string        `json:"abi"`
	ExecPlatforms    []string       `json:"execPlatforms"`
	Test             *TestPlacement `json:"test"`
	TestMode         string         `json:"testMode"`
	TestSkip         string         `json:"testSkip,omitempty"`
	Coverage         string         `json:"coverage,omitempty"`
	TestTimeoutScale float64        `json:"testTimeoutScale,omitempty"`
	Notes            string         `json:"notes,omitempty"`
	Excluded         bool           `json:"excluded,omitempty"`
	Reason           string         `json:"reason,omitempty"`
}

// TestPlacement is where a target's tests run.
type TestPlacement struct {
	Label  string `json:"label"`
	Pool   string `json:"pool"`
	Runner string `json:"runner"`
	Mode   string `json:"mode"`
}

// PlatformName is the target name of the hermetic-llvm platform label ("linux_x86_64_musl").
func (t Target) PlatformName() string {
	_, name, _ := strings.Cut(t.Platform, ":")
	return name
}

// Catalog is both files, validated.
type Catalog struct {
	Pools   Pools
	Targets Targets
}

// Labels of the generated module (also referenced by targets.json and the docs).
const (
	moduleName     = "cucina_platforms"
	execPackage    = "@" + moduleName + "//exec"
	testPackage    = "@" + moduleName + "//test"
	llvmPlatforms  = "@llvm//platforms:"
	sdkVersionFlag = "--@" + moduleName + "//apple:sdk_version="
)

// Load reads and validates both catalogs.
func Load(poolsPath, targetsPath string) (*Catalog, error) {
	var c Catalog
	if err := decodeFile(poolsPath, &c.Pools); err != nil {
		return nil, err
	}
	if err := decodeFile(targetsPath, &c.Targets); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Parse validates catalogs given as JSON documents.
func Parse(poolsJSON, targetsJSON []byte) (*Catalog, error) {
	var c Catalog
	if err := decode(poolsJSON, &c.Pools); err != nil {
		return nil, fmt.Errorf("pools.json: %w", err)
	}
	if err := decode(targetsJSON, &c.Targets); err != nil {
		return nil, fmt.Errorf("targets.json: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func decodeFile(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := decode(data, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func decode(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	return dec.Decode(v)
}

// bazelCPU maps a pools.json arch to Bazel's @platforms//cpu name.
func bazelCPU(arch string) string {
	if arch == "arm64" {
		return "aarch64"
	}
	return arch
}

// reapiISA is the REAPI platform-lexicon ISA of a Bazel CPU (s390x is a Cucina extension).
var reapiISA = map[string]string{
	"x86_64":  "x86-64",
	"aarch64": "arm-a64",
	"armv7":   "arm-a32",
	"riscv64": "rv64g",
	"s390x":   "s390x",
}

var (
	validTestModes = []string{"native", "qemu-user", "none"}
	validTestSkips = []string{"incompatible", "not-applicable"}
	validCoverage  = []string{"full", "build-full-test-smoke", "build-example"}
	validOS        = []string{"linux", "macos", "windows", "none"}
	// Bazel target names in the generated packages: letters, digits and . _ - only.
	targetNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// Pool returns the pool platform named name.
func (c *Catalog) Pool(name string) (PoolPlatform, bool) {
	for _, p := range c.Pools.Platforms {
		if p.Name == name {
			return p, true
		}
	}
	return PoolPlatform{}, false
}

// Runner returns runner `runner` of pool `pool`.
func (c *Catalog) Runner(pool, runner string) (Runner, bool) {
	p, ok := c.Pool(pool)
	if !ok {
		return Runner{}, false
	}
	for _, r := range p.Runners {
		if r.Name == runner {
			return r, true
		}
	}
	return Runner{}, false
}

// ExecPlatform returns the compile exec platform named name.
func (c *Catalog) ExecPlatform(name string) (ExecPlatform, bool) {
	for _, e := range c.Targets.ExecPlatforms {
		if e.Name == name {
			return e, true
		}
	}
	return ExecPlatform{}, false
}

// Runnable returns the targets that have a test runner, in catalog order.
func (c *Catalog) Runnable() []Target {
	var out []Target
	for _, t := range c.Targets.Targets {
		if !t.Excluded && t.Test != nil {
			out = append(out, t)
		}
	}
	return out
}

// InScope returns the non-excluded targets, in catalog order.
func (c *Catalog) InScope() []Target {
	var out []Target
	for _, t := range c.Targets.Targets {
		if !t.Excluded {
			out = append(out, t)
		}
	}
	return out
}

func (c *Catalog) excluded(os, cpu string) (ExcludedPlatform, bool) {
	for _, e := range c.Targets.ExcludedPlatforms {
		if e.OS == os && e.CPU == cpu {
			return e, true
		}
	}
	return ExcludedPlatform{}, false
}

// Validate checks every invariant documented in docs/cross-compilation.md#schema. It reports
// all problems at once.
func (c *Catalog) Validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if c.Pools.SchemaVersion != 1 {
		fail("pools.json: unsupported schemaVersion %d", c.Pools.SchemaVersion)
	}
	if c.Targets.SchemaVersion != 1 {
		fail("targets.json: unsupported schemaVersion %d", c.Targets.SchemaVersion)
	}
	for _, e := range c.Targets.ExcludedPlatforms {
		if e.Reason == "" {
			fail("excludedPlatforms %s/%s: missing reason", e.OS, e.CPU)
		}
	}

	// Pools: no excluded OS/CPU may be offered as a pool (it would be an exec platform).
	for _, p := range c.Pools.Platforms {
		if e, ok := c.excluded(p.OS, bazelCPU(p.Arch)); ok {
			fail("pool %s: %s/%s is excluded: %s", p.Name, p.OS, p.Arch, e.Reason)
		}
		seen := map[string]bool{}
		for _, r := range p.Runners {
			key := propertyKey(r.Properties)
			if seen[key] {
				fail("pool %s: two runners advertise the same properties %s", p.Name, key)
			}
			seen[key] = true
		}
	}

	// Compile exec platforms.
	names, labels := map[string]bool{}, map[string]bool{}
	for _, e := range c.Targets.ExecPlatforms {
		where := "execPlatforms " + e.Name
		if names[e.Name] {
			fail("%s: duplicate name", where)
		}
		names[e.Name] = true
		if labels[e.Label] {
			fail("%s: duplicate label %s", where, e.Label)
		}
		labels[e.Label] = true
		if want := execPackage + ":" + e.Pool + "-" + e.Runner; e.Label != want {
			fail("%s: label %s, want %s", where, e.Label, want)
		}
		if !targetNameRE.MatchString(e.Pool + "-" + e.Runner) {
			fail("%s: %s-%s is not a valid Bazel target name", where, e.Pool, e.Runner)
		}
		if ex, ok := c.excluded(e.OS, e.CPU); ok {
			fail("%s: %s/%s is excluded as an exec platform: %s", where, e.OS, e.CPU, ex.Reason)
		}
		pool, ok := c.Pool(e.Pool)
		if !ok {
			fail("%s: unknown pool %q", where, e.Pool)
			continue
		}
		if pool.OS != e.OS || bazelCPU(pool.Arch) != e.CPU {
			fail("%s: os/cpu %s/%s differ from pool %s (%s/%s)", where, e.OS, e.CPU, pool.Name, pool.OS, pool.Arch)
		}
		r, ok := c.Runner(e.Pool, e.Runner)
		if !ok {
			fail("%s: pool %s has no runner %q", where, e.Pool, e.Runner)
			continue
		}
		if r.Emulated() || r.Generic {
			fail("%s: runner %s/%s is test-only (emulated or generic) and cannot compile", where, e.Pool, e.Runner)
		}
		for _, cv := range e.Constraints {
			if !strings.HasPrefix(cv, "@") || !strings.Contains(cv, "//") {
				fail("%s: constraint %q is not an absolute label", where, cv)
			}
		}
		if e.OS == "macos" {
			want := sdkVersionFlag + pool.XcodeVersion
			if pool.XcodeVersion == "" || !slices.Contains(e.Flags, want) {
				fail("%s: macOS exec platforms need the flag %s (the pool's Xcode SDK)", where, want)
			}
		}
	}

	// Targets.
	ids := map[string]string{}
	for _, t := range c.Targets.Targets {
		where := "target " + t.Name
		for _, id := range append([]string{t.Name, t.PlatformName()}, t.Aliases...) {
			if other, dup := ids[id]; dup && other != t.Name {
				fail("%s: name or alias %q also names %s", where, id, other)
			}
			ids[id] = t.Name
		}
		if !strings.HasPrefix(t.Platform, llvmPlatforms) {
			fail("%s: platform %s is not a hermetic-llvm platform (%s...)", where, t.Platform, llvmPlatforms)
		}
		if !slices.Contains(validOS, t.OS) {
			fail("%s: unknown os %q", where, t.OS)
		}
		ex, isExcluded := c.excluded(t.OS, t.CPU)
		if t.Excluded {
			if t.Reason == "" {
				fail("%s: excluded without a reason", where)
			}
			if len(t.ExecPlatforms) != 0 || t.Test != nil {
				fail("%s: excluded targets have no exec platforms and no test placement", where)
			}
			if !isExcluded {
				fail("%s: excluded, but %s/%s is not in excludedPlatforms", where, t.OS, t.CPU)
			}
			continue
		}
		if isExcluded {
			fail("%s: %s/%s is excluded (%s) but the row is not marked excluded", where, t.OS, t.CPU, ex.Reason)
		}
		if len(t.ExecPlatforms) == 0 {
			fail("%s: no exec platforms", where)
		}
		for _, name := range t.ExecPlatforms {
			e, ok := c.ExecPlatform(name)
			if !ok {
				fail("%s: unknown exec platform %q", where, name)
				continue
			}
			// User decision (section 13): macOS targets compile on macOS only.
			if t.OS == "macos" && e.OS != "macos" {
				fail("%s: macOS targets compile on macOS exec platforms only, not %s", where, name)
			}
		}
		if !slices.Contains(validTestModes, t.TestMode) {
			fail("%s: testMode %q, want one of %v", where, t.TestMode, validTestModes)
		}
		if !slices.Contains(validTestSkips, t.TestSkip) {
			fail("%s: testSkip %q, want one of %v", where, t.TestSkip, validTestSkips)
		}
		if !slices.Contains(validCoverage, t.Coverage) {
			fail("%s: coverage %q, want one of %v", where, t.Coverage, validCoverage)
		}
		if t.TestMode == "none" {
			if t.Test != nil || t.TestSkip != "not-applicable" {
				fail("%s: build-only targets have test null and testSkip not-applicable", where)
			}
			continue
		}
		if t.Test == nil {
			fail("%s: testMode %s needs a test placement", where, t.TestMode)
			continue
		}
		if t.TestSkip == "not-applicable" {
			fail("%s: a runnable target cannot skip its test step", where)
		}
		if want := testPackage + ":test_on_" + t.PlatformName(); t.Test.Label != want {
			fail("%s: test label %s, want %s", where, t.Test.Label, want)
		}
		if t.Test.Mode != t.TestMode {
			fail("%s: test.mode %q differs from testMode %q", where, t.Test.Mode, t.TestMode)
		}
		r, ok := c.Runner(t.Test.Pool, t.Test.Runner)
		if !ok {
			fail("%s: test runner %s/%s not in pools.json", where, t.Test.Pool, t.Test.Runner)
			continue
		}
		if r.Emulated() != (t.TestMode == "qemu-user") {
			fail("%s: testMode %s but runner %s/%s emulated=%v", where, t.TestMode, t.Test.Pool, t.Test.Runner, r.Emulated())
		}
		if got := r.Properties["OSFamily"]; got != t.OS {
			fail("%s: test runner OSFamily %q, want %q", where, got, t.OS)
		}
		if got, want := r.Properties["ISA"], reapiISA[t.CPU]; got != want {
			fail("%s: test runner ISA %q, want %q", where, got, want)
		}
	}
	return errors.Join(errs...)
}

// propertyKey renders a property set canonically (sorted name=value pairs).
func propertyKey(props map[string]string) string {
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + props[k]
	}
	return "{" + strings.Join(parts, ",") + "}"
}
