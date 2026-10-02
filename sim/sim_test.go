// SPDX-License-Identifier: FSL-1.1-ALv2

package sim_test

import (
	"flag"
	"path/filepath"
	"testing"
	"time"

	"github.com/sloper-ai/cucina/sim"
)

// TestScenarios runs every scenario in sim/scenarios with its fixed seed
// (R-SCALE-8, R-TEST-8c smoke): no invariant may be violated at any step and
// every SLO expectation of the scenario must hold.
func TestScenarios(t *testing.T) {
	runDir(t, "scenarios", true)
}

// TestRegressions runs the minimised failing seeds written by the sweep.
func TestRegressions(t *testing.T) {
	runDir(t, filepath.Join("scenarios", "regressions"), false)
}

func runDir(t *testing.T, dir string, required bool) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if required && len(files) == 0 {
		t.Fatalf("no scenarios in %s (missing data dependency?)", dir)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			sc, err := sim.Load(f)
			if err != nil {
				t.Fatal(err)
			}
			res := sim.RunWith(sc, sim.Options{Policy: sim.DefaultPolicy(), LogLines: 60})
			t.Log(res.Summary())
			if !res.Passed {
				for _, l := range res.Log {
					t.Log(l)
				}
				t.Fatalf("scenario %s failed:\n%s", sc.Name, res.Problems())
			}
		})
	}
}

var (
	sweepSeeds = flag.Int("sweep.seeds", 0, "TestSweep: seeds per scenario (0 = skip; nightly: 1000)")
	sweepChaos = flag.Bool("sweep.chaos", true, "TestSweep: add random faults per seed")
	sweepWrite = flag.Bool("sweep.write", false, "TestSweep: write minimised failing seeds to scenarios/regressions")
)

// TestSweep is the simulation tier (R-TEST-8c): every scenario over many
// seeds with random fault schedules; invariants are checked after every step
// and no action may hang. Failing seeds are minimised and, with -sweep.write,
// committed as regression scenarios (red-first).
//
//	go test ./sim -run TestSweep -sweep.seeds=1000
func TestSweep(t *testing.T) {
	if *sweepSeeds <= 0 {
		t.Skip("set -sweep.seeds=N (simulation tier)")
	}
	files, err := filepath.Glob(filepath.Join("scenarios", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no scenarios found (missing data dependency?)")
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			sc, err := sim.Load(f)
			if err != nil {
				t.Fatal(err)
			}
			o := sim.SweepOptions{Seeds: *sweepSeeds, FirstSeed: 1, Chaos: *sweepChaos}
			if *sweepWrite {
				o.RegressionDir = filepath.Join("scenarios", "regressions")
			}
			rep := sim.Sweep(sc, o)
			t.Log(rep)
			for _, w := range rep.Written {
				t.Log("wrote", w)
			}
			for _, r := range rep.Failed[:min(len(rep.Failed), 3)] {
				t.Errorf("%s\n%s", r.Summary(), r.Problems())
			}
		})
	}
}

// TestShadowMode guards R-TEST-7 shadow mode: a candidate policy decides on
// a copy of the state without acting, so an identical candidate never differs
// and a different one is detected without changing the outcome of the run.
func TestShadowMode(t *testing.T) {
	sc, err := sim.Load(filepath.Join("scenarios", "burst.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	plain := sim.Run(sc)
	same := sim.Shadow(sc, sim.DefaultPolicy(), sim.DefaultPolicy())
	if same.Metrics.ShadowDiffSteps != 0 {
		t.Fatalf("identical candidate differed in %d decisions: %v", same.Metrics.ShadowDiffSteps, same.Metrics.ShadowSamples)
	}
	cand, err := sim.ParsePolicy("idle1m:idle=1m")
	if err != nil {
		t.Fatal(err)
	}
	diff := sim.Shadow(sc, sim.DefaultPolicy(), cand)
	if diff.Metrics.ShadowDiffSteps == 0 {
		t.Fatal("a 1-minute idle timeout should scale in earlier than the 5-minute default")
	}
	if diff.Summary() != plain.Summary() || same.Summary() != plain.Summary() {
		t.Fatalf("shadow planning changed the run:\n%s\n%s\n%s", plain.Summary(), same.Summary(), diff.Summary())
	}
}

// TestMinimize guards the sweep's minimiser (R-TEST-8c): a failing scenario
// shrinks to the faults and work that matter while failing the same way, and
// is written as a loadable regression scenario.
func TestMinimize(t *testing.T) {
	sc, err := sim.Load(filepath.Join("scenarios", "burst.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	sc.Duration = sim.Duration(10 * time.Minute)
	sc.Expectations = sim.Expectations{MaxLaunches: map[string]int{"linux-x86-64": 1}} // fails: the burst needs 4 VMs
	sc.Faults = []sim.Fault{{At: sim.Duration(time.Minute), Kind: sim.FaultSchedulerRestart},
		{At: sim.Duration(2 * time.Minute), Kind: sim.FaultNetworkCut, Duration: sim.Duration(30 * time.Second)}}
	sc.Workload.Arrivals = append(sc.Workload.Arrivals, sim.Arrival{At: sim.Duration(5 * time.Minute), Pool: "linux-x86-64",
		Count: 3, Duration: sim.Duration(time.Second)})
	run := func(c *sim.Scenario) sim.Result { return sim.Run(c) }
	first := run(sc)
	if first.Passed {
		t.Fatal("the scenario must fail before minimisation")
	}
	min := sim.Minimize(sc, sim.Signature(first), run)
	if res := run(min); res.Passed {
		t.Fatalf("minimised scenario passes: %s", res.Summary())
	}
	if len(min.Faults) != 0 || len(min.Workload.Arrivals) != 1 {
		t.Fatalf("irrelevant faults/arrivals kept: %d faults, %d arrivals", len(min.Faults), len(min.Workload.Arrivals))
	}
	if min.Workload.Arrivals[0].Count >= sc.Workload.Arrivals[0].Count || min.Duration >= sc.Duration {
		t.Fatalf("not shrunk: count %d, duration %s", min.Workload.Arrivals[0].Count, min.Duration.D())
	}
	path, err := sim.WriteRegression(min, t.TempDir(), first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sim.Load(path); err != nil {
		t.Fatalf("written regression does not load: %v", err)
	}
}
