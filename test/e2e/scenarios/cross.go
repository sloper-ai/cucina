// SPDX-License-Identifier: FSL-1.1-ALv2

package scenarios

import (
	"encoding/json"

	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sloper-ai/cucina/test/e2e/bazelrun"
	"github.com/sloper-ai/cucina/test/e2e/collect/execlog"
	"github.com/sloper-ai/cucina/test/e2e/harness"
	"github.com/sloper-ai/cucina/test/e2e/infra"
	"github.com/sloper-ai/cucina/test/e2e/nfr"
	"github.com/sloper-ai/cucina/test/e2e/remote"
)

// Targets is platforms/targets.json (schema 1, docs/cli.md "Targets schema";
// owned by the cross agent).
type Targets struct {
	SchemaVersion int            `json:"schemaVersion"`
	ExecPlatforms []ExecPlatform `json:"execPlatforms"`
	Targets       []Target       `json:"targets"`
}

// ExecPlatform is one compile exec platform (a pool runner).
type ExecPlatform struct {
	Name, Label, Pool, Runner, OS, CPU string
}

// Target is one hermetic-llvm target platform of the R-XPLAT-1 matrix.
type Target struct {
	Name          string    `json:"name"`
	Aliases       []string  `json:"aliases"`
	Platform      string    `json:"platform"`
	OS            string    `json:"os"`
	CPU           string    `json:"cpu"`
	ABI           string    `json:"abi"`
	ExecPlatforms []string  `json:"execPlatforms"`
	Test          *TestSpec `json:"test"`
}

// TestSpec is where a target's tests run.
type TestSpec struct {
	Label, Pool, Runner, Mode string
}

// Runnable reports whether tests execute (wasm/BPF are build-only).
func (t Target) Runnable() bool { return t.Test != nil }

func loadTargets(env *harness.Env) (*Targets, error) {
	b, err := os.ReadFile(filepath.Join(env.RepoDir, "platforms", "targets.json"))
	if err != nil {
		return nil, harness.Skip("platforms/targets.json is not available yet (cross agent): %v", err)
	}
	var t Targets
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("platforms/targets.json: %w", err)
	}
	if t.SchemaVersion != 1 {
		return nil, fmt.Errorf("platforms/targets.json: schemaVersion %d, want 1", t.SchemaVersion)
	}
	return &t, nil
}

// runnerProps maps pool → runner → exact REAPI properties (platforms/pools.json).
func runnerProps(env *harness.Env) (map[string]map[string]map[string]string, error) {
	b, err := os.ReadFile(filepath.Join(env.RepoDir, "platforms", "pools.json"))
	if err != nil {
		return nil, err
	}
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
		return nil, err
	}
	out := map[string]map[string]map[string]string{}
	for _, p := range doc.Platforms {
		out[p.Name] = map[string]map[string]string{}
		for _, r := range p.Runners {
			out[p.Name][r.Name] = r.Properties
		}
	}
	return out, nil
}

func (t *Targets) execPlatform(name string) (ExecPlatform, bool) {
	for _, e := range t.ExecPlatforms {
		if e.Name == name {
			return e, true
		}
	}
	return ExecPlatform{}, false
}

// Config is one (client host, compile exec platform, target) combination.
type Config struct {
	Name     string // e.g. x86_64-linux-musl@linux-client
	Client   Lane
	Target   Target
	ExecPool string // compile exec platform name ("" = default order)
	// Patterns built/tested (qemu targets use a smoke subset; wasm/BPF an
	// example workspace).
	Patterns []string
	Command  string // test | build
}

// ConfigResult is the per-configuration record (NFR-X1/X2/X4).
type ConfigResult struct {
	Config      string            `json:"config"`
	Target      string            `json:"target"`
	Client      string            `json:"client"`
	ExecPool    string            `json:"execPool"`
	ExitCode    int               `json:"exitCode"`
	Wall        time.Duration     `json:"wall"`
	Routing     *execlog.Routing  `json:"routing,omitempty"`
	Tests       map[string]string `json:"tests,omitempty"`
	RemoteRatio float64           `json:"remoteRatio"`
	HitRatio    float64           `json:"hitRatio"`
	Error       string            `json:"error,omitempty"`
}

