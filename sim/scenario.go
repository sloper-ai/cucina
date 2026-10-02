// SPDX-License-Identifier: FSL-1.1-ALv2

// Package sim is the deterministic simulation of the Cucina controller
// (R-TEST-8c, R-SCALE-8): the autoscaler core (internal/scaling) runs against
// every fake (internal/fakes) on one manual clock and one seeded PRNG, driven
// by a scenario (fleet, workload trace, fault schedule, seed, SLO
// expectations). The shared invariants (invariants/) are checked against the
// fakes' ground truth after every step.
//
// Entry points: Load/Parse a scenario, Run it (or RunWith a policy), Sweep it
// over many seeds with random faults (failing seeds are minimised and written
// as regression scenarios), Replay a JSONL queue trace under several policies
// (cost vs wait), and Shadow-diff a candidate policy against the current one.
// cmd/simctl wraps them for the command line.
package sim

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/sloper-ai/cucina/internal/domain"
)

// Duration is a time.Duration written as a Go duration string ("5m").
type Duration time.Duration

// D returns the time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = Duration(v)
	return nil
}

// MarshalYAML implements yaml.Marshaler.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// Scenario is one simulation run description (sim/scenarios/*.yaml).
type Scenario struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
	// Seed drives every random choice (fake latencies, workload generators, sweeps).
	Seed uint64 `yaml:"seed"`
	// Duration of simulated time; Step is the controller poll interval (1 s).
	Duration Duration `yaml:"duration"`
	Step     Duration `yaml:"step,omitempty"`
	// Drain is extra simulated time after Duration with no new arrivals, so
	// the fleet can scale back to zero (default 30m).
	Drain        Duration     `yaml:"drain,omitempty"`
	Fleet        Fleet        `yaml:"fleet"`
	Workload     Workload     `yaml:"workload"`
	Faults       []Fault      `yaml:"faults,omitempty"`
	Expectations Expectations `yaml:"expectations"`

	dir string // directory of the scenario file (trace paths are relative to it)
}

// Fleet describes the pools, Mac hosts and the simulated provider behaviour.
type Fleet struct {
	ClusterID string      `yaml:"clusterId,omitempty"`
	Pools     []PoolDef   `yaml:"pools"`
	Hosts     []HostDef   `yaml:"hosts,omitempty"`
	Compute   ComputeDef  `yaml:"compute,omitempty"`
	HostFleet HostDefault `yaml:"hostFleet,omitempty"`
	// QueueFailAfter overrides the controller's capacity window (default 10m).
	QueueFailAfter Duration `yaml:"queueFailAfter,omitempty"`
}

// RunnerDef is one runner of a pool.
type RunnerDef struct {
	Name        string            `yaml:"name"`
	Properties  map[string]string `yaml:"properties"`
	Concurrency int               `yaml:"concurrency"`
}

// PoolDef is one WorkerPool.
type PoolDef struct {
	Name                 string         `yaml:"name"`
	Provider             string         `yaml:"provider"` // ec2 | tart
	Platform             string         `yaml:"platform,omitempty"`
	SizeClass            uint32         `yaml:"sizeClass,omitempty"`
	InstanceNames        []string       `yaml:"instanceNames,omitempty"`
	Runners              []RunnerDef    `yaml:"runners"`
	VCPUs                int            `yaml:"vcpus,omitempty"`
	MinRunning           int            `yaml:"minRunning,omitempty"`
	Max                  int            `yaml:"max"`
	IdleTimeout          Duration       `yaml:"idleTimeout"`
	DrainTimeout         Duration       `yaml:"drainTimeout,omitempty"`
	StartupTimeout       Duration       `yaml:"startupTimeout"`
	Generation           string         `yaml:"generation,omitempty"`
	Rollout              string         `yaml:"rollout,omitempty"`
	Paused               bool           `yaml:"paused,omitempty"`
	InstanceTypes        []string       `yaml:"instanceTypes,omitempty"`
	Subnets              []string       `yaml:"subnets,omitempty"`
	Image                ImageDef       `yaml:"image,omitempty"`
	Floors               []FloorDef     `yaml:"floors,omitempty"`
	DailyInstanceHourCap float64        `yaml:"dailyInstanceHourCap,omitempty"`
	VMsPerHost           int            `yaml:"vmsPerHost,omitempty"`
	FastLaunch           *FastLaunchDef `yaml:"fastLaunch,omitempty"`
	// Undeclared leaves the pool's queues out of the scheduler's predeclared
	// queues (a WorkerPool created outside Helm, ADR 0002).
	Undeclared bool `yaml:"undeclared,omitempty"`
}

