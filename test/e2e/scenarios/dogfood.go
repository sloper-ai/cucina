// SPDX-License-Identifier: FSL-1.1-ALv2

package scenarios

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/sloper-ai/cucina/test/e2e/bazelrun"
	"github.com/sloper-ai/cucina/test/e2e/harness"
	"github.com/sloper-ai/cucina/test/e2e/infra"
	"github.com/sloper-ai/cucina/test/e2e/nfr"
	"github.com/sloper-ai/cucina/test/e2e/remote"
)

// t22 builds and tests the Cucina repository itself through Cucina (R-BUILD):
// Linux and macOS lanes from linux-client, the Windows lane from
// windows-client, a warm rerun, the macOS lane from the dev Mac, and a local
// build on the dev Mac for comparison.
func t22() *harness.Scenario {
	return &harness.Scenario{
		ID: "T22", Title: "Dogfood: the Cucina repository built and tested through Cucina",
		Requires: []harness.Requirement{harness.RequiresAWS, harness.RequiresLinuxClient, harness.RequiresCucinactl},
		Cost:     harness.CostHigh, EstimateUSD: 15, MaxInstances: 10, Essential: true, Timeout: 6 * time.Hour,
		Post: Guards,
		Run:  runT22,
	}
}

type cucinaCheckout struct {
	lr *laneRun
	ws string
}

// cloneCucina clones this repository at the pinned commit on a host and
// writes user.bazelrc from `cucinactl bazelrc --platform …` (the repo's
// .bazelrc try-imports it).
func cloneCucina(c *harness.Context, lane Lane) (*cucinaCheckout, error) {
	pin := c.Env.Cucina
	if pin == nil || pin.URL == "" || pin.Commit == "" {
		return nil, harness.Skip("no cucina {url, commit} in the environment descriptor (the feature branch must be pushed)")
	}
	svc, err := infra.Of(c)
	if err != nil {
		return nil, err
	}
	h, err := svc.Host(c, lane.Host)
	if err != nil {
		return nil, err
	}
	user := svc.ClientUser(lane.Host)
	ws := hjoin(h, h.WorkDir(), "cucina")
	var script string
	if h.OS() == remote.Windows {
		script = fmt.Sprintf(`$ErrorActionPreference = 'Continue'
if (-not (Test-Path '%[1]s\.git')) { git clone --quiet %[2]s '%[1]s'; if ($LASTEXITCODE) { exit $LASTEXITCODE } }
Set-Location '%[1]s'; git fetch --quiet origin %[3]s; git checkout --quiet --detach %[3]s; exit $LASTEXITCODE`, ws, pin.URL, pin.Commit)
	} else {
		script = fmt.Sprintf(`set -e
[ -d %[1]s/.git ] || git clone --quiet %[2]s %[1]s
cd %[1]s && git fetch --quiet origin %[3]s && git checkout --quiet --detach %[3]s`, quoteFor(h, ws), quoteFor(h, pin.URL), quoteFor(h, pin.Commit))
	}
	if err := c.Step(lane.Host+": clone cucina @"+pin.Commit[:min(12, len(pin.Commit))], func() error {
		res, err := h.Run(c, script, remote.Opts{User: user, Timeout: 20 * time.Minute})
		if err != nil {
			return err
		}
		return res.Err()
	}); err != nil {
		return nil, err
	}
	cli, err := installCucinactl(c, svc, h)
	if err != nil {
		return nil, err
	}
	res, err := h.Run(c, fmt.Sprintf("%s bazelrc --platform %s", quoteFor(h, cli), lane.Platform), remote.Opts{User: user})
	if err != nil {
		return nil, err
	}
	if err := res.Err(); err != nil {
		return nil, err
	}
	local := filepath.Join(c.Dir(), lane.Host+"-cucina-user.bazelrc")
	if err := writeAndPut(c, h, local, hjoin(h, ws, "user.bazelrc"), string(res.Stdout)); err != nil {
		return nil, err
	}
	return &cucinaCheckout{lr: &laneRun{c: c, svc: svc, lane: lane, host: h, ws: ws, user: user}, ws: ws}, nil
}

func (cc *cucinaCheckout) bazel(name, command string, fresh bool, extra ...string) (*bazelrun.Outcome, error) {
	return cc.bazelWith(name, command, fresh, nil, extra...)
}

