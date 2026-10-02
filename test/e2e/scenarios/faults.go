// SPDX-License-Identifier: FSL-1.1-ALv2

package scenarios

import (
	"fmt"
	"strings"
	"time"

	"github.com/sloper-ai/cucina/test/e2e/bazelrun"
	"github.com/sloper-ai/cucina/test/e2e/harness"
	"github.com/sloper-ai/cucina/test/e2e/infra"
	"github.com/sloper-ai/cucina/test/e2e/nfr"
	"github.com/sloper-ai/cucina/test/e2e/remote"
)

// fault is one T9 failure injection: inject runs mid-build (once a worker of
// the Linux pool is busy); after runs once the build has finished.
type fault struct {
	id, title string
	requires  []harness.Requirement
	// fromZero starts the build with the pool at zero and injects during the
	// scale-out instead of waiting for a busy worker (T9d).
	fromZero bool
	inject   func(c *harness.Context, svc *infra.Services, busyNode string) error
	after    func(c *harness.Context, svc *infra.Services, lr *laneRun, o *bazelrun.Outcome) error
	nfrs     []string
}

func faultScenario(f fault) *harness.Scenario {
	req := append([]harness.Requirement{harness.RequiresAWS, harness.RequiresLinuxClient, harness.RequiresCucinactl, harness.RequiresDestructive}, f.requires...)
	return &harness.Scenario{
		ID: f.id, Title: "Failure injection mid-build: " + f.title,
		Requires: req, Cost: harness.CostHigh, EstimateUSD: 8, MaxInstances: 6, Essential: true, Timeout: 3 * time.Hour,
		DependsOn: []string{"T1"}, NFRs: append([]string{"NFR-R1"}, f.nfrs...),
		Post: append([]harness.Check{ZeroResidueCheck("no leaked instances after scale-in")}, Guards...),
		Run: func(c *harness.Context) error {
			svc, err := infra.Of(c)
			if err != nil {
				return err
			}
			pool := LinuxLane.Pool(c.Env)
			if f.fromZero {
				if _, r, err := waitZero(c, scaleInLimit); err != nil || !r.Zero() {
					return harness.Fail("pool not at zero before %s: %v %s", f.id, err, r)
				}
			}
			lr, err := openLane(c, LinuxLane)
			if err != nil {
				return err
			}
			var out *bazelrun.Outcome
			usage, err := sampled(c, 10*time.Second, func() error {
				done := make(chan error, 1)
				go func() {
					o, err := lr.bazel("build-under-fault", BuildOpts{Command: "build", Extra: []string{"--noremote_accept_cached"}})
					out = o
					done <- err
				}()
				var node string
				var err error
				if f.fromZero {
					err = waitInstances(c, svc, pool, 1, 20*time.Minute)
				} else {
					node, err = busyWorker(c, svc, pool, 30*time.Minute)
				}
				if err != nil {
					return err
				}
				c.Event(node, "fault injected: "+f.title, c.Now())
				if err := c.Step("inject: "+f.title, func() error { return f.inject(c, svc, node) }); err != nil {
					return err
				}
				return <-done
			})
			if err != nil {
				return err
			}
			ok := out != nil && out.Succeeded()
			failed := 0
			if !ok {
				failed = 1
			}
			c.NFR(nfr.Count("NFR-R1", f.id+" build under fault", failed, "bazel exit code after the injected failure"))
			if len(usage.DuplicateTokens) > 0 {
				c.NFR(nfr.Count("NFR-R3", f.id+" duplicate launches", len(usage.DuplicateTokens), fmt.Sprint(usage.DuplicateTokens)))
			}
			if !ok {
				return harness.Fail("build failed under %s", f.title)
			}
			if f.after != nil {
				if err := f.after(c, svc, lr, out); err != nil {
					return err
				}
			}
			_, res, err := waitZero(c, scaleInLimit)
			if err != nil {
				return err
			}
			if !res.Zero() {
				return harness.Fail("residue after idle/drain grace: %s", res)
			}
			return nil
		},
	}
}

