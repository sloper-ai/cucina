// SPDX-License-Identifier: FSL-1.1-ALv2

package scenarios

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sloper-ai/cucina/slo"
	"github.com/sloper-ai/cucina/test/e2e/bazelrun"
	"github.com/sloper-ai/cucina/test/e2e/collect/awsinv"
	"github.com/sloper-ai/cucina/test/e2e/collect/spend"
	"github.com/sloper-ai/cucina/test/e2e/harness"
	"github.com/sloper-ai/cucina/test/e2e/infra"
	"github.com/sloper-ai/cucina/test/e2e/nfr"
	"github.com/sloper-ai/cucina/test/e2e/remote"
)

// workerSelector returns the PromQL matchers for a lane's bb_worker metrics.
func workerSelector(c *harness.Context, lane string) string {
	if s := c.Env.WorkerSelectors[lane]; s != "" {
		return s
	}
	return workerSel
}

// ---------------------------------------------------------------- T0

// t0 installs the chart. In kind (fakes) the same scenario runs as kind-t0
// with the 2-minute budget of non-AWS scenarios.
func t0() *harness.Scenario {
	return &harness.Scenario{
		ID: "T0", Title: "Fresh helm install of the packaged chart",
		Requires: []harness.Requirement{harness.RequiresKubernetes, harness.RequiresCucinactl},
		Cost:     harness.CostNone, Essential: true, Timeout: 20 * time.Minute, NFRs: []string{"NFR-C1", "NFR-M1"},
		Post: append([]harness.Check{ZeroResidueCheck("no worker instances or pool volumes")}, Guards...),
		Run: func(c *harness.Context) error {
			svc, err := infra.Of(c)
			if err != nil {
				return err
			}
			k := svc.Kube
			if _, err := k.Helm(c, "status", k.K.Release); err == nil {
				return harness.Skip("release %s already exists: T0 installs into a fresh cluster (helm uninstall first)", k.K.Release)
			}
			if err := c.Step("helm install --wait", func() error {
				_, err := k.Install(c, c.Scenario.Timeout*3/4)
				return err
			}); err != nil {
				return harness.Fail("%v", err)
			}
			if err := c.Step("all pods Ready", func() error {
				pods, err := k.Pods(c, "")
				if err != nil {
					return err
				}
				if nr := infra.NotReady(pods); len(nr) > 0 {
					return fmt.Errorf("not ready: %s", strings.Join(nr, ", "))
				}
				return nil
			}); err != nil {
				return harness.Fail("%v", err)
			}
			if err := c.Step("helm test", func() error {
				_, err := k.Test(c, c.Scenario.Timeout/4)
				return err
			}); err != nil {
				return harness.Fail("%v", err)
			}
			if err := svc.BootstrapLocalProfile(c); err != nil {
				return err
			}
			if err := c.Step("cucinactl: every pool at zero", func() error { return poolsAtZero(c, svc) }); err != nil {
				return harness.Fail("%v", err)
			}
			if r, err := residue(c); err == nil {
				c.NFR(nfr.Residue("after install", r.Instances, r.Volumes, r.ENIs, r.EIPs, r.PublicIPs))
			}
			if v, ok := promScalar(c, fmt.Sprintf(`%s{namespace=%q}`, slo.ControlPlaneRSS, k.K.Namespace), time.Time{}); ok {
				addNFRs(c, nfr.ControlPlaneMemory(v, 0, 0)[:1])
			}
			return nil
		},
	}
}

// poolsAtZero checks `cucinactl pools list --output json`: nothing desired,
// launching, registered or busy anywhere.
func poolsAtZero(c *harness.Context, svc *infra.Services) error {
	var raw any
	if err := svc.CucinactlJSON(c, &raw, "pools", "list"); err != nil {
		return err
	}
	pools, _ := raw.([]any)
	if m, ok := raw.(map[string]any); ok {
		pools, _ = field(m, "pools").([]any)
	}
	if len(pools) == 0 {
		return errors.New("cucinactl pools list returned no pools")
	}
	var busy []string
	for _, p := range pools {
		m, _ := p.(map[string]any)
		for _, k := range []string{"desired", "launching", "registered", "busy", "idle", "draining"} {
			if n, _ := field(m, k).(float64); n > 0 {
				busy = append(busy, fmt.Sprintf("%s %s=%g", str(field(m, "name")), k, n))
			}
		}
	}
	if len(busy) > 0 {
		return fmt.Errorf("pools not at zero: %s", strings.Join(busy, ", "))
	}
	return nil
}

