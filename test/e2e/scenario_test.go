// SPDX-License-Identifier: FSL-1.1-ALv2

// Package e2e is the entry point of the scenario harness (R-TEST-8d):
//
//	go test ./test/e2e -run TestScenario -args -env aws-e2e -id T1
//	bazel run //test/e2e:scenario -- --env=aws-e2e --id=T1
//
// -id takes a comma-separated list; -id all runs the campaign order. Results
// land in <artifactsDir>/<runId>/results (override with -results).
// Without -env (or $CUCINA_E2E_ENV) the test skips: scenarios need a real
// environment and never run in `bazel test //...` (tier acceptance: manual).
package e2e

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/sloper-ai/cucina/test/e2e/harness"
	"github.com/sloper-ai/cucina/test/e2e/infra"
	"github.com/sloper-ai/cucina/test/e2e/scenarios"
)

var (
	envFlag     = flag.String("env", os.Getenv("CUCINA_E2E_ENV"), "environment descriptor name (~/.config/cucina/e2e/<name>.json) or path")
	idFlag      = flag.String("id", "", "comma-separated scenario IDs, or \"all\" for the campaign order")
	resultsFlag = flag.String("results", "", "results directory (default <artifactsDir>/<runId>/results)")
	reserveFlag = flag.Float64("reserve-usd", 25, "budget reserve for the standing environment and teardown")
)

func TestScenario(t *testing.T) {
	if *envFlag == "" {
		t.Skip("no environment: pass -env <name> (or set CUCINA_E2E_ENV); scenarios run only against a real environment")
	}
	if *idFlag == "" {
		t.Fatal("pass -id <ID[,ID…]> or -id all")
	}
	env, err := harness.LoadEnv(*envFlag)
	if err != nil {
		t.Fatal(err)
	}
	reg := harness.NewRegistry()
	scenarios.Register(reg)
	ids := strings.Split(*idFlag, ",")
	if *idFlag == "all" {
		ids = reg.IDs()
	}
	results := *resultsFlag
	if results == "" {
		results = filepath.Join(env.ArtifactsDir, env.RunID, "results")
	}
	budget := env.Safety.MaxSpendUSD
	if budget <= 0 {
		budget = harness.DefaultBudgetUSD
	}
	gov, err := harness.OpenGovernor(filepath.Join(results, "budget.json"), budget, *reserveFlag)
	if err != nil {
		t.Fatal(err)
	}
	runner := &harness.Runner{
		Registry: reg, Env: env, Governor: gov, NewServices: infra.New, ResultsDir: results,
		Log: slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
	defer func() { _ = runner.Close() }()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for _, id := range ids {
		res, err := runner.Run(ctx, strings.TrimSpace(id))
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		switch res.Status {
		case harness.StatusFunctionalPass:
			t.Logf("%s FUNCTIONAL PASS in %s (ADR0004; original NFR-P2 unqualified)", res.ID, res.Duration)
		case harness.StatusPass:
			t.Logf("%s PASS in %s", res.ID, res.Duration)
		case harness.StatusSkip:
			t.Logf("%s SKIP: %s", res.ID, res.SkipReason)
		default:
			t.Errorf("%s %s: %s", res.ID, strings.ToUpper(string(res.Status)), res.Error)
		}
	}
}
