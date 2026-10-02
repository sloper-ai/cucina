// SPDX-License-Identifier: FSL-1.1-ALv2

// Command simctl runs the deterministic controller simulation (R-TEST-8c).
//
//	simctl run    -scenario sim/scenarios/burst.yaml [-seed N] [-policy P] [-log 200]
//	simctl sweep  -scenario sim/scenarios/burst.yaml -seeds 1000 [-chaos] [-regressions sim/scenarios/regressions]
//	simctl replay -scenario sim/scenarios/replay-sample.yaml -policy default -policy idle2m:idle=2m
//	simctl shadow -scenario sim/scenarios/oscillation.yaml -policy default -candidate idle2m:idle=2m
//
// Policies are "default" or "<name>:<key>=<duration>,…" (keys: idle, startup,
// drain, queueFailAfter, backoffMin, backoffMax, cooldown, grace).
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/sloper-ai/cucina/sim"
)

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(s string) error { *m = append(*m, s); return nil }

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: simctl run|sweep|replay|shadow -scenario FILE [flags]")
		os.Exit(2)
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	scenario := fs.String("scenario", "", "scenario YAML file")
	seed := fs.Uint64("seed", 0, "override the scenario seed (run)")
	seeds := fs.Int("seeds", 100, "number of seeds (sweep)")
	first := fs.Uint64("first-seed", 1, "first seed (sweep)")
	chaos := fs.Bool("chaos", false, "add random faults per seed (sweep, run)")
	regressions := fs.String("regressions", "", "write minimised failing seeds here (sweep)")
	logLines := fs.Int("log", 0, "print the last N decision log lines (run)")
	candidate := fs.String("candidate", "", "candidate policy (shadow)")
	var policies multi
	fs.Var(&policies, "policy", "policy (repeatable for replay)")
	_ = fs.Parse(os.Args[2:])
	if *scenario == "" {
		fail("-scenario is required")
	}
	sc, err := sim.Load(*scenario)
	if err != nil {
		fail(err.Error())
	}
	if len(policies) == 0 {
		policies = multi{"default"}
	}
	parse := func(s string) sim.Policy {
		p, err := sim.ParsePolicy(s)
		if err != nil {
			fail(err.Error())
		}
		return p
	}
	switch cmd {
	case "run":
		if *seed != 0 {
			sc.Seed = *seed
		}
		if *chaos {
			sc = sim.Variant(sc, sc.Seed, true)
		}
		res := sim.RunWith(sc, sim.Options{Policy: parse(policies[0]), LogLines: *logLines})
		for _, l := range res.Log {
			fmt.Println(l)
		}
		fmt.Println(res.Summary())
		fmt.Print(res.Problems())
		if !res.Passed {
			os.Exit(1)
		}
	case "sweep":
		rep := sim.Sweep(sc, sim.SweepOptions{Seeds: *seeds, FirstSeed: *first, Chaos: *chaos, Policy: parse(policies[0]), RegressionDir: *regressions})
		fmt.Println(rep)
		for _, r := range rep.Failed[:min(len(rep.Failed), 10)] {
			fmt.Println(r.Summary())
			fmt.Print(r.Problems())
		}
		for _, p := range rep.Written {
			fmt.Println("wrote", p)
		}
		if len(rep.Failed) > 0 {
			os.Exit(1)
		}
	case "replay":
		var ps []sim.Policy
		for _, s := range policies {
			ps = append(ps, parse(s))
		}
		for _, row := range sim.Replay(sc, ps) {
			fmt.Println(row)
		}
	case "shadow":
		if *candidate == "" {
			fail("-candidate is required")
		}
		res := sim.Shadow(sc, parse(policies[0]), parse(*candidate))
		fmt.Println(res.Summary())
		fmt.Printf("decisions differing: %d\n", res.Metrics.ShadowDiffSteps)
		for _, s := range res.Metrics.ShadowSamples {
			fmt.Println("  " + s)
		}
	default:
		fail("unknown command " + cmd)
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "simctl:", msg)
	os.Exit(2)
}