// smokePatterns is the qemu-user smoke subset (R-XPLAT-1: MUST for riscv64,
// s390x, armv7; the full suite is SHOULD).
var smokePatterns = []string{"//absl/base/...", "//absl/strings/...", "//absl/numeric/..."}

func configsFor(t *Targets, client Lane, filter func(Target) bool) []Config {
	var out []Config
	for _, tg := range t.Targets {
		if filter != nil && !filter(tg) {
			continue
		}
		c := Config{Name: tg.Name + "@" + client.Host, Client: client, Target: tg, Command: "test", Patterns: bazelrun.AbseilTargets}
		switch {
		case !tg.Runnable():
			c.Command, c.Patterns = "build", []string{"//absl/base:base", "//absl/strings:strings"}
		case strings.HasPrefix(tg.Test.Mode, "qemu"):
			c.Patterns = smokePatterns
		}
		out = append(out, c)
	}
	return out
}

// matrix runs configurations in concurrent batches, each in its own output
// base on its client, and records one ConfigResult per configuration.
type matrix struct {
	c       *harness.Context
	targets *Targets
	props   map[string]map[string]map[string]string
	lanes   map[string]*laneRun
	mu      sync.Mutex
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
	return &matrix{c: c, targets: t, props: props, lanes: map[string]*laneRun{}}, nil
}

func (m *matrix) lane(l Lane) (*laneRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if lr, ok := m.lanes[l.Host]; ok {
		return lr, nil
	}
	lr, err := openLane(m.c, Lane{Name: l.Name, Host: l.Host, PoolRole: l.PoolRole, Platform: l.Platform, Toolchain: bazelrun.HermeticLLVM, OSName: l.OSName})
	if err != nil {
		return nil, err
	}
	m.lanes[l.Host] = lr
	return lr, nil
}

// crossRC returns `cucinactl bazelrc --cross --target <t> [--exec-pool <p>]`.
func (m *matrix) crossRC(lr *laneRun, cfg Config) (string, error) {
	cli := hjoin(lr.host, lr.host.WorkDir(), "bin", "cucinactl")
	if lr.host.OS() == remote.Windows {
		cli += ".exe"
	}
	if lr.host.Name() == infra.DevMac {
		svc, _ := infra.Of(m.c)
		p, err := svc.CucinactlPath()
		if err != nil {
			return "", err
		}
		cli = p
	}
	args := fmt.Sprintf("bazelrc --cross --target %s", cfg.Target.Name)
	if cfg.ExecPool != "" {
		args += " --exec-pool " + cfg.ExecPool
	}
	res, err := lr.host.Run(m.c, quoteFor(lr.host, cli)+" "+args, remote.Opts{User: lr.user})
	if err != nil {
		return "", err
	}
	return string(res.Stdout), res.Err()
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
	r := ConfigResult{Config: cfg.Name, Target: cfg.Target.Name, Client: cfg.Client.Host, ExecPool: cfg.ExecPool}
	lr, err := m.lane(cfg.Client)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	rc, err := m.crossRC(lr, cfg)
	if err != nil {
		r.Error = "cucinactl bazelrc --cross: " + err.Error()
		return r
	}
	h := lr.host
	rcName := "cucina-" + strings.ReplaceAll(cfg.Name, "@", "-") + ".bazelrc"
	local := filepath.Join(m.c.Dir(), rcName)
	if err := os.WriteFile(local, []byte(rc), 0o644); err != nil {
		r.Error = err.Error()
		return r
	}
	if err := h.Put(m.c, local, hjoin(h, lr.ws, rcName)); err != nil {
		r.Error = err.Error()
		return r
	}
	safe := strings.NewReplacer("@", "-", ".", "_").Replace(cfg.Name)
	inv := bazelrun.Invocation{
		Name: m.c.Scenario.ID + "-" + safe, Host: h, Workspace: lr.ws, User: lr.user, Collect: true,
		Startup: []string{"--bazelrc=" + hjoin(h, lr.ws, rcName), "--output_base=" + hjoin(h, h.WorkDir(), "ob", safe)},
		// Concurrent servers share the checkout: never race on MODULE.bazel.lock.
		Command: cfg.Command, Args: append([]string{"--lockfile_mode=off", "--"}, cfg.Patterns...), Bazel: bazelBinary(m.c.Env, h),
	}
	o, err := bazelrun.Run(m.c, inv, filepath.Join(m.c.Dir(), safe))
	if err != nil {
		r.Error = err.Error()
		return r
	}
	recordOutcome(m.c, "config."+safe, o)
	r.ExitCode, r.Wall = o.ExitCode, o.Wall
	if o.ExecLog != nil {
		r.RemoteRatio, r.HitRatio = o.ExecLog.RemoteRatio(), o.ExecLog.RemoteCacheHitRatio()
	}
	if o.BEP != nil {
		r.Tests = map[string]string{}
		for _, t := range o.BEP.Tests {
			r.Tests[t.Label] = t.Status
		}
	}
	if o.ExecLogPath != "" {
		if l, err := execlog.ReadFile(o.ExecLogPath); err == nil {
			compile := m.compileProps(cfg)
			var test map[string]string
			if cfg.Target.Test != nil {
				test = m.props[cfg.Target.Test.Pool][cfg.Target.Test.Runner]
			}
			rt := l.RouteCheck(compile, test)
			r.Routing = &rt
		}
	}
	return r
}

