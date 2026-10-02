// SPDX-License-Identifier: FSL-1.1-ALv2

package sim

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/fakes"
)

// SweepOptions configure a seed sweep (R-TEST-8c: nightly 1,000 seeds).
type SweepOptions struct {
	Seeds     int
	FirstSeed uint64
	// Parallel runs (default GOMAXPROCS); results do not depend on it.
	Parallel int
	// Chaos adds a random fault schedule per seed on top of the scenario's
	// own faults; only invariants and liveness are then judged (the SLO bounds
	// of the scenario assume its own fault schedule).
	Chaos  bool
	Policy Policy
	// RegressionDir receives the minimised failing seeds as scenario files
	// ("" = do not write). At most MaxRegressions are written (default 5).
	RegressionDir  string
	MaxRegressions int
}

// SweepReport summarises a sweep.
type SweepReport struct {
	Scenario string
	Runs     int
	Failed   []Result // failing runs, in seed order
	Written  []string // regression files written
}

func (r SweepReport) String() string {
	return fmt.Sprintf("%s: %d runs, %d failed, %d regressions written", r.Scenario, r.Runs, len(r.Failed), len(r.Written))
}

// Variant returns the scenario as run for one sweep seed.
func Variant(sc *Scenario, seed uint64, chaos bool) *Scenario {
	v := sc.Clone()
	v.Seed = seed
	if chaos {
		v.Faults = append(v.Faults, ChaosFaults(v, seed)...)
		v.Expectations = Expectations{EndAtZero: sc.Expectations.EndAtZero, AllSucceed: false,
			MaxInstances: sc.Expectations.MaxInstances}
		v.Name = sc.Name + "-chaos"
	}
	return v
}

// judge applies the sweep's pass criterion: the run's own verdict, plus
// liveness in chaos mode (no action may be left hanging: it completes or is
// failed fast, UC27).
func judge(res Result, chaos bool) Result {
	if chaos && res.Metrics.Unfinished > 0 {
		res.Failures = append(res.Failures, fmt.Sprintf("%d actions unfinished (hang)", res.Metrics.Unfinished))
		res.Passed = false
	}
	return res
}

// Sweep runs a scenario over many seeds, minimises failing seeds and writes
// them as regression scenarios.
func Sweep(sc *Scenario, o SweepOptions) SweepReport {
	if o.Parallel <= 0 {
		o.Parallel = runtime.GOMAXPROCS(0)
	}
	if o.Policy.Name == "" {
		o.Policy = DefaultPolicy()
	}
	if o.MaxRegressions <= 0 {
		o.MaxRegressions = 5
	}
	results := make([]Result, o.Seeds)
	var wg sync.WaitGroup
	jobs := make(chan int)
	for w := 0; w < o.Parallel; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				v := Variant(sc, o.FirstSeed+uint64(i), o.Chaos)
				results[i] = judge(RunWith(v, Options{Policy: o.Policy}), o.Chaos)
			}
		}()
	}
	for i := 0; i < o.Seeds; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	rep := SweepReport{Scenario: sc.Name, Runs: o.Seeds}
	for _, r := range results {
		if !r.Passed {
			rep.Failed = append(rep.Failed, r)
		}
	}
	if o.RegressionDir == "" {
		return rep
	}
	for _, r := range rep.Failed[:min(len(rep.Failed), o.MaxRegressions)] {
		v := Variant(sc, r.Seed, o.Chaos)
		min := Minimize(v, Signature(r), func(c *Scenario) Result { return judge(RunWith(c, Options{Policy: o.Policy}), o.Chaos) })
		path, err := WriteRegression(min, o.RegressionDir, r)
		if err == nil {
			rep.Written = append(rep.Written, path)
		}
	}
	return rep
}

var numbers = regexp.MustCompile(`[0-9]+(\.[0-9]+)?`)

// Signature is the set of problem kinds of a failing run: invariant names and
// expectation messages with every number replaced by "#" (so the kind stays
// stable across seeds), used to keep minimisation on the same failure.
func Signature(r Result) []string {
	var sig []string
	for _, v := range r.Violations {
		sig = append(sig, string(v.Invariant))
	}
	for _, f := range r.Failures {
		sig = append(sig, numbers.ReplaceAllString(f, "#"))
	}
	slices.Sort(sig)
	return slices.Compact(sig)
}

func sameFailure(want []string, r Result) bool {
	if r.Passed {
		return false
	}
	for _, s := range Signature(r) {
		if slices.Contains(want, s) {
			return true
		}
	}
	return false
}

