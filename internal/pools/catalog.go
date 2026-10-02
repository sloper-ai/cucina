// SPDX-License-Identifier: FSL-1.1-ALv2

// Package pools turns the platform catalog (platforms/pools.json merged with
// Helm values platforms.extra) and a WorkerPool resource into the resolved,
// provider-neutral domain.PoolSpec the autoscaler works on, plus the
// WorkerSettings handed to workers at enrollment.
//
// The catalog is parsed strictly (unknown members are an error, R-TEST-7 "fail
// fast on configuration") and validated as a whole, so a broken catalog stops
// the controller at startup with every problem listed, instead of failing one
// pool at a time at runtime.
package pools

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"

	"github.com/sloper-ai/cucina/internal/config"
	"github.com/sloper-ai/cucina/internal/domain"
)

// SchemaVersion is the catalog schema this controller understands.
const SchemaVersion = 1

// Catalog is platforms/pools.json.
type Catalog struct {
	Comment             string     `json:"$comment,omitempty"`
	SchemaVersion       int        `json:"schemaVersion"`
	DefaultInstanceName string     `json:"defaultInstanceName"`
	Platforms           []Platform `json:"platforms"`
}

// Platform is one pool platform: OS + ISA + toolchain image, with its runners
// (exact REAPI property sets) and size classes.
type Platform struct {
	Comment      string           `json:"$comment,omitempty"`
	Name         string           `json:"name"`
	Description  string           `json:"description,omitempty"`
	Provider     domain.Provider  `json:"provider"`
	OS           string           `json:"os"`
	Arch         string           `json:"arch"`
	XcodeVersion string           `json:"xcodeVersion,omitempty"`
	Defaults     PlatformDefaults `json:"defaults"`
	SizeClasses  []SizeClass      `json:"sizeClasses"`
	Runners      []Runner         `json:"runners"`
}

// PlatformDefaults are the per-platform defaults a WorkerPool may override.
type PlatformDefaults struct {
	IdleTimeout    config.Duration `json:"idleTimeout"`
	DrainTimeout   config.Duration `json:"drainTimeout"`
	StartupTimeout config.Duration `json:"startupTimeout"`
	BuildDirectory string          `json:"buildDirectory"`
	L1Placement    string          `json:"l1Placement"`
}

// SizeClass maps a size class name to Buildbarn's uint32 size class.
type SizeClass struct {
	Name      string `json:"name"`
	SizeClass uint32 `json:"sizeClass"`
}

// Runner is one bb_runner platform advertised by every worker of the platform.
type Runner struct {
	Name        string            `json:"name"`
	Properties  map[string]string `json:"properties"`
	Concurrency Concurrency       `json:"concurrency"`
	Emulator    string            `json:"emulator,omitempty"`
	// Generic marks the macOS generic arm64 runner, whose property set is shared
	// by every macOS platform (several pools may serve its queue).
	Generic bool `json:"generic,omitempty"`
}

// Concurrency is the number of execution slots of a runner on one VM: either
// VCPUsFactor x vCPUs (rounded down, at least 1) or Fixed.
type Concurrency struct {
	VCPUsFactor float64 `json:"vcpusFactor,omitempty"`
	Fixed       int     `json:"fixed,omitempty"`
}

// Slots returns the runner's slots on a VM with vcpus vCPUs.
func (c Concurrency) Slots(vcpus int) int {
	if c.Fixed > 0 {
		return c.Fixed
	}
	n := int(c.VCPUsFactor * float64(vcpus))
	return max(n, 1)
}

// LoadCatalog reads and validates the catalog at path.
func LoadCatalog(path string) (*Catalog, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("platform catalog: %w", err)
	}
	c, err := ParseCatalog(b)
	if err != nil {
		return nil, fmt.Errorf("platform catalog %s: %w", path, err)
	}
	return c, nil
}

// ParseCatalog parses strictly (unknown members and trailing data are errors)
// and validates the catalog.
func ParseCatalog(b []byte) (*Catalog, error) {
	var c Catalog
	if err := json.Unmarshal(b, &c, json.RejectUnknownMembers(true)); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Platform returns the platform with the given name.
func (c *Catalog) Platform(name string) (*Platform, bool) {
	for i := range c.Platforms {
		if c.Platforms[i].Name == name {
			return &c.Platforms[i], true
		}
	}
	return nil, false
}

// SizeClass returns the Buildbarn size class for a size class name.
func (p *Platform) SizeClass(name string) (uint32, bool) {
	for _, s := range p.SizeClasses {
		if s.Name == name {
			return s.SizeClass, true
		}
	}
	return 0, false
}

var (
	nameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
	// REAPI platform lexicon ISAs plus Cucina's documented s390x extension (R-RE-1).
	validISAs     = []string{"x86-64", "arm-a64", "arm-a32", "rv64g", "s390x"}
	validOS       = []string{"linux", "windows", "macos"}
	validArch     = []string{"x86_64", "arm64"}
	validBuildDir = []string{"auto", "fuse", "winfsp", "nfsv4", "native"}
	validL1       = []string{"auto", "instance-store", "ebs", "memory", "vm-disk"}
)

// Property names with meaning to Cucina (R-RE-1).
const (
	PropOSFamily     = "OSFamily"
	PropISA          = "ISA"
	PropXcodeVersion = "xcode-version"
	PropEmulation    = "cucina-emulation"
)

// Validate checks the whole catalog and reports every problem at once.
func (c *Catalog) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if c.SchemaVersion != SchemaVersion {
		add("schemaVersion is %d, this controller understands %d", c.SchemaVersion, SchemaVersion)
	}
	if c.DefaultInstanceName == "" {
		add("defaultInstanceName must not be empty")
	}
	if len(c.Platforms) == 0 {
		add("platforms must not be empty")
	}
	seen := map[string]bool{}
	for i := range c.Platforms {
		p := &c.Platforms[i]
		at := fmt.Sprintf("platforms[%d] (%s)", i, p.Name)
		if !nameRE.MatchString(p.Name) || len(p.Name) > 63 {
			add("%s: name must be a lowercase DNS-style label of at most 63 characters", at)
		}
		if seen[p.Name] {
			add("%s: duplicate platform name", at)
		}
		seen[p.Name] = true
		errs = append(errs, p.validate(at)...)
	}
	return errors.Join(errs...)
}

