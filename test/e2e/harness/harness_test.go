// SPDX-License-Identifier: FSL-1.1-ALv2

package harness

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func awsEnv() *Env {
	return &Env{
		Name: "aws-e2e", Kind: EnvAWS, RunID: "e2e-test",
		Capabilities: []Requirement{RequiresAWS, RequiresKubernetes, RequiresLinuxClient, RequiresDestructive, RequiresMacHost},
		Kubernetes:   &KubeEnv{Namespace: "cucina", Release: "cucina"},
		AWS:          &AWSEnv{Region: "us-west-1", Tags: map[string]string{"cucina:env": "e2e", "cucina:run": "e2e-test"}},
		Clients:      map[string]ClientEnv{"linux-client": {OS: "linux", InstanceID: "i-0"}},
		Safety:       Safety{MaxSpendUSD: 300, MaxInstances: 12, AllowDestructive: true},
	}
}

func scenario(mut func(*Scenario)) *Scenario {
	s := &Scenario{ID: "T1", Title: "t", Timeout: time.Minute, Cost: CostLow, Run: func(*Context) error { return nil }}
	if mut != nil {
		mut(s)
	}
	return s
}

// Guards R-TEST-8d: "an unmet prerequisite yields an explicit SKIP with a
// reason", the environment's safety limits, prod-smoke being read-only, and
// §12's budget governor.
func TestPreflightSkipsWithReason(t *testing.T) {
	smoke := awsEnv()
	smoke.Kind, smoke.Safety.AllowDestructive = EnvProdSmoke, false
	noWin := awsEnv()
	overBudget := NewTestGovernor(290)

	for _, tc := range []struct {
		name   string
		env    *Env
		gov    *Governor
		s      *Scenario
		prior  map[string]*Result
		reason string // substring; "" = runs
	}{
		{name: "all prerequisites met", env: awsEnv(), s: scenario(func(s *Scenario) { s.Requires = []Requirement{RequiresAWS, RequiresLinuxClient} })},
		{name: "missing client", env: noWin, s: scenario(func(s *Scenario) { s.Requires = []Requirement{RequiresWindowsClient} }), reason: "requires windows-client"},
		{name: "missing idp", env: awsEnv(), s: scenario(func(s *Scenario) { s.Requires = []Requirement{RequiresIdP} }), reason: "requires idp"},
		{name: "wrong environment kind", env: awsEnv(), s: scenario(func(s *Scenario) { s.Envs = []EnvKind{EnvKindCluster} }), reason: "does not run in aws-e2e"},
		{name: "prod-smoke needs read-only", env: smoke, s: scenario(func(s *Scenario) {
			s.Envs = []EnvKind{EnvAWS, EnvProdSmoke}
			s.ReadOnly = true
			s.Requires = []Requirement{RequiresDestructive}
		}), reason: "requires destructive"},
		{name: "too many instances", env: awsEnv(), s: scenario(func(s *Scenario) { s.MaxInstances = 13 }), reason: "above the safety limit of 12"},
		{name: "dependency not run", env: awsEnv(), s: scenario(func(s *Scenario) { s.ID, s.DependsOn = "T2", []string{"T1"} }), reason: "depends on T1, which has not run"},
		{name: "dependency failed", env: awsEnv(), s: scenario(func(s *Scenario) { s.ID, s.DependsOn = "T2", []string{"T1"} }),
			prior: map[string]*Result{"T1": {ID: "T1", Status: StatusFail}}, reason: "depends on T1, which ended fail"},
		{name: "dependency passed", env: awsEnv(), s: scenario(func(s *Scenario) { s.ID, s.DependsOn = "T2", []string{"T1"} }),
			prior: map[string]*Result{"T1": {ID: "T1", Status: StatusPass}}},
		{name: "non-essential over budget", env: awsEnv(), gov: overBudget, s: scenario(func(s *Scenario) { s.EstimateUSD = 20 }), reason: "non-essential scenario skipped"},
		{name: "essential over budget is held", env: awsEnv(), gov: overBudget, s: scenario(func(s *Scenario) { s.EstimateUSD = 20; s.Essential = true }), reason: "ask the user"},
		{name: "free scenario always admitted", env: awsEnv(), gov: overBudget, s: scenario(func(s *Scenario) { s.EstimateUSD = 0 })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &Runner{Registry: NewRegistry(), Env: tc.env, Governor: tc.gov}
			got := r.Preflight(tc.s, tc.prior)
			if tc.reason == "" {
				require.Empty(t, got)
			} else {
				require.Contains(t, got, tc.reason)
			}
		})
	}
}

// NewTestGovernor returns an in-memory governor that has spent `spent`.
func NewTestGovernor(spent float64) *Governor {
	g := &Governor{L: Ledger{BudgetUSD: DefaultBudgetUSD}}
	g.L.Entries = []LedgerEntry{{ID: "earlier", MeasuredUSD: spent}}
	return g
}