// busyWorker polls `cucinactl workers list --output json` for a worker of
// the pool with busy threads.
func busyWorker(c *harness.Context, svc *infra.Services, pool string, limit time.Duration) (string, error) {
	deadline := c.Now().Add(limit)
	for {
		var raw any
		if err := svc.CucinactlJSON(c, &raw, "workers", "list"); err == nil {
			ws, _ := raw.([]any)
			if m, ok := raw.(map[string]any); ok {
				ws, _ = field(m, "workers").([]any)
			}
			for _, w := range ws {
				m, _ := w.(map[string]any)
				busy, _ := field(m, "busy_threads").(float64)
				if str(field(m, "pool")) == pool && (busy > 0 || str(field(m, "state")) == "busy") {
					return str(field(m, "node")), nil
				}
			}
		}
		if c.Now().After(deadline) {
			return "", fmt.Errorf("no busy %s worker within %s", pool, limit)
		}
		if err := remote.RealSleep(c, 10*time.Second); err != nil {
			return "", err
		}
	}
}

func warmAfter(name string) func(*harness.Context, *infra.Services, *laneRun, *bazelrun.Outcome) error {
	return func(c *harness.Context, _ *infra.Services, lr *laneRun, _ *bazelrun.Outcome) error {
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
			return harness.Fail("%s: missing warm-cache execution evidence", name)
		}
		r := nfr.CacheHits("NFR-R2", name, o.ExecLog.RemoteCacheHitRatio())
		c.NFR(r)
		if !r.Pass {
			return harness.Fail("cache lost: %.2f%% hits", r.Measured)
		}
		return nil
	}
}

func controllerDeployment(c *harness.Context, svc *infra.Services) (string, error) {
	out, err := svc.Kube.Kubectl(c, "get", "deploy", "-l", "app.kubernetes.io/component="+infra.ComponentController, "-o", "jsonpath={.items[0].metadata.name}")
	if err != nil {
		return "", fmt.Errorf("controller deployment: %w", err)
	}
	if strings.TrimSpace(string(out)) == "" {
		return "", fmt.Errorf("controller deployment not found")
	}
	return strings.TrimSpace(string(out)), nil
}

func t9() []*harness.Scenario {
	return []*harness.Scenario{
		faultScenario(fault{id: "T9a", title: "terminate a busy worker",
			inject: func(c *harness.Context, svc *infra.Services, node string) error {
				return svc.EC2.TerminateWorker(c, node)
			}}),
		faultScenario(fault{id: "T9b", title: "delete the scheduler pod", nfrs: []string{"NFR-R2"},
			inject: func(c *harness.Context, svc *infra.Services, _ string) error {
				_, err := svc.Kube.DeletePods(c, infra.ComponentScheduler, 0)
				return err
			}}),
		faultScenario(fault{id: "T9c", title: "delete a storage pod", nfrs: []string{"NFR-R2"},
			inject: func(c *harness.Context, svc *infra.Services, _ string) error {
				pods, err := svc.Kube.Pods(c, "app.kubernetes.io/component="+infra.ComponentStorage)
				if err != nil {
					return fmt.Errorf("storage pods: %w", err)
				}
				if len(pods) == 0 {
					return fmt.Errorf("no storage pods")
				}
				_, err = svc.Kube.Kubectl(c, "delete", "pod", "--wait=false", pods[0].Name)
				return err
			},
			after: warmAfter("warm-after-storage-restart")}),
		faultScenario(fault{id: "T9d", title: "restart the controller during scale-out", fromZero: true, nfrs: []string{"NFR-R3"},
			inject: func(c *harness.Context, svc *infra.Services, _ string) error {
				d, err := controllerDeployment(c, svc)
				if err != nil {
					return err
				}
				return svc.Kube.RolloutRestart(c, "deployment", d, 5*time.Minute)
			},
			after: func(c *harness.Context, _ *infra.Services, _ *laneRun, _ *bazelrun.Outcome) error {
				_, r, err := waitZero(c, scaleInLimit)
				if err != nil {
					return err
				}
				c.NFR(nfr.Count("NFR-R3", "T9d leaked instances", r.Instances, r.String()))
				if !r.Zero() {
					return harness.Fail("instances leaked after the controller restart: %s", r)
				}
				return nil
			}}),
		faultScenario(fault{id: "T9e", title: "cut a worker's network to the control plane for 2 min", requires: nil,
			inject: func(c *harness.Context, svc *infra.Services, node string) error {
				iso := c.Env.AWS.IsolationSecurityGroup
				if iso == "" {
					return harness.Skip("no aws.isolationSecurityGroup in the environment descriptor")
				}
				prev, err := svc.EC2.SwapSecurityGroups(c, node, []string{iso})
				if err != nil {
					return err
				}
				c.Event(node, "network cut (security groups swapped)", c.Now())
				werr := remote.RealSleep(c, 2*time.Minute)
				if _, err := svc.EC2.SwapSecurityGroups(c, node, prev); err != nil && !strings.Contains(err.Error(), "not found") {
					return fmt.Errorf("restore security groups of %s: %w", node, err)
				}
				c.Event(node, "network restored", c.Now())
				return werr
			},
			after: func(c *harness.Context, svc *infra.Services, _ *laneRun, _ *bazelrun.Outcome) error {
				// Per policy the worker either reconnected or terminated itself;
				// it must not linger unregistered.
				_, r, err := waitZero(c, scaleInLimit)
				if err != nil {
					return err
				}
				if !r.Zero() {
					return harness.Fail("worker neither reconnected and scaled in nor self-terminated: %s", r)
				}
				return nil
			}}),
		{
			ID: "T9f", Title: "Orphaned worker (controller down, control plane unreachable) powers itself off and terminates",
			Requires: []harness.Requirement{harness.RequiresAWS, harness.RequiresLinuxClient, harness.RequiresCucinactl, harness.RequiresDestructive, harness.RequiresKubernetes},
			Cost:     harness.CostMedium, EstimateUSD: 3, MaxInstances: 4, Timeout: 2 * time.Hour, DependsOn: []string{"T1"}, NFRs: []string{"NFR-R4"},
			Post: append([]harness.Check{ZeroResidueCheck("orphan gone")}, Guards...),
			Run:  runOrphan,
		},
	}
}

