// SPDX-License-Identifier: FSL-1.1-ALv2

package pools_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"pgregory.net/rapid"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/config"
	"github.com/sloper-ai/cucina/internal/pools"
)

func shippedCatalog(t *testing.T) *pools.Catalog {
	t.Helper()
	c, err := pools.LoadCatalog("../../platforms/pools.json")
	require.NoError(t, err)
	return c
}

// Guards R-RE-1/R-POOL-1: the catalog shipped in the repository (and copied into
// the chart) passes the same validation the controller applies at startup.
func TestShippedCatalogIsValid(t *testing.T) {
	c := shippedCatalog(t)
	for _, name := range []string{"linux-x86-64", "linux-aarch64", "windows-x86-64", "macos-arm64-xcode27.0"} {
		_, ok := c.Platform(name)
		assert.True(t, ok, name)
	}
}

// Guards R-TEST-7 "fail fast on configuration": a broken catalog is rejected
// with a message that locates the problem.
func TestCatalogRejectsInvalidInput(t *testing.T) {
	base, err := os.ReadFile("../../platforms/pools.json")
	require.NoError(t, err)
	edit := func(old, new string) string {
		s := strings.Replace(string(base), old, new, 1)
		require.NotEqual(t, string(base), s, "edit %q did not apply", old)
		return s
	}
	cases := []struct {
		name, doc, want string
	}{
		{"unknown member", edit(`"emulator": "qemu-riscv64"`, `"emulator": "qemu-riscv64", "bogus": 1`), `"bogus" within "/platforms/0/runners/1"`},
		{"trailing data", string(base) + "{}", "after top-level value"},
		{"schema version", edit(`"schemaVersion": 1`, `"schemaVersion": 2`), "schemaVersion is 2"},
		{"duplicate property set", edit(`"ISA": "rv64g", "cucina-emulation": "qemu"`, `"ISA": "s390x", "cucina-emulation": "qemu"`), "same platform properties as runner"},
		{"OSFamily mismatch", edit(`"properties": { "OSFamily": "windows", "ISA": "x86-64" }`, `"properties": { "OSFamily": "linux", "ISA": "x86-64" }`), "must equal the platform os"},
		{"ISA outside lexicon", edit(`"ISA": "arm-a64" },`, `"ISA": "aarch64" },`), `ISA "aarch64"`},
		{"concurrency ambiguous", edit(`"concurrency": { "fixed": 2 },`, `"concurrency": { "fixed": 2, "vcpusFactor": 1.0 },`), "exactly one of vcpusFactor"},
		{"no default size class", edit(`"sizeClasses": [{ "name": "default", "sizeClass": 1 }],`, `"sizeClasses": [{ "name": "big", "sizeClass": 1 }],`), `size class "default" must exist`},
		{"xcode mismatch", edit(`"xcode-version": "27.0"`, `"xcode-version": "26.4"`), "must equal the platform xcodeVersion"},
		{"emulator missing", edit(`"concurrency": { "fixed": 2 },
          "emulator": "qemu-s390x"`, `"concurrency": { "fixed": 2 }`), "must name their emulator"},
		{"tart serves macOS only", edit(`"provider": "ec2",
      "os": "linux",
      "arch": "arm64"`, `"provider": "tart",
      "os": "linux",
      "arch": "arm64"`), "provider tart serves macOS only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pools.ParseCatalog([]byte(tc.doc))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func pool(platform, provider string, mut func(*v1alpha1.WorkerPoolSpec)) *v1alpha1.WorkerPool {
	wp := &v1alpha1.WorkerPool{ObjectMeta: metav1.ObjectMeta{Name: "p"}, Spec: v1alpha1.WorkerPoolSpec{
		Platform: platform, Provider: provider, SizeClass: "default", Capacity: v1alpha1.CapacitySpec{Max: 4},
	}}
	if mut != nil {
		mut(&wp.Spec)
	}
	return wp
}

func i32(v int32) *int32 { return &v }

// Guards R-POOL-1 (timers and concurrency defaults per platform, CR overrides)
// and R-RE-2/R-RE-3 (queues = instance names x runners x size class).
func TestResolve(t *testing.T) {
	cat := shippedCatalog(t)
	env := pools.Env{InstanceNames: []string{"main", "ci"}, VCPUsPerVM: 32, Generation: "v1"}
	type want struct {
		idle, startup time.Duration
		slots         map[string]int
		instances     []string
		err           error
	}
	cases := []struct {
		name string
		wp   *v1alpha1.WorkerPool
		env  pools.Env
		want want
	}{
		{"linux defaults", pool("linux-x86-64", "ec2", nil), env, want{idle: 5 * time.Minute, startup: 5 * time.Minute,
			slots: map[string]int{"native": 32, "qemu-rv64g": 2, "qemu-s390x": 2, "qemu-arm-a32": 2}, instances: []string{"main", "ci"}}},
		{"windows defaults", pool("windows-x86-64", "ec2", nil), env, want{idle: 10 * time.Minute, startup: 15 * time.Minute,
			slots: map[string]int{"native": 32}, instances: []string{"main", "ci"}}},
		{"macos shares vCPUs between runners", pool("macos-arm64-xcode27.0", "tart", nil), pools.Env{InstanceNames: []string{"main"}, VCPUsPerVM: 7},
			want{idle: 10 * time.Minute, startup: 5 * time.Minute, slots: map[string]int{"xcode": 7, "generic": 7}, instances: []string{"main"}}},
		{"overrides", pool("linux-x86-64", "ec2", func(s *v1alpha1.WorkerPoolSpec) {
			s.Timers.IdleTimeout = &metav1.Duration{Duration: time.Minute}
			s.Worker.Concurrency = i32(16)
			s.Worker.RunnerConcurrency = map[string]int32{"qemu-s390x": 4}
			s.InstanceNames = []string{"ci"}
		}), env, want{idle: time.Minute, startup: 5 * time.Minute,
			slots: map[string]int{"native": 16, "qemu-rv64g": 2, "qemu-s390x": 4, "qemu-arm-a32": 2}, instances: []string{"ci"}}},
		{"unknown platform", pool("solaris-sparc", "ec2", nil), env, want{err: pools.ErrUnknownPlatform}},
		{"provider mismatch", pool("linux-x86-64", "tart", nil), env, want{err: pools.ErrProviderMismatch}},
		{"unknown size class", pool("linux-x86-64", "ec2", func(s *v1alpha1.WorkerPoolSpec) { s.SizeClass = "huge" }), env, want{err: pools.ErrUnknownSizeClass}},
		{"unconfigured instance name", pool("linux-x86-64", "ec2", func(s *v1alpha1.WorkerPoolSpec) { s.InstanceNames = []string{"other"} }), env, want{err: pools.ErrInstanceName}},
		{"unknown VM size", pool("linux-x86-64", "ec2", nil), pools.Env{InstanceNames: []string{"main"}}, want{err: pools.ErrSizing}},
		{"non-positive timer", pool("linux-x86-64", "ec2", func(s *v1alpha1.WorkerPoolSpec) { s.Timers.DrainTimeout = &metav1.Duration{} }), env, want{err: pools.ErrInvalidSpec}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := pools.Resolve(cat, tc.wp, tc.env)
			if tc.want.err != nil {
				require.Error(t, err)
				assert.ErrorIs(t, err, tc.want.err, "got %v", err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want.idle, r.Spec.IdleTimeout)
			assert.Equal(t, tc.want.startup, r.Spec.StartupTimeout)
			assert.Equal(t, tc.want.instances, r.Spec.InstanceNames)
			got := map[string]int{}
			for _, ru := range r.Spec.Runners {
				got[ru.Name] = ru.Concurrency
			}
			assert.Equal(t, tc.want.slots, got)
			assert.Len(t, r.Queues(), len(tc.want.instances)*len(r.Spec.Runners))
		})
	}
}

// Guards R-RE-1 (each runner advertises exactly its catalog property set) and
// R-POOL-7 (dead-man limits reach the worker).
func TestWorkerSettingsCarryExactRunnerPlatforms(t *testing.T) {
	cat := shippedCatalog(t)
	r, err := pools.Resolve(cat, pool("macos-arm64-xcode27.0", "tart", nil), pools.Env{InstanceNames: []string{"main"}, VCPUsPerVM: 7, Generation: "27.0-1"})
	require.NoError(t, err)
	cfg := &config.Controller{}
	cfg.Autoscaler.DeadmanIdleLimit = config.Duration{Duration: 30 * time.Minute}
	set := pools.NewSet(cfg)
	set.Put(r)
	ws, gen, err := set.SettingsFor("p", "mini-1/vm-1")
	require.NoError(t, err)
	assert.Equal(t, "27.0-1", gen)
	assert.Equal(t, "mini-1/vm-1", ws.GetNode())
	require.Len(t, ws.GetRunners(), 2)
	props := func(i int) map[string]string {
		m := map[string]string{}
		for _, p := range ws.GetRunners()[i].GetPlatform() {
			m[p.GetName()] = p.GetValue()
		}
		return m
	}
	assert.Equal(t, map[string]string{"OSFamily": "macos", "ISA": "arm-a64", "xcode-version": "27.0"}, props(0))
	assert.Equal(t, map[string]string{"OSFamily": "macos", "ISA": "arm-a64"}, props(1))
	assert.Equal(t, uint64(pools.DefaultMacL1GiB)<<30, ws.GetL1SizeBytes())
	assert.Equal(t, 30*time.Minute, ws.GetDeadman().GetIdleLimit().AsDuration())
	_, _, err = set.SettingsFor("missing", "i-1")
	assert.Error(t, err)
}

// Guards R-POOL-8/R-OPS-2: every image maps to a generation usable as a tag and
// label value, and a different image (without a version label) is a different
// generation.
func TestGenerationIsLabelSafeAndDistinguishesImages(t *testing.T) {
	assert.Equal(t, "27.0-0.1.0", pools.Generation("27.0-0.1.0", "ghcr.io/x:27.0-0.1.0"))
	rapid.Check(t, func(t *rapid.T) {
		version := rapid.String().Draw(t, "version")
		a := rapid.StringMatching(`ami-[0-9a-f]{8,17}`).Draw(t, "a")
		b := rapid.StringMatching(`ami-[0-9a-f]{8,17}`).Draw(t, "b")
		g := pools.Generation(version, a)
		if g == "" || len(g) > 63 || strings.ContainsAny(g, " /:=;,") {
			t.Fatalf("generation %q is not label safe", g)
		}
		if strings.ContainsAny(version, " /:") && a != b && pools.Generation(version, b) == g {
			t.Fatalf("images %s and %s share generation %q", a, b, g)
		}
	})
}