// compileProps are the runner properties of the configuration's compile exec
// platform: the requested pool, else the target's first (default) one.
func (m *matrix) compileProps(cfg Config) map[string]string {
	name := cfg.ExecPool
	if name == "" && len(cfg.Target.ExecPlatforms) > 0 {
		name = cfg.Target.ExecPlatforms[0]
	}
	if ep, ok := m.targets.execPlatform(name); ok {
		return m.props[ep.Pool][ep.Runner]
	}
	return nil
}

// recordMatrix stores the per-configuration records and the NFR rows.
func recordMatrix(c *harness.Context, results []ConfigResult, start time.Time) []string {
	c.Record("configurations", results)
	var failed []string
	for _, r := range results {
		switch {
		case r.Error != "":
			failed = append(failed, r.Config+": "+r.Error)
			continue
		case r.ExitCode != 0:
			failed = append(failed, fmt.Sprintf("%s: bazel exited %d", r.Config, r.ExitCode))
		}
		if r.Routing != nil {
			addNFRs(c, nfr.Routing(r.Config, r.Routing.CompileLinkOnPool, r.Routing.CompileLinkTotal, r.Routing.TestOnRunner, r.Routing.TestTotal))
		}
		c.NFR(nfr.Reported("NFR-X4", r.Config, r.Wall.Seconds(), "s", ""))
	}
	c.NFR(nfr.Reported("NFR-X4", "whole matrix (concurrent batches)", c.Now().Sub(start).Seconds(), "s", fmt.Sprintf("%d configurations", len(results))))
	return failed
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
			m, err := newMatrix(c)
			if err != nil {
				return err
			}
			start := c.Now()
			cfgs := configsFor(m.targets, LinuxLane, nil)
			var results []ConfigResult
			if _, err := sampled(c, 15*time.Second, func() error {
				results = m.run(cfgs, 4)
				return nil
			}); err != nil {
				return err
			}
			failed := recordMatrix(c, results, start)
			outcomesVsBaseline(c, results)
			toolchainUploads(c, results)
			if len(failed) > 0 {
				return harness.Fail("%d configurations failed: %s", len(failed), strings.Join(failed, "; "))
			}
			return nil
		},
	}
}