// runOrphan scales the controller to zero, cuts one idle worker off the
// control plane, and expects the worker's dead-man switch to shut it down
// (InstanceInitiatedShutdownBehavior=terminate, R-POOL-7).
func runOrphan(c *harness.Context) error {
	svc, err := infra.Of(c)
	if err != nil {
		return err
	}
	iso := c.Env.AWS.IsolationSecurityGroup
	if iso == "" {
		return harness.Skip("no aws.isolationSecurityGroup in the environment descriptor")
	}
	pool := LinuxLane.Pool(c.Env)
	lr, err := openLane(c, LinuxLane)
	if err != nil {
		return err
	}
	// One short re-execution brings a worker up.
	if _, err := lr.bazel("bring-up", BuildOpts{Command: "build", Extra: []string{"--noremote_accept_cached"}}); err != nil {
		return err
	}
	s, err := poolInventory(svc, pool).Describe(c)
	if err != nil || len(s.Instances) == 0 {
		return harness.Fail("no worker to orphan: %v", err)
	}
	node := s.Instances[0].ID
	d, err := controllerDeployment(c, svc)
	if err != nil {
		return err
	}
	replicas, _ := svc.Kube.Kubectl(c, "get", "deploy", d, "-o", "jsonpath={.spec.replicas}")
	restore := func() {
		n := strings.TrimSpace(string(replicas))
		if n == "" || n == "0" {
			n = "1"
		}
		_, _ = svc.Kube.Kubectl(c, "scale", "deploy", d, "--replicas="+n)
	}
	defer restore()
	if _, err := svc.Kube.Kubectl(c, "scale", "deploy", d, "--replicas=0"); err != nil {
		return err
	}
	if _, err := svc.EC2.SwapSecurityGroups(c, node, []string{iso}); err != nil {
		return err
	}
	start := c.Now()
	c.Event(node, "orphaned: controller scaled to 0, network cut", start)
	deadline := start.Add(45 * time.Minute)
	for {
		s, err := poolInventory(svc, pool).Describe(c)
		if err != nil {
			return err
		}
		alive := false
		for _, i := range s.Instances {
			if i.ID == node && (i.State == "running" || i.State == "pending") {
				alive = true
			}
		}
		if !alive {
			took := c.Now().Sub(start)
			c.Event(node, "self-terminated", c.Now())
			c.NFR(harness.NFRResult{ID: "NFR-R4", Subject: "orphaned worker", Measured: took.Minutes(), Unit: "min", Target: "powers off and terminates", Pass: true})
			return nil
		}
		if c.Now().After(deadline) {
			c.NFR(harness.NFRResult{ID: "NFR-R4", Subject: "orphaned worker", Measured: 45, Unit: "min", Target: "powers off and terminates", Pass: false,
				Detail: "still running after 45 min"})
			return harness.Fail("orphaned worker %s still running after 45 min", node)
		}
		if err := remote.RealSleep(c, 30*time.Second); err != nil {
			return err
		}
	}
}