func (cc *cucinaCheckout) bazelWith(name, command string, fresh bool, startup []string, extra ...string) (*bazelrun.Outcome, error) {
	c := cc.lr.c
	inv := bazelrun.Invocation{Name: c.Scenario.ID + "-" + name, Host: cc.lr.host, Workspace: cc.ws, User: cc.lr.user, Collect: true, Bazel: bazelBinary(c.Env, cc.lr.host),
		FreshServer: fresh, Startup: startup, Command: command, Args: append(append([]string{}, extra...), "--", "//...")}
	var o *bazelrun.Outcome
	err := c.Step(cc.lr.host.Name()+": bazel "+command+" "+strings.Join(extra, " "), func() error {
		var err error
		o, err = bazelrun.Run(c, inv, filepath.Join(c.Dir(), name))
		return err
	})
	if o != nil {
		recordOutcome(c, "dogfood."+name, o)
	}
	return o, err
}

func runT22(c *harness.Context) error {
	linux, err := cloneCucina(c, LinuxLane)
	if err != nil {
		return err
	}
	var failed []string
	gate := func(o *bazelrun.Outcome, err error, what string, minRemote float64) {
		switch {
		case err != nil:
			failed = append(failed, what+": "+err.Error())
		case !o.Succeeded():
			failed = append(failed, fmt.Sprintf("%s: bazel exited %d", what, o.ExitCode))
		case o.ExecLog != nil && minRemote > 0:
			r := o.ExecLog.RemoteRatio()
			c.Check(harness.CheckResult{Name: what + ": ≥ 95 % remote", Kind: "bazel", Pass: r >= minRemote, Value: fmt.Sprintf("%.1f%%", 100*r)})
			if r < minRemote {
				failed = append(failed, fmt.Sprintf("%s: %.1f%% remote", what, 100*r))
			}
		}
	}
	var linuxCold *bazelrun.Outcome
	_, err = sampled(c, 15*time.Second, func() error {
		o, err := linux.bazel("linux", "test", true, "--config=cucina")
		linuxCold = o
		gate(o, err, "Linux lane from linux-client", 0.95)
		o, err = linux.bazel("macos-from-linux", "test", false, "--config=cucina-macos")
		gate(o, err, "macOS lane from linux-client", 0.95)
		if c.Env.Has(harness.RequiresWindowsClient) {
			win, err := cloneCucina(c, WindowsLane)
			if err != nil {
				failed = append(failed, "windows-client: "+err.Error())
			} else {
				o, err := win.bazel("windows", "test", true, "--config=cucina")
				gate(o, err, "Windows lane from windows-client", 0.95)
			}
		}
		// Warm rerun after clean --expunge.
		if _, err := linux.bazel("expunge", "clean", false, "--expunge"); err != nil {
			return err
		}
		o, err = linux.bazel("linux-warm", "test", true, "--config=cucina")
		gate(o, err, "warm rerun", 0)
		if o != nil && o.ExecLog != nil {
			c.NFR(nfr.CacheHits("NFR-P3", "dogfood warm rerun", o.ExecLog.RemoteCacheHitRatio()))
		}
		return nil
	})
	if err != nil {
		return err
	}
	if c.Env.Has(harness.RequiresMacHost) {
		mac, err := cloneCucina(c, MacLane)
		if err != nil {
			failed = append(failed, "dev Mac: "+err.Error())
		} else {
			o, err := mac.bazel("macos-from-mac", "test", true, "--config=cucina-macos")
			gate(o, err, "macOS lane from the dev Mac", 0)
			if o != nil && o.ExecLog != nil {
				c.Metric("dogfood.macos-from-mac.cache_hit_ratio", o.ExecLog.RemoteCacheHitRatio(), "")
			}
			// Local comparison: own output base, remote execution and caches off.
			local, err := mac.bazelWith("local-on-mac", "test", true, []string{"--output_base=" + mac.ws + ".local-ob"},
				"--remote_executor=", "--remote_cache=", "--disk_cache=")
			if err == nil && o != nil {
				c.Metric("dogfood.local-on-mac.wall_seconds", local.Wall.Seconds(), "s")
				c.Note("dogfood on the dev Mac: through Cucina %s vs local %s", o.Wall.Round(time.Second), local.Wall.Round(time.Second))
			}
		}
	}
	if linuxCold != nil {
		c.Metric("dogfood.linux.wall_seconds", linuxCold.Wall.Seconds(), "s")
	}
	if len(failed) > 0 {
		return harness.Fail("%s", strings.Join(failed, "; "))
	}
	return nil
}