// outcomesVsBaseline evaluates NFR-X2 for configurations that run locally
// on their client: the local baseline outcomes come from the baseline-<lane>
// scenario results (label → status). Root causes for deviations are recorded
// by the lead in values (docs/reports/issues.md) and re-attached by the
// report generator.
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
		var local map[string]string
		switch {
		case strings.HasPrefix(r.Target, "x86_64-linux-gnu"):
			local = locals["linux"]
		case strings.HasPrefix(r.Target, "x86_64-windows"):
			local = locals["windows"]
		case strings.HasPrefix(r.Target, "aarch64-apple-darwin"):
			local = locals["macos"]
		}
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
	var sent []float64
	for _, r := range results {
		if m, ok := c.Result.Metrics["config."+strings.NewReplacer("@", "-", ".", "_").Replace(r.Config)+".network_bytes_sent"]; ok {
			sent = append(sent, m.Value)
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
		ID: "T17", Title: "Exec coverage: compile a non-macOS target with each non-default exec pool (Linux arm64, macOS arm64, Windows x86_64)",
		Requires: []harness.Requirement{harness.RequiresAWS, harness.RequiresLinuxClient, harness.RequiresCrossMatrix, harness.RequiresCucinactl},
		Cost:     harness.CostMedium, EstimateUSD: 8, MaxInstances: 6, Essential: true, Timeout: 4 * time.Hour,
		NFRs: []string{"NFR-X1"}, Post: Guards,
		Run: func(c *harness.Context) error {
			m, err := newMatrix(c)
			if err != nil {
				return err
			}
			var tgt *Target
			for i := range m.targets.Targets {
				if m.targets.Targets[i].Name == "x86_64-linux-gnu" {
					tgt = &m.targets.Targets[i]
				}
			}
			if tgt == nil {
				return harness.Fail("x86_64-linux-gnu not in platforms/targets.json")
			}
			var cfgs []Config
			for _, ep := range tgt.ExecPlatforms[1:] { // the first is the default (Linux x86_64)
				cfgs = append(cfgs, Config{Name: tgt.Name + "@exec-" + ep, Client: LinuxLane, Target: *tgt, ExecPool: ep, Command: "build", Patterns: []string{"//absl/base/...", "//absl/strings/..."}})
			}
			start := c.Now()
			results := m.run(cfgs, 3)
			if failed := recordMatrix(c, results, start); len(failed) > 0 {
				return harness.Fail("%s", strings.Join(failed, "; "))
			}
			return nil
		},
	}
}

// ---------------------------------------------------------------- T18

func t18() *harness.Scenario {
	return &harness.Scenario{
		ID: "T18", Title: "Cross-host cache: rebuild two configurations from the dev Mac and windows-client after linux-client built them",
		Requires: []harness.Requirement{harness.RequiresAWS, harness.RequiresWindowsClient, harness.RequiresMacHost, harness.RequiresCrossMatrix, harness.RequiresCucinactl},
		Cost:     harness.CostMedium, EstimateUSD: 4, Essential: true, Timeout: 4 * time.Hour, DependsOn: []string{"T16"},
		NFRs: []string{"NFR-X3", "NFR-X1"}, Post: Guards,
		Run: func(c *harness.Context) error {
			m, err := newMatrix(c)
			if err != nil {
				return err
			}
			want := map[string]bool{"aarch64-apple-darwin": true, "x86_64-windows-msvc": true}
			var cfgs []Config
			for _, client := range []Lane{MacLane, WindowsLane} {
				cfgs = append(cfgs, configsFor(m.targets, client, func(t Target) bool { return want[t.Name] })...)
			}
			start := c.Now()
			results := m.run(cfgs, 4)
			failed := recordMatrix(c, results, start)
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
		ID: "T19", Title: "Windows client cross pattern: a Linux target and a Windows MinGW target, compiled on Linux, tested on their runners",
		Requires: []harness.Requirement{harness.RequiresAWS, harness.RequiresWindowsClient, harness.RequiresCrossMatrix, harness.RequiresCucinactl},
		Cost:     harness.CostMedium, EstimateUSD: 6, Essential: true, Timeout: 4 * time.Hour, DependsOn: []string{"T16"},
		NFRs: []string{"NFR-X1", "NFR-X2"}, Post: Guards,
		Run: func(c *harness.Context) error {
			m, err := newMatrix(c)
			if err != nil {
				return err
			}
			want := map[string]bool{"x86_64-linux-gnu": true, "x86_64-windows-gnu": true}
			cfgs := configsFor(m.targets, WindowsLane, func(t Target) bool { return want[t.Name] })
			start := c.Now()
			results := m.run(cfgs, 2)
			failed := recordMatrix(c, results, start)
			// Outcomes must match T16's for the same targets.
			if p, ok := c.Prior["T16"]; ok {
				b, _ := json.Marshal(p.Values["configurations"])
				var t16 []ConfigResult
				_ = json.Unmarshal(b, &t16)
				for _, r := range results {
					for _, o := range t16 {
						if o.Target == r.Target && o.Tests != nil && r.Tests != nil {
							res, _ := nfr.Outcomes(r.Config+" vs T16", r.Tests, o.Tests, nil)
							c.NFR(res)
						}
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