// ImageDef is the pool's image.
type ImageDef struct {
	ID       string `yaml:"id,omitempty"`
	Platform string `yaml:"platform,omitempty"` // linux | windows (EC2 boot model)
}

// FastLaunchDef enables EC2 Fast Launch for a Windows pool image.
type FastLaunchDef struct {
	TargetCount int `yaml:"targetCount"`
	// Ready pre-provisions the snapshots before the run starts.
	Ready bool `yaml:"ready,omitempty"`
}

// FloorDef is a WorkerPool floor window.
type FloorDef struct {
	Name       string   `yaml:"name"`
	Days       []string `yaml:"days"`
	Start      string   `yaml:"start"`
	End        string   `yaml:"end"`
	MinRunning int      `yaml:"minRunning"`
}

// HostDef is one Mac host.
type HostDef struct {
	Serial string            `yaml:"serial"`
	Slots  int               `yaml:"slots,omitempty"`
	Labels map[string]string `yaml:"labels,omitempty"`
}

// ComputeDef overrides the fake EC2 behaviour (zero fields keep the defaults).
type ComputeDef struct {
	PendingMin       Duration           `yaml:"pendingMin,omitempty"`
	PendingMax       Duration           `yaml:"pendingMax,omitempty"`
	BootMin          Duration           `yaml:"bootMin,omitempty"`
	BootMax          Duration           `yaml:"bootMax,omitempty"`
	WindowsBootFast  Duration           `yaml:"windowsBootFast,omitempty"`
	WindowsBootSlow  Duration           `yaml:"windowsBootSlow,omitempty"`
	VisibleMax       Duration           `yaml:"visibleMax,omitempty"`
	VCPUQuota        int                `yaml:"vcpuQuota,omitempty"`
	LaunchBurst      float64            `yaml:"launchBurst,omitempty"`
	LaunchRate       float64            `yaml:"launchRate,omitempty"`
	FastLaunchRefill Duration           `yaml:"fastLaunchRefill,omitempty"`
	Prices           map[string]float64 `yaml:"prices,omitempty"`
}

// HostDefault overrides the fake host fleet behaviour.
type HostDefault struct {
	BootMin Duration `yaml:"bootMin,omitempty"`
	BootMax Duration `yaml:"bootMax,omitempty"`
}

// Workload is the arriving work: inline arrivals, generators and/or a JSONL trace.
type Workload struct {
	Arrivals   []Arrival   `yaml:"arrivals,omitempty"`
	Generators []Generator `yaml:"generators,omitempty"`
	// Trace is a JSONL file (relative to the scenario file) of TraceRecord lines.
	Trace string `yaml:"trace,omitempty"`
}

// Arrival submits Count actions starting At, Every apart, each running Duration.
type Arrival struct {
	At       Duration `yaml:"at"`
	Pool     string   `yaml:"pool"`
	Runner   string   `yaml:"runner,omitempty"` // default: the pool's first runner
	Count    int      `yaml:"count,omitempty"`  // default 1
	Every    Duration `yaml:"every,omitempty"`
	Duration Duration `yaml:"duration"`
}

// Generator submits actions as a Poisson process of Rate per minute between
// From and Until, durations uniform in [MinDuration, MaxDuration].
type Generator struct {
	Pool        string   `yaml:"pool"`
	Runner      string   `yaml:"runner,omitempty"`
	RatePerMin  float64  `yaml:"ratePerMin"`
	From        Duration `yaml:"from,omitempty"`
	Until       Duration `yaml:"until"`
	MinDuration Duration `yaml:"minDuration"`
	MaxDuration Duration `yaml:"maxDuration"`
	// Burst submits this many actions per arrival event (default 1).
	Burst int `yaml:"burst,omitempty"`
}

// TraceRecord is one line of a queue trace (R-TEST-8c): an anonymised action
// as exported by the controller.
type TraceRecord struct {
	At        float64 `json:"at"`       // seconds since the start of the trace
	Platform  string  `json:"platform"` // pool platform (PoolDef.Platform or Name)
	Runner    string  `json:"runner,omitempty"`
	SizeClass uint32  `json:"sizeClass,omitempty"`
	Duration  float64 `json:"duration"` // seconds of execution
}