// ---------------------------------------------------------------- T7

func t7() *harness.Scenario {
	return &harness.Scenario{
		ID: "T7", Title: "Concurrency: Linux + Windows builds simultaneously, plus a second Linux configuration (-c opt)",
		Requires: []harness.Requirement{harness.RequiresAWS, harness.RequiresLinuxClient, harness.RequiresWindowsClient, harness.RequiresPrometheus},
		Cost:     harness.CostHigh, EstimateUSD: 15, MaxInstances: 10, Essential: true, Timeout: 3 * time.Hour,
		DependsOn: []string{"T1", "T4"}, NFRs: []string{"NFR-X4", "NFR-M1"},
		Post: append([]harness.Check{}, Guards...),
		Run: func(c *harness.Context) error {
			linux, err := openLane(c, LinuxLane)
			if err != nil {
				return err
			}
			windows, err := openLane(c, WindowsLane)
			if err != nil {
				return err
			}
			svc, err := infra.Of(c)
			if err != nil {
				return err
			}
			capture, err := startScaleInCapture(c, svc)
			if err != nil {
				return err
			}
			defer func() {
				capture.close()
				if err := saveScaleInEvidence(c, capture.snapshot()); err != nil {
					c.Check(harness.CheckResult{Name: "persist live scale-in evidence", Kind: "collector", Detail: err.Error()})
				}
			}()
			var workEnd time.Time
			type job struct {
				name string
				lr   *laneRun
				opts BuildOpts
			}
			jobs := []job{
				{"linux-fastbuild", linux, BuildOpts{Command: "build", Extra: []string{"--noremote_accept_cached"}}},
				{"linux-opt", linux, BuildOpts{Command: "build", Extra: []string{"-c", "opt", "--noremote_accept_cached"},
					OutputBase: hjoin(linux.host, linux.host.WorkDir(), "ob-opt")}},
				{"windows", windows, BuildOpts{Command: "build", Extra: []string{"--noremote_accept_cached"}}},
			}
			start := c.Now()
			outs := make([]*bazelrun.Outcome, len(jobs))
			errs := make([]error, len(jobs))
			usage, err := sampled(c, 10*time.Second, func() error {
				var wg sync.WaitGroup
				for i, j := range jobs {
					wg.Add(1)
					go func() {
						defer wg.Done()
						outs[i], errs[i] = j.lr.bazel(j.name, j.opts)
					}()
				}
				wg.Wait()
				workEnd = c.Now()
				if err := errors.Join(errs...); err != nil {
					return err
				}
				capture.finish()
				return nil
			})
			end := workEnd
			if err != nil {
				return err
			}
			var failed []string
			for i, o := range outs {
				if err := mustSucceed(o, jobs[i].name); err != nil {
					failed = append(failed, err.Error())
					continue
				}
				c.NFR(nfr.Reported("NFR-X4", "T7 "+jobs[i].name, o.Wall.Seconds(), "s", fmt.Sprintf("time to first remote action %s", o.TimeToFirstRemoteAction.Round(time.Second))))
				// No starvation: every build got its first remote action within 10 min.
				if o.TimeToFirstRemoteAction > 10*time.Minute {
					failed = append(failed, fmt.Sprintf("%s starved: first remote action after %s", jobs[i].name, o.TimeToFirstRemoteAction))
				}
			}
			c.NFR(nfr.Reported("NFR-X4", "T7 all three concurrently", end.Sub(start).Seconds(), "s", ""))
			lp, wp := LinuxLane.Pool(c.Env), WindowsLane.Pool(c.Env)
			indep := usage.MaxByPool[lp] > 0 && usage.MaxByPool[wp] > 0
			c.Check(harness.CheckResult{Name: "pools scaled independently", Kind: "aws", Pass: indep,
				Value: fmt.Sprintf("max %s=%d, %s=%d", lp, usage.MaxByPool[lp], wp, usage.MaxByPool[wp])})
			if v, ok := promMax(c, fmt.Sprintf(`%s{namespace=%q}`, slo.ControlPlaneRSS, namespace(c.Env)), start, end); ok {
				addNFRs(c, nfr.ControlPlaneMemory(0, v, 0)[:1])
			}
			if len(failed) > 0 || !indep {
				return harness.Fail("%s", strings.Join(append(failed, fmt.Sprintf("independent scaling: %v", indep)), "; "))
			}
			return nil
		},
	}
}