// Minimize shrinks a failing scenario while it keeps failing the same way:
// drop faults, drop or shrink arrivals and generators, then shorten the run.
func Minimize(sc *Scenario, sig []string, run func(*Scenario) Result) *Scenario {
	cur := sc.Clone()
	fails := func(c *Scenario) bool { return sameFailure(sig, run(c)) }
	for i := 0; i < len(cur.Faults); {
		c := cur.Clone()
		c.Faults = slices.Delete(c.Faults, i, i+1)
		if fails(c) {
			cur = c
		} else {
			i++
		}
	}
	for i := 0; i < len(cur.Workload.Arrivals); {
		c := cur.Clone()
		c.Workload.Arrivals = slices.Delete(c.Workload.Arrivals, i, i+1)
		if len(c.Workload.Arrivals)+len(c.Workload.Generators) > 0 && fails(c) {
			cur = c
			continue
		}
		for cur.Workload.Arrivals[i].Count > 1 {
			c := cur.Clone()
			c.Workload.Arrivals[i].Count /= 2
			if !fails(c) {
				break
			}
			cur = c
		}
		i++
	}
	for i := 0; i < len(cur.Workload.Generators); {
		c := cur.Clone()
		c.Workload.Generators = slices.Delete(c.Workload.Generators, i, i+1)
		if len(c.Workload.Arrivals)+len(c.Workload.Generators) > 0 && fails(c) {
			cur = c
		} else {
			i++
		}
	}
	for cur.Duration > Duration(2*time.Minute) {
		c := cur.Clone()
		c.Duration /= 2
		if !fails(c) {
			break
		}
		cur = c
	}
	return cur
}

// WriteRegression writes a minimised failing scenario into dir and returns its path.
func WriteRegression(sc *Scenario, dir string, from Result) (string, error) {
	c := sc.Clone()
	c.Name = fmt.Sprintf("%s-seed%d", strings.TrimSuffix(sc.Name, "-chaos"), from.Seed)
	c.Description = fmt.Sprintf("Minimised by the simulation sweep from seed %d (red-first regression: it must pass once the bug is fixed). "+
		"Original problems: %s", from.Seed, strings.Join(Signature(from), "; "))
	if c.Workload.Trace != "" && c.dir != "" && !filepath.IsAbs(c.Workload.Trace) {
		if rel, err := filepath.Rel(dir, filepath.Join(c.dir, c.Workload.Trace)); err == nil {
			c.Workload.Trace = rel
		}
	}
	b, err := c.Marshal()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, c.Name+".yaml")
	return path, os.WriteFile(path, append([]byte("# SPDX-License-Identifier: FSL-1.1-ALv2\n"), b...), 0o644)
}

// ChaosFaults draws a random fault schedule for a scenario from seed: 0–4
// faults of the kinds that apply to its fleet, at random times, for random
// durations.
func ChaosFaults(sc *Scenario, seed uint64) []Fault {
	r := fakes.NewRand(seed ^ 0xc4a05c4a05)
	var ec2, tart []PoolDef
	for _, p := range sc.Fleet.Pools {
		if p.Provider == string(domain.ProviderTart) {
			tart = append(tart, p)
		} else {
			ec2 = append(ec2, p)
		}
	}
	kinds := []string{FaultControllerRestart, FaultWorkerDeath, FaultNetworkCut, FaultSchedulerRestart}
	if len(ec2) > 0 {
		kinds = append(kinds, FaultICE, FaultThrottle, FaultAPIError, FaultSpotInterruption, FaultLeak, FaultQuota, FaultStartFailure)
	}
	if len(tart) > 0 && len(sc.Fleet.Hosts) > 0 {
		kinds = append(kinds, FaultHostOffline, FaultHostReboot)
	}
	pick := func(xs []PoolDef) PoolDef { return xs[r.Int63n(int64(len(xs)))] }
	n := int(r.Int63n(5))
	var out []Fault
	for i := 0; i < n; i++ {
		f := Fault{Kind: kinds[r.Int63n(int64(len(kinds)))], At: Duration(r.Duration(0, sc.Duration.D())),
			Duration: Duration(r.Duration(30*time.Second, 10*time.Minute))}
		all := append(slices.Clone(ec2), tart...)
		switch f.Kind {
		case FaultICE:
			p := pick(ec2)
			f.Pool = p.Name
			if r.Float64() < 0.5 && len(p.InstanceTypes) > 0 {
				f.Type = p.InstanceTypes[r.Int63n(int64(len(p.InstanceTypes)))]
				f.Pool = ""
			}
		case FaultThrottle:
			f.Value, f.Rate = 1, 0.1
		case FaultAPIError:
			f.Op = []string{"Launch", "Describe", "Terminate"}[r.Int63n(3)]
			f.Value = 0.2 + 0.6*r.Float64()
		case FaultSpotInterruption, FaultWorkerDeath, FaultStartFailure:
			p := pick(all)
			if f.Kind != FaultWorkerDeath {
				p = pick(ec2)
			}
			f.Pool, f.Count = p.Name, 1+int(r.Int63n(2))
			if f.Kind == FaultStartFailure {
				f.Duration = Duration(r.Duration(30*time.Second, 3*time.Minute))
			}
		case FaultLeak:
			f.Value = 0.5
		case FaultQuota:
			f.Value = float64(16 * (1 + r.Int63n(3)))
		case FaultControllerRestart:
			f.MidScale = r.Float64() < 0.5
		case FaultNetworkCut:
			f.Duration = Duration(r.Duration(10*time.Second, 3*time.Minute))
		case FaultHostOffline, FaultHostReboot:
			f.Host = sc.Fleet.Hosts[r.Int63n(int64(len(sc.Fleet.Hosts)))].Serial
		}
		out = append(out, f)
	}
	return out
}