// Fault kinds.
const (
	FaultICE               = "ice"                // Type (or every type of Pool) has no capacity in AZ (or every AZ)
	FaultQuota             = "quota"              // vCPU quota set to Value
	FaultThrottle          = "throttle"           // RunInstances bucket burst Value, refill Rate
	FaultAPIError          = "api-error"          // Op fails with an ambiguous error with probability Value (default 1)
	FaultControllerRestart = "controller-restart" // planner state lost; MidScale: results of the last decision lost too
	FaultWorkerDeath       = "worker-death"       // Count workers of Pool lose their scheduler connection (VM stays up)
	FaultSpotInterruption  = "spot-interruption"  // Count instances of Pool are terminated by EC2
	FaultNetworkCut        = "network-cut"        // the scheduler is unreachable for Duration
	FaultSchedulerRestart  = "scheduler-restart"  // in-memory scheduler state lost; workers reconnect
	FaultHostOffline       = "host-offline"       // Host disconnected for Duration
	FaultHostReboot        = "host-reboot"        // Host's VMs stop
	FaultImageMissing      = "image-missing"      // Pool's image cannot be resolved for Duration
	FaultStartFailure      = "start-failure"      // VMs of Pool launched during Duration never register
	FaultLeak              = "leak"               // terminated instances leak volumes with probability Value for Duration
	FaultRollout           = "rollout"            // operator event: Pool moves to generation Op (default "<gen>-next") with rollout policy Type (lazy|eager)
	FaultDelete            = "delete"             // operator event: Pool is deleted (finalizer: drain and terminate everything, R-OPS-3)
)

// Fault is one scheduled fault.
type Fault struct {
	At       Duration `yaml:"at"`
	Kind     string   `yaml:"kind"`
	Duration Duration `yaml:"duration,omitempty"`
	Pool     string   `yaml:"pool,omitempty"`
	Type     string   `yaml:"type,omitempty"`
	AZ       string   `yaml:"az,omitempty"`
	Host     string   `yaml:"host,omitempty"`
	Op       string   `yaml:"op,omitempty"`
	Count    int      `yaml:"count,omitempty"`
	Value    float64  `yaml:"value,omitempty"`
	Rate     float64  `yaml:"rate,omitempty"`
	MidScale bool     `yaml:"midScale,omitempty"`
}

// Expectations are the SLO bounds of a scenario (zero = not checked).
type Expectations struct {
	// QueueWaitP50/P95/Max bound the wait of actions (arrival → first start of the attempt that succeeded).
	QueueWaitP50 Duration `yaml:"queueWaitP50,omitempty"`
	QueueWaitP95 Duration `yaml:"queueWaitP95,omitempty"`
	QueueWaitMax Duration `yaml:"queueWaitMax,omitempty"`
	// MaxInstances bounds the live instances/VMs per pool at any step (≤ max is an invariant anyway).
	MaxInstances map[string]int `yaml:"maxInstances,omitempty"`
	// MaxLaunches bounds the launches per pool (cost-runaway guard).
	MaxLaunches map[string]int `yaml:"maxLaunches,omitempty"`
	// MaxCostUSD bounds the EC2 cost of the run.
	MaxCostUSD float64 `yaml:"maxCostUSD,omitempty"`
	// AllSucceed requires every action to complete successfully (after client retries).
	AllSucceed bool `yaml:"allSucceed,omitempty"`
	// MinFailed requires at least this many actions to be failed fast (fail-fast scenarios).
	MinFailed int `yaml:"minFailed,omitempty"`
	// MaxFailWait bounds how long a failed action waited before failing (fail fast, R-RE-2).
	MaxFailWait Duration `yaml:"maxFailWait,omitempty"`
	// EndAtZero requires zero live instances and VMs at the end (scale to zero, NFR-C1).
	EndAtZero bool `yaml:"endAtZero,omitempty"`
	// MaxIdleWithEmptyQueue bounds how long any VM stays idle while its pool's queues are empty (idleTimeout + drain grace, NFR-C1).
	MaxIdleWithEmptyQueue Duration `yaml:"maxIdleWithEmptyQueue,omitempty"`
}