// ---------------------------------------------------------------- T8

func t8() *harness.Scenario {
	return &harness.Scenario{
		ID: "T8", Title: "Scale-in: every worker drained and provider-confirmed terminated within idle timeout + drain grace",
		Requires: []harness.Requirement{harness.RequiresAWS, harness.RequiresCucinactl, harness.RequiresKubernetes},
		Cost:     harness.CostNone, Essential: true, Timeout: 45 * time.Minute, NFRs: []string{"NFR-C1"},
		// A failed T7 can still leave valuable complete lifecycle evidence;
		// read that evidence explicitly instead of hiding it behind a PASS dependency.
		Post: append([]harness.Check{ZeroResidueCheck("zero worker instances, volumes, ENIs and IPs")}, Guards...),
		Run:  runScaleInEvidence,
	}
}

// PoolEvent is one entry of a pool's timeline.
type PoolEvent struct {
	Time    time.Time
	Type    string
	Subject string
	Message string
}

func specOf(pi PoolInfo) map[string]any {
	var spec map[string]any
	if s := str(field(pi.Raw, "spec_json")); s != "" {
		_ = json.Unmarshal([]byte(s), &spec)
	}
	if spec == nil {
		spec, _ = field(pi.Raw, "spec").(map[string]any)
	}
	return spec
}

