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
	for _, known := range []float64{5, 50} {
		for _, samplerGap := range []bool{false, true} {
			e := awsEnv()
			g := &Governor{L: Ledger{BudgetUSD: 300}}
			reg := NewRegistry()
			reg.Register(scenario(func(s *Scenario) {
				s.EstimateUSD = 20
				s.Run = func(c *Context) error {
					c.Result.Cost.MeasuredUSD = known
					if samplerGap {
						c.Result.Cost.Incomplete = true
					} else {
						c.Result.Cost.Unpriced = []string{"unknown.large"}
					}
					return nil
				}
			}))
			_, err := (&Runner{Registry: reg, Env: e, Governor: g}).Run(context.Background(), "T1")
			require.NoError(t, err)
			require.InDelta(t, max(known, 20), g.SpentUSD(), 1e-9, "incomplete costs retain both known lower bound and reservation")
		}
	}
	incomplete := &Governor{L: Ledger{BudgetUSD: 300}}
	require.NoError(t, incomplete.Record("unpriced", time.Unix(0, 0), 17, 0))
	require.InDelta(t, 17.0, incomplete.SpentUSD(), 1e-9, "unmeasured spend must retain the conservative estimate, not become zero")
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
		// Missing mandatory evidence must not be silently converted to PASS.
		scenario(func(s *Scenario) { s.ID = "T8"; s.NFRs = []string{"NFR-X1"} }),
		scenario(func(s *Scenario) { s.ID = "T9"; s.Post = []Check{unavailable, bad} }),
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
		"T1": StatusSkip, "T2": StatusFail, "T3": StatusFail, "T4": StatusSkip,
		"T5": StatusFail, "T6": StatusError, "T7": StatusSkip, "T8": StatusFail, "T9": StatusFail,
	}, got)
	require.Equal(t, "no windows AMI yet", results[3].SkipReason)
	require.Contains(t, results[2].Error, "NFR-P1 (linux) missed")
	require.Contains(t, results[6].SkipReason, "requires idp")
	require.Len(t, results[0].Checks, 2)
	// ADR0004 waives only P2 in explicit small-functional scope. All other
	// functional evidence, and P2 in the original scope, remain mandatory.
	for _, tc := range []struct {
		name, scope                                                      string
		p2Present, p2Pass, otherMissing, otherFail, checkSkip, checkFail bool
		want                                                             Status
	}{
		{name: "small P2 passes arithmetic", scope: ScopeSmallFunctional, p2Present: true, p2Pass: true, want: Status("functional-pass")},
		{name: "small P2 fails arithmetic", scope: ScopeSmallFunctional, p2Present: true, want: Status("functional-pass")},
		{name: "small no worker benchmark", scope: ScopeSmallFunctional, want: Status("functional-pass")},
		{name: "full missing P2", want: StatusFail},
		{name: "full failed P2", p2Present: true, want: StatusFail},
		{name: "full passed P2", p2Present: true, p2Pass: true, want: StatusPass},
		{name: "unknown scope has no waiver", scope: "other", want: StatusFail},
		{name: "small missing other NFR", scope: ScopeSmallFunctional, otherMissing: true, want: StatusFail},
		{name: "small failed other NFR", scope: ScopeSmallFunctional, otherFail: true, want: StatusFail},
		{name: "small skipped mandatory check", scope: ScopeSmallFunctional, checkSkip: true, want: StatusSkip},
		{name: "small failed mandatory check", scope: ScopeSmallFunctional, checkFail: true, want: StatusFail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := awsEnv()
			e.MeasurementScope = tc.scope
			reg2 := NewRegistry()
			reg2.Register(scenario(func(s *Scenario) {
				s.NFRs = []string{"NFR-P2"}
				if tc.otherMissing || tc.otherFail {
					s.NFRs = append(s.NFRs, "NFR-P1")
				}
				if tc.checkSkip {
					s.Post = []Check{unavailable}
				}
				if tc.checkFail {
					s.Post = []Check{bad}
				}
				s.Run = func(c *Context) error {
					if tc.p2Present {
						c.NFR(NFRResult{ID: "NFR-P2", Pass: tc.p2Pass, Measured: 40, Unit: "%", Target: "<=50%"})
					}
					if tc.otherFail {
						c.NFR(NFRResult{ID: "NFR-P1", Pass: false})
					}
					return nil
				}
			}))
			res, err := (&Runner{Registry: reg2, Env: e}).Run(context.Background(), "T1")
			require.NoError(t, err)
			require.Equal(t, tc.want, res.Status)
			if tc.want == Status("functional-pass") {
				require.Equal(t, ScopeSmallFunctional, res.MeasurementScope)
				require.Len(t, res.NFRs, 1)
				require.Contains(t, res.NFRs[0].Unqualified, "ADR0004")
				require.False(t, res.NFRs[0].Pass)
				dep := scenario(func(s *Scenario) { s.ID = "T7"; s.DependsOn = []string{"T1"} })
				require.Empty(t, (&Runner{Env: e}).Preflight(dep, map[string]*Result{"T1": res}))
				require.NotEmpty(t, (&Runner{Env: awsEnv()}).Preflight(dep, map[string]*Result{"T1": res}), "functional pass cannot satisfy original-scope qualification")
			}
		})
	}
}

// Guards the descriptor contract: unknown fields fail fast, AWS environments
// must carry the §12 tags and safety limits, prod-smoke is never destructive.
func TestParseEnv(t *testing.T) {
	ok := `{"name":"aws-e2e","kind":"aws-e2e","runId":"r","capabilities":["aws"],
	  "aws":{"profile":"default","region":"us-west-1","tags":{"cucina:env":"e2e","cucina:run":"r","cucina:expires":"2026-10-09T00:00:00Z"}},
	  "abseil":{"tag":"20260817.0","commit":"c"},"safety":{"maxSpendUSD":300,"maxInstances":12},"artifactsDir":"/tmp/x"}`
	e, err := ParseEnv([]byte(ok))
	require.NoError(t, err)
	require.True(t, e.Has(RequiresAWS))
	require.False(t, e.Has(RequiresDestructive))

	for name, doc := range map[string]string{
		"unknown field":      `{"name":"x","kind":"kind","bogus":1}`,
		"unknown kind":       `{"name":"x","kind":"staging"}`,
		"aws without tags":   `{"name":"x","kind":"aws-e2e","capabilities":["aws"],"aws":{"region":"us-west-1"},"safety":{"maxSpendUSD":1,"maxInstances":1}}`,
		"aws without limits": `{"name":"x","kind":"aws-e2e","capabilities":["aws"],"aws":{"region":"us-west-1","tags":{"cucina:env":"e2e","cucina:run":"r","cucina:expires":"2026-10-09T00:00:00Z"}}}`,
		"destructive smoke":  `{"name":"x","kind":"prod-smoke","safety":{"allowDestructive":true}}`,
		"bad client os":      `{"name":"x","kind":"kind","clients":{"c":{"os":"plan9","instanceId":"i"}}}`,
	} {
		_, err := ParseEnv([]byte(doc))
		require.Error(t, err, name)
	}
}