// Load reads a scenario file (strict: unknown fields are an error).
func Load(path string) (*Scenario, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sc, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	sc.dir = filepath.Dir(path)
	return sc, nil
}

// Parse decodes and validates a scenario.
func Parse(b []byte) (*Scenario, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var sc Scenario
	if err := dec.Decode(&sc); err != nil {
		return nil, err
	}
	if err := sc.validate(); err != nil {
		return nil, err
	}
	return &sc, nil
}

// Marshal encodes a scenario as YAML.
func (sc *Scenario) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(sc); err != nil {
		return nil, err
	}
	return buf.Bytes(), enc.Close()
}

// Clone returns a deep copy (via YAML).
func (sc *Scenario) Clone() *Scenario {
	b, err := sc.Marshal()
	if err != nil {
		panic(err)
	}
	var c Scenario
	if err := yaml.Unmarshal(b, &c); err != nil {
		panic(err)
	}
	c.dir = sc.dir
	return &c
}

func (sc *Scenario) validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if sc.Name == "" {
		add("name is required")
	}
	if sc.Duration <= 0 {
		add("duration must be > 0")
	}
	if len(sc.Fleet.Pools) == 0 {
		add("fleet.pools is empty")
	}
	pools := map[string]PoolDef{}
	for _, p := range sc.Fleet.Pools {
		if _, dup := pools[p.Name]; dup || p.Name == "" {
			add("pool name %q empty or duplicated", p.Name)
		}
		pools[p.Name] = p
		if p.Provider != string(domain.ProviderEC2) && p.Provider != string(domain.ProviderTart) {
			add("pool %s: provider %q", p.Name, p.Provider)
		}
		if len(p.Runners) == 0 {
			add("pool %s: no runners", p.Name)
		}
		if p.Provider == string(domain.ProviderEC2) && len(p.InstanceTypes) == 0 {
			add("pool %s: no instanceTypes", p.Name)
		}
		if p.IdleTimeout <= 0 || p.StartupTimeout <= 0 {
			add("pool %s: idleTimeout and startupTimeout are required", p.Name)
		}
	}
	if slices.ContainsFunc(sc.Fleet.Pools, func(p PoolDef) bool { return p.Provider == string(domain.ProviderTart) }) && len(sc.Fleet.Hosts) == 0 {
		add("tart pools need fleet.hosts")
	}
	runnerOK := func(pool, runner string) bool {
		p, ok := pools[pool]
		return ok && (runner == "" || slices.ContainsFunc(p.Runners, func(r RunnerDef) bool { return r.Name == runner }))
	}
	for i, a := range sc.Workload.Arrivals {
		if !runnerOK(a.Pool, a.Runner) {
			add("workload.arrivals[%d]: unknown pool/runner %s/%s", i, a.Pool, a.Runner)
		}
	}
	for i, g := range sc.Workload.Generators {
		if !runnerOK(g.Pool, g.Runner) || g.RatePerMin <= 0 || g.Until <= g.From || g.MaxDuration < g.MinDuration {
			add("workload.generators[%d]: invalid", i)
		}
	}
	known := []string{FaultICE, FaultQuota, FaultThrottle, FaultAPIError, FaultControllerRestart, FaultWorkerDeath, FaultSpotInterruption,
		FaultNetworkCut, FaultSchedulerRestart, FaultHostOffline, FaultHostReboot, FaultImageMissing, FaultStartFailure, FaultLeak,
		FaultRollout, FaultDelete}
	for i, f := range sc.Faults {
		if !slices.Contains(known, f.Kind) {
			add("faults[%d]: unknown kind %q", i, f.Kind)
		}
		if f.Pool != "" {
			if _, ok := pools[f.Pool]; !ok {
				add("faults[%d]: unknown pool %q", i, f.Pool)
			}
		}
	}
	return errors.Join(errs...)
}

// LoadTrace reads a JSONL queue trace.
func LoadTrace(path string) ([]TraceRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []TraceRecord
	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(text))
		dec.DisallowUnknownFields()
		var r TraceRecord
		if err := dec.Decode(&r); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		if r.At < 0 || r.Duration < 0 || r.Platform == "" {
			return nil, fmt.Errorf("%s:%d: invalid record", path, line)
		}
		out = append(out, r)
	}
	return out, sc.Err()
}