func poolEvents(pi PoolInfo) []PoolEvent {
	var out []PoolEvent
	evs, _ := field(pi.Raw, "events").([]any)
	for _, e := range evs {
		m, _ := e.(map[string]any)
		t, err := time.Parse(time.RFC3339Nano, str(field(m, "time")))
		if err != nil {
			continue
		}
		out = append(out, PoolEvent{Time: t, Type: str(field(m, "type")), Subject: str(field(m, "subject")), Message: str(field(m, "message"))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out
}

// ---------------------------------------------------------------- T11

func t11() *harness.Scenario {
	return &harness.Scenario{
		ID: "T11", Title: "helm upgrade with a config change, then helm rollback",
		Requires: []harness.Requirement{harness.RequiresKubernetes, harness.RequiresAWS, harness.RequiresLinuxClient},
		Envs:     []harness.EnvKind{harness.EnvAWS},
		Cost:     harness.CostLow, EstimateUSD: 1, Essential: true, Timeout: time.Hour, DependsOn: []string{"T1"}, NFRs: []string{"NFR-R2"},
		Post: append([]harness.Check{}, Guards...),
		Run: func(c *harness.Context) error {
			svc, err := infra.Of(c)
			if err != nil {
				return err
			}
			k := svc.Kube
			if k.K.UpgradeValuesFile == "" {
				return harness.Skip("no kubernetes.upgradeValuesFile with T11's configuration change")
			}
			rev, err := k.Revision(c)
			if err != nil {
				return err
			}
			lr, err := openLane(c, LinuxLane)
			if err != nil {
				return err
			}
			warm := func(name string) error {
				if _, err := lr.bazel(name+"-expunge", BuildOpts{Command: "clean", Extra: []string{"--expunge"}}); err != nil {
					return err
				}
				o, err := lr.bazel(name, BuildOpts{Command: "build", FreshServer: true})
				if err != nil {
					return err
				}
				if err := mustSucceed(o, name); err != nil {
					return err
				}
				if o.ExecLog == nil || o.ExecLog.Spawns == 0 {
					return harness.Fail("%s: missing cache-hit evidence", name)
				}
				c.NFR(nfr.CacheHits("NFR-R2", "after "+name, o.ExecLog.RemoteCacheHitRatio()))
				return nil
			}
			if err := c.Step("helm upgrade (config change)", func() error {
				_, err := k.Upgrade(c, 10*time.Minute, "-f", k.K.UpgradeValuesFile)
				return err
			}); err != nil {
				return harness.Fail("%v", err)
			}
			if err := k.WaitReady(c, 10*time.Minute); err != nil {
				return harness.Fail("not ready after upgrade: %v", err)
			}
			if err := c.Step("controller converged", func() error { return poolsAtZero(c, svc) }); err != nil {
				return harness.Fail("controller did not converge after upgrade: %v", err)
			}
			if err := warm("warm-after-upgrade"); err != nil {
				return err
			}
			if err := c.Step(fmt.Sprintf("helm rollback to revision %d", rev), func() error {
				_, err := k.Rollback(c, rev, 10*time.Minute)
				return err
			}); err != nil {
				return harness.Fail("%v", err)
			}
			if err := k.WaitReady(c, 10*time.Minute); err != nil {
				return harness.Fail("not ready after rollback: %v", err)
			}
			return warm("warm-after-rollback")
		},
	}
}

// ---------------------------------------------------------------- T12

func t12() *harness.Scenario {
	return &harness.Scenario{
		ID: "T12", Title: "Image rollout: publish a new Linux AMI during a build, then roll back",
		Requires: []harness.Requirement{harness.RequiresAWS, harness.RequiresKubernetes, harness.RequiresLinuxClient},
		Cost:     harness.CostHigh, EstimateUSD: 12, MaxInstances: 6, Essential: true, Timeout: 3 * time.Hour, DependsOn: []string{"T1"},
		Post: append([]harness.Check{}, Guards...),
		Run: func(c *harness.Context) error {
			svc, err := infra.Of(c)
			if err != nil {
				return err
			}
			pool := LinuxLane.Pool(c.Env)
			img, ok := c.Env.Images[pool]
			if !ok || img.Current == "" || img.Next == "" || img.Current == img.Next {
				return harness.Skip("no images[%q] {current, next} in the environment descriptor", pool)
			}
			lr, err := openLane(c, LinuxLane)
			if err != nil {
				return err
			}
			setImage := func(id string) error {
				patch := fmt.Sprintf(`{"spec":{"image":{"id":%q}}}`, id)
				_, err := svc.Kube.Kubectl(c, "patch", "workerpool", pool, "--type", "merge", "-p", patch)
				return err
			}
			var patched time.Time
			snaps := []awsinv.Snapshot{}
			usage, err := sampled(c, 10*time.Second, func() error {
				// Build A runs while the image changes.
				done := make(chan error, 1)
				go func() {
					o, err := lr.bazel("build-a", BuildOpts{Command: "build", Extra: []string{"--noremote_accept_cached"}})
					if err == nil {
						err = mustSucceed(o, "build A (image changed mid-build)")
					}
					done <- err
				}()
				// Change the image once the pool has scaled out.
				if err := waitInstances(c, svc, pool, 1, 20*time.Minute); err != nil {
					return err
				}
				if err := c.Step("publish the new image", func() error { return setImage(img.Next) }); err != nil {
					return err
				}
				patched = c.Now()
				if err := <-done; err != nil {
					return err
				}
				// Prove the old generation drains, then force a new launch rather
				// than accepting an empty set of post-rollout launches.
				_, res, err := waitZero(c, scaleInLimit)
				if err != nil {
					return err
				}
				if !res.Zero() {
					return harness.Fail("old generation did not drain: %s", res)
				}
				// Build B: launches after the change must use the new image.
				o, err := lr.bazel("build-b", BuildOpts{Command: "build", Extra: []string{"--noremote_accept_cached"}})
				if err == nil {
					err = mustSucceed(o, "build B (new image)")
				}
				if err != nil {
					return err
				}
				s, err := poolInventory(svc, pool).Describe(c)
				if err != nil {
					return err
				}
				if len(s.Instances) == 0 {
					return harness.Fail("no new-image instance observed after build B")
				}
				snaps = append(snaps, s)
				if err := c.Step("roll back the image", func() error { return setImage(img.Current) }); err != nil {
					return err
				}
				return nil
			})
			if err != nil {
				return err
			}
			var wrong []string
			observed := 0
			for _, l := range usage.Lifecycles {
				if l.Pool != pool || l.Launched.Before(patched) {
					continue
				}
				for _, s := range snaps {
					for _, in := range s.Instances {
						if in.ID == l.ID {
							observed++
							if in.ImageID != img.Next {
								wrong = append(wrong, in.ID+"="+in.ImageID)
							}
						}
					}
				}
			}
			if observed == 0 {
				return harness.Fail("no post-rollout launch has both lifecycle and image evidence")
			}
			if len(wrong) > 0 {
				return harness.Fail("launches after the rollout used the old image: %s", strings.Join(wrong, ", "))
			}
			// Old-generation workers finished and are gone once the pool idles.
			if _, r, err := waitZero(c, scaleInLimit); err != nil || !r.Zero() {
				return harness.Fail("old-generation workers not terminated: %v %s", err, r)
			}
			// Rollback: the next launch uses the previous image again.
			o, err := lr.bazel("build-c", BuildOpts{Command: "build", Extra: []string{"--noremote_accept_cached", "--keep_going"}})
			if err != nil {
				return err
			}
			if err := mustSucceed(o, "build C (rolled back)"); err != nil {
				return err
			}
			s, err := poolInventory(svc, pool).Describe(c)
			if err != nil {
				return err
			}
			if len(s.Instances) == 0 {
				return harness.Fail("no rollback-image instance observed")
			}
			for _, in := range s.Instances {
				if in.ImageID != img.Current {
					return harness.Fail("after rollback %s runs %s, want %s", in.ID, in.ImageID, img.Current)
				}
			}
			return nil
		},
	}
}

func waitInstances(c *harness.Context, svc *infra.Services, pool string, n int, limit time.Duration) error {
	deadline := c.Now().Add(limit)
	for {
		s, err := poolInventory(svc, pool).Describe(c)
		if err != nil {
			return err
		}
		if len(s.Instances) >= n {
			return nil
		}
		if c.Now().After(deadline) {
			return fmt.Errorf("pool %s: fewer than %d instances after %s", pool, n, limit)
		}
		if err := remote.RealSleep(c, 10*time.Second); err != nil {
			return err
		}
	}
}

// ---------------------------------------------------------------- T15

func t15() *harness.Scenario {
	return &harness.Scenario{
		ID: "T15", Title: "Teardown: helm uninstall → tofu destroy → tag sweep",
		Requires: []harness.Requirement{harness.RequiresAWS, harness.RequiresKubernetes, harness.RequiresDestructive},
		Cost:     harness.CostNone, Essential: true, Timeout: 2 * time.Hour,
		NFRs: []string{"NFR-C1", "NFR-C2", "NFR-C3", "NFR-T5", "NFR-T9"},
		Run: func(c *harness.Context) error {
			svc, err := infra.Of(c)
			if err != nil {
				return err
			}
			// Campaign-wide accounting first, while the environment exists.
			campaignAccounting(c, svc)
			if err := c.Step("helm uninstall (finalizers terminate pool instances)", func() error {
				_, err := svc.Kube.Uninstall(c, 20*time.Minute)
				return err
			}); err != nil {
				return harness.Fail("%v", err)
			}
			took, r, err := waitZero(c, 20*time.Minute)
			if err != nil {
				return err
			}
			c.Metric("uninstall_to_zero_seconds", took.Seconds(), "s")
			c.NFR(nfr.Residue("after helm uninstall", r.Instances, r.Volumes, r.ENIs, r.EIPs, r.PublicIPs))
			down := filepath.Join(c.Env.RepoDir, "deploy", "aws-e2e", "scripts", "down.sh")
			if err := c.Step("tofu destroy (deploy/aws-e2e/scripts/down.sh)", func() error {
				// User decision (§13): the AMIs go too; down.sh disables Fast Launch first.
				cmd := exec.CommandContext(c, down, "--delete-amis")
				cmd.Dir = c.Env.RepoDir
				out, err := cmd.CombinedOutput()
				c.Record("down.sh", tail(string(out), 8000))
				return err
			}); err != nil {
				return harness.Fail("%v", err)
			}
			var sw infra.SweepResult
			if err := c.Step("tag sweep", func() error {
				var err error
				sw, err = svc.Sweep(c)
				return err
			}); err != nil {
				return err
			}
			c.Record("sweep", sw)
			if !sw.Clean {
				return harness.Fail("tag sweep found leftovers (see values.sweep)")
			}
			return nil
		},
	}
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// campaignAccounting records NFR-C2 (standing cost), NFR-C3 (spend so far),
// NFR-T5 (one AZ ⇒ no cross-AZ bytes) and NFR-T9 (no NAT gateway, no public
// IPv4 on workers) from the run's results and tag-filtered describes.
func campaignAccounting(c *harness.Context, svc *infra.Services) {
	var spent float64
	azs := map[string]bool{}
	publicWorkers := 0
	for _, p := range c.Prior {
		spent += p.Cost.MeasuredUSD
		if u, ok := p.Values["aws"].(map[string]any); ok {
			lcs, _ := u["lifecycles"].([]any)
			for _, l := range lcs {
				m, _ := l.(map[string]any)
				if b, _ := m["publicIp"].(bool); b {
					publicWorkers++
				}
			}
		}
	}
	if svc.EC2 != nil {
		if s, err := svc.EC2.Describe(c); err == nil {
			for _, i := range s.Instances {
				azs[i.AZ] = true
			}
		}
	}
	test, testMonth := spend.Standing(spend.TestTopology())
	prod, prodMonth := spend.Standing(spend.SmallProductionTopology())
	c.Record("standingCost", map[string]any{"test": test, "testUSDPerMonth": testMonth, "production": prod, "productionUSDPerMonth": prodMonth})
	testRow := nfr.Reported("NFR-C2", "test topology", testMonth, "USD/month", "itemised in values.standingCost")
	if c.Env.MeasurementScope == harness.ScopeSmallFunctional {
		testRow.Pass = false
		testRow.Unqualified = "standing table is the original topology; actual small-functional standing inventory/rates must be supplied"
	}
	c.NFR(testRow)
	c.NFR(nfr.Reported("NFR-C2", "small production topology", prodMonth, "USD/month", "itemised in values.standingCost"))
	budgetRow := nfr.Spend(spent, harness.DefaultBudgetUSD)
	budgetRow.Pass = false
	budgetRow.Unqualified = "worker scenario subtotal only; standing environment, baseline hosts, image builds, snapshots, Fast Launch and transfer charges require complete campaign accounting"
	c.NFR(budgetRow)
	c.Note("NFR-C3: $%.2f is the sum of the scenarios' measured worker spend; the report adds the standing environment, image builds and Fast Launch", spent)
	if svc.EC2 != nil {
		if n, err := svc.EC2.NATGateways(c); err == nil {
			c.NFR(nfr.Count("NFR-T9", "NAT gateways", n, "no NAT gateway ⇒ zero NAT bytes"))
		}
	}
	c.NFR(nfr.Count("NFR-T9", "workers with public IPv4", publicWorkers, "no public IPv4 on workers ⇒ zero same-AZ public-IP bytes"))
	c.NFR(nfr.Count("NFR-T5", "extra AZs", max(0, len(azs)-1), "all tagged instances in one AZ ⇒ no cross-AZ bytes"))
}
