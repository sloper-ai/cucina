// SPDX-License-Identifier: FSL-1.1-ALv2

package scenarios

import (
	"strings"
	"time"

	"github.com/sloper-ai/cucina/test/e2e/harness"
	"github.com/sloper-ai/cucina/test/e2e/infra"
)

// Register adds every scenario in campaign order (§10.3; T15 last).
func Register(r *harness.Registry) {
	r.Register(t0(),
		baselineScenario("baseline-linux", LinuxLane),
		baselineScenario("baseline-windows", WindowsLane),
		baselineScenario("baseline-macos", MacLane),
		coldScenario("T1", LinuxLane), warmScenario("T2", "T1", LinuxLane), l1Scenario("T3", "T1", LinuxLane),
		coldScenario("T4", WindowsLane), warmScenario("T5", "T4", WindowsLane), l1Scenario("T6", "T4", WindowsLane),
		t7(), t8())
	r.Register(t9()...)
	r.Register(t10()...)
	r.Register(t11(), t12(), t13(), t14(), t16(), t17(), t18(), t19(), t20(), t21(), t22(),
		canaryCache(), canaryExec(), t15())
	r.Register(kindInstall(), cliSmoke(), podsReady(), zeroScale())
}

// kindInstall is T0 in a kind cluster with fakes (≤ 2 min, R-TEST-5.7).
func kindInstall() *harness.Scenario {
	s := t0()
	s.ID, s.Title = "kind-t0", "kind: fresh helm install of the packaged chart (fakes)"
	s.Envs, s.Timeout, s.Essential, s.NFRs = []harness.EnvKind{harness.EnvKindCluster}, 2*time.Minute, false, nil
	return s
}

// cliSmoke runs the read-only cucinactl commands (safe in production).
func cliSmoke() *harness.Scenario {
	return &harness.Scenario{
		ID: "cli-smoke", Title: "Read-only cucinactl commands with --output json",
		Requires: []harness.Requirement{harness.RequiresCucinactl},
		Envs:     []harness.EnvKind{harness.EnvAWS, harness.EnvKindCluster, harness.EnvProdSmoke}, ReadOnly: true,
		Cost: harness.CostNone, Timeout: 2 * time.Minute,
		Run: func(c *harness.Context) error {
			svc, err := infra.Of(c)
			if err != nil {
				return err
			}
			var bad []string
			for _, args := range readOnlyCommands {
				var v any
				err := svc.CucinactlJSON(c, &v, args...)
				c.Check(harness.CheckResult{Name: "cucinactl " + strings.Join(args, " "), Kind: "cli", Pass: err == nil, Detail: errString(err)})
				if err != nil {
					bad = append(bad, strings.Join(args, " "))
				}
			}
			if len(bad) > 0 {
				return harness.Fail("failed: %s", strings.Join(bad, ", "))
			}
			return nil
		},
	}
}

// podsReady checks that every Cucina pod is Ready (read-only).
func podsReady() *harness.Scenario {
	return &harness.Scenario{
		ID: "pods-ready", Title: "Every Cucina pod is Ready",
		Requires: []harness.Requirement{harness.RequiresKubernetes},
		Envs:     []harness.EnvKind{harness.EnvAWS, harness.EnvKindCluster, harness.EnvProdSmoke}, ReadOnly: true,
		Cost: harness.CostNone, Timeout: time.Minute, Post: Guards,
		Run: func(c *harness.Context) error {
			svc, err := infra.Of(c)
			if err != nil {
				return err
			}
			pods, err := svc.Kube.Pods(c, "")
			if err != nil {
				return err
			}
			if nr := infra.NotReady(pods); len(nr) > 0 {
				return harness.Fail("not ready: %s", strings.Join(nr, ", "))
			}
			restarts := 0
			for _, p := range pods {
				restarts += p.Restarts
			}
			c.Metric("pod_restarts", float64(restarts), "")
			return nil
		},
	}
}

// zeroScale is NFR-C1's read-only check: no worker resources exist (run it
// when every pool should be at zero).
func zeroScale() *harness.Scenario {
	return &harness.Scenario{
		ID: "zero-scale", Title: "At zero scale no worker instances, volumes, ENIs, EIPs or public IPs exist",
		Requires: []harness.Requirement{harness.RequiresAWS},
		Envs:     []harness.EnvKind{harness.EnvAWS, harness.EnvProdSmoke}, ReadOnly: true,
		Cost: harness.CostNone, Timeout: 2 * time.Minute, NFRs: []string{"NFR-C1"},
		Post: []harness.Check{ZeroResidueCheck("zero residue")},
		Run:  func(*harness.Context) error { return nil },
	}
}