// Guards §12: the projection uses the larger of AWS-measured actual spend and
// the scenario ledger, holds back a reserve, and an approved overrun admits
// essential scenarios only.
func TestGovernorProjection(t *testing.T) {
	g := &Governor{L: Ledger{BudgetUSD: 300, ReserveUSD: 30}}
	ess := scenario(func(s *Scenario) { s.EstimateUSD = 50; s.Essential = true })
	require.True(t, g.Decide(ess, Safety{}).Admit)

	require.NoError(t, g.Record("T1", time.Unix(0, 0), 50, 40))
	require.NoError(t, g.Refresh(240, time.Unix(1, 0)))
	require.InDelta(t, 240.0, g.SpentUSD(), 1e-9, "actual spend wins when larger")
	d := g.Decide(ess, Safety{})
	require.False(t, d.Admit)
	require.InDelta(t, 320, d.ProjectedUSD, 1e-9)
	require.True(t, g.Decide(ess, Safety{AllowOverBudget: true}).Admit)
	require.False(t, g.Decide(scenario(func(s *Scenario) { s.EstimateUSD = 50 }), Safety{AllowOverBudget: true}).Admit)
	require.False(t, g.Decide(scenario(func(s *Scenario) { s.EstimateUSD = 5 }), Safety{MaxSpendUSD: 100}).Admit, "a lower descriptor limit wins")
}

// Guards R-TEST-8d's result contract: run → status from the error kind and
// post-conditions, NFR misses fail the scenario, skips carry the reason.
func TestRunnerStatus(t *testing.T) {
	env := awsEnv()
	pass := CheckFunc{Name: "ok", Kind: "custom", Fn: func(context.Context, *Context) CheckResult { return CheckResult{Pass: true} }}
	bad := CheckFunc{Name: "zero instances", Kind: "aws", Fn: func(context.Context, *Context) CheckResult { return CheckResult{Detail: "1 left"} }}
	unavailable := CheckFunc{Name: "prom", Kind: "promql", Fn: func(context.Context, *Context) CheckResult { return CheckResult{Skipped: "no prometheus"} }}

	reg := NewRegistry()
	reg.Register(
		scenario(func(s *Scenario) { s.ID = "T1"; s.Post = []Check{pass, unavailable} }),
		scenario(func(s *Scenario) { s.ID = "T2"; s.Post = []Check{bad} }),
		scenario(func(s *Scenario) {
			s.ID = "T3"
			s.Run = func(c *Context) error {
				c.NFR(NFRResult{ID: "NFR-P1", Subject: "linux", Measured: 95, Unit: "s", Target: "max ≤ 90 s", Pass: false})
				return nil
			}
		}),
		scenario(func(s *Scenario) { s.ID = "T4"; s.Run = func(*Context) error { return Skip("no windows AMI yet") } }),
		scenario(func(s *Scenario) { s.ID = "T5"; s.Run = func(*Context) error { return Fail("build failed") } }),
		scenario(func(s *Scenario) { s.ID = "T6"; s.Run = func(*Context) error { panic("boom") } }),
		scenario(func(s *Scenario) { s.ID = "T7"; s.Requires = []Requirement{RequiresIdP} }),
	)
	clock := time.Unix(1_790_000_000, 0)
	r := &Runner{Registry: reg, Env: env, Now: func() time.Time { clock = clock.Add(time.Second); return clock }}
	results, err := r.RunAll(context.Background(), nil)
	require.NoError(t, err)

	got := map[string]Status{}
	for _, res := range results {
		got[res.ID] = res.Status
		require.Equal(t, "e2e-test", res.RunID)
		require.Equal(t, map[string]string{"cucina:env": "e2e", "cucina:run": "e2e-test"}, res.Tags)
	}
	require.Equal(t, map[string]Status{
		"T1": StatusPass, "T2": StatusFail, "T3": StatusFail, "T4": StatusSkip,
		"T5": StatusFail, "T6": StatusError, "T7": StatusSkip,
	}, got)
	require.Equal(t, "no windows AMI yet", results[3].SkipReason)
	require.Contains(t, results[2].Error, "NFR-P1 (linux) missed")
	require.Contains(t, results[6].SkipReason, "requires idp")
	require.Len(t, results[0].Checks, 2)
}

// Guards the descriptor contract: unknown fields fail fast, AWS environments
// must carry the §12 tags and safety limits, prod-smoke is never destructive.
func TestParseEnv(t *testing.T) {
	ok := `{"name":"aws-e2e","kind":"aws-e2e","runId":"r","capabilities":["aws"],
	  "aws":{"profile":"default","region":"us-west-1","tags":{"cucina:env":"e2e","cucina:run":"r"}},
	  "abseil":{"tag":"20260817.0","commit":"c"},"safety":{"maxSpendUSD":300,"maxInstances":12},"artifactsDir":"/tmp/x"}`
	e, err := ParseEnv([]byte(ok))
	require.NoError(t, err)
	require.True(t, e.Has(RequiresAWS))
	require.False(t, e.Has(RequiresDestructive))

	for name, doc := range map[string]string{
		"unknown field":      `{"name":"x","kind":"kind","bogus":1}`,
		"unknown kind":       `{"name":"x","kind":"staging"}`,
		"aws without tags":   `{"name":"x","kind":"aws-e2e","capabilities":["aws"],"aws":{"region":"us-west-1"},"safety":{"maxSpendUSD":1,"maxInstances":1}}`,
		"aws without limits": `{"name":"x","kind":"aws-e2e","capabilities":["aws"],"aws":{"region":"us-west-1","tags":{"cucina:env":"e2e","cucina:run":"r"}}}`,
		"destructive smoke":  `{"name":"x","kind":"prod-smoke","safety":{"allowDestructive":true}}`,
		"bad client os":      `{"name":"x","kind":"kind","clients":{"c":{"os":"plan9","instanceId":"i"}}}`,
	} {
		_, err := ParseEnv([]byte(doc))
		require.Error(t, err, name)
	}
}