func (p *Platform) validate(at string) []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(at+": "+format, a...)) }
	switch p.Provider {
	case domain.ProviderEC2, domain.ProviderTart:
	default:
		add("provider %q must be ec2 or tart", p.Provider)
	}
	if !slices.Contains(validOS, p.OS) {
		add("os %q must be one of %v", p.OS, validOS)
	}
	if !slices.Contains(validArch, p.Arch) {
		add("arch %q must be one of %v", p.Arch, validArch)
	}
	if p.Provider == domain.ProviderTart && p.OS != "macos" {
		add("provider tart serves macOS only")
	}
	d := p.Defaults
	for name, v := range map[string]config.Duration{"idleTimeout": d.IdleTimeout, "drainTimeout": d.DrainTimeout, "startupTimeout": d.StartupTimeout} {
		if v.Duration <= 0 {
			add("defaults.%s must be a positive duration", name)
		}
	}
	if !slices.Contains(validBuildDir, d.BuildDirectory) {
		add("defaults.buildDirectory %q must be one of %v", d.BuildDirectory, validBuildDir)
	}
	if !slices.Contains(validL1, d.L1Placement) {
		add("defaults.l1Placement %q must be one of %v", d.L1Placement, validL1)
	}
	if d.L1Placement == "vm-disk" && p.Provider != domain.ProviderTart {
		add("defaults.l1Placement vm-disk is only valid for provider tart")
	}

	if len(p.SizeClasses) == 0 {
		add("sizeClasses must not be empty")
	}
	names, numbers := map[string]bool{}, map[uint32]bool{}
	for _, s := range p.SizeClasses {
		if s.Name == "" || names[s.Name] {
			add("size class names must be non-empty and unique (%q)", s.Name)
		}
		if numbers[s.SizeClass] {
			add("size class %d is declared twice", s.SizeClass)
		}
		names[s.Name], numbers[s.SizeClass] = true, true
	}
	if !names["default"] {
		add(`size class "default" must exist`)
	}

	if len(p.Runners) == 0 {
		add("runners must not be empty")
	}
	runnerNames, propSets := map[string]bool{}, map[string]string{}
	xcodeRunner := false
	for _, r := range p.Runners {
		rat := fmt.Sprintf("runner %q", r.Name)
		if r.Name == "" || runnerNames[r.Name] {
			add("%s: runner names must be non-empty and unique", rat)
		}
		runnerNames[r.Name] = true
		for k, v := range r.Properties {
			if k == "" || v == "" {
				add("%s: platform property names and values must be non-empty", rat)
			}
		}
		if got := r.Properties[PropOSFamily]; got != p.OS {
			add("%s: property OSFamily %q must equal the platform os %q", rat, got, p.OS)
		}
		if isa := r.Properties[PropISA]; !slices.Contains(validISAs, isa) {
			add("%s: property ISA %q must be one of %v", rat, isa, validISAs)
		}
		if xv, ok := r.Properties[PropXcodeVersion]; ok {
			if xv != p.XcodeVersion {
				add("%s: xcode-version %q must equal the platform xcodeVersion %q", rat, xv, p.XcodeVersion)
			}
			xcodeRunner = true
		}
		if r.Properties[PropEmulation] != "" && r.Emulator == "" {
			add("%s: emulated runners must name their emulator", rat)
		}
		key := domain.PropertiesKey(r.Properties)
		if other, dup := propSets[key]; dup {
			add("%s: same platform properties as runner %q (Buildbarn matches the entire set, so they would be one queue)", rat, other)
		}
		propSets[key] = r.Name
		c := r.Concurrency
		if (c.Fixed > 0) == (c.VCPUsFactor > 0) || c.Fixed < 0 || c.VCPUsFactor < 0 {
			add("%s: concurrency needs exactly one of vcpusFactor > 0 or fixed >= 1", rat)
		}
	}
	if p.XcodeVersion != "" && !xcodeRunner {
		add("xcodeVersion %q is set but no runner advertises xcode-version", p.XcodeVersion)
	}
	return errs
}
