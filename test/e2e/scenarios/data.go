// SPDX-License-Identifier: FSL-1.1-ALv2

package scenarios

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sloper-ai/cucina/test/e2e/bazelrun"
	"github.com/sloper-ai/cucina/test/e2e/harness"
	"github.com/sloper-ai/cucina/test/e2e/nfr"
	"github.com/sloper-ai/cucina/test/e2e/remote"
)

// t21: seed the remote repo contents cache from each client platform (Bazel
// ≥ 9.0.2 keys it by client host OS/CPU, R-DATA-2) with the trusted writer,
// then cold-build a cross target from a fresh output base and an empty
// repository cache on each client: what lands in the repository cache is
// exactly what the client downloaded (NFR-T7). A run without the repo
// contents cache gives the comparison. The fresh Linux client's upload bytes
// against T1's first client give NFR-T2.
func t21() *harness.Scenario {
	return &harness.Scenario{
		ID: "T21", Title: "Repo contents cache: seed with the trusted writer, then fresh-client cold builds (Linux, Windows, macOS targets)",
		Requires: []harness.Requirement{harness.RequiresAWS, harness.RequiresLinuxClient, harness.RequiresCrossMatrix, harness.RequiresCucinactl},
		Cost:     harness.CostMedium, EstimateUSD: 6, Essential: true, Timeout: 5 * time.Hour, DependsOn: []string{"T1"},
		NFRs: []string{"NFR-T7", "NFR-T2"}, Post: Guards,
		Run: func(c *harness.Context) error {
			if err := xplatPrecheck(c); err != nil {
				return err
			}
			m, err := newMatrix(c)
			if err != nil {
				return err
			}
			plan := []struct {
				lane   Lane
				target string
			}{{LinuxLane, "x86_64-linux-musl"}, {WindowsLane, "x86_64-windows-msvc"}, {MacLane, "aarch64-apple-darwin"}}
			var failed []string
			for _, p := range plan {
				if !laneAvailable(c.Env, p.lane) {
					c.Note("T21: %s skipped (no %s)", p.target, p.lane.Host)
					continue
				}
				if err := repoCacheRun(c, m, p.lane, p.target); err != nil {
					failed = append(failed, p.target+": "+err.Error())
				}
			}
			if len(failed) > 0 {
				return harness.Fail("%s", strings.Join(failed, "; "))
			}
			return nil
		},
	}
}

func repoCacheRun(c *harness.Context, m *matrix, lane Lane, target string) error {
	ml, err := m.lane(lane)
	if err != nil {
		return err
	}
	lr := ml.lr
	tgt, ok := m.targets.target(target)
	if !ok {
		return fmt.Errorf("%s is not in platforms/targets.json", target)
	}
	cfg, err := planConfig(tgt, lane, "", ScopeCoverage)
	if err != nil {
		return err
	}
	raw, err := m.crossRC(lr, cfg)
	if err != nil {
		return err
	}
	h := lr.host
	rep := rcContract(m.targets, cfg, h.OS(), raw)
	if rep.Fatal {
		return fmt.Errorf("cucinactl bazelrc --cross: %s", strings.Join(rep.Issues, "; "))
	}
	// A build: no test step, so no @bazel_tools overlay.
	rc, _ := composeRC(raw, cfg, "", rep.Fix)
	rcFile := hjoin(h, lr.ws, "cucina-t21.bazelrc")
	local := filepath.Join(c.Dir(), lane.Name+"-t21.bazelrc")
	if err := writeAndPut(c, h, local, rcFile, rc); err != nil {
		return err
	}
	run := func(name string, repoContents bool) (int64, *bazelrun.Outcome, error) {
		repoCache := hjoin(h, h.WorkDir(), "t21", name, "repository-cache")
		startup := []string{"--bazelrc=" + rcFile, "--output_base=" + hjoin(h, h.WorkDir(), "t21", name, "ob")}
		// cucinactl's configuration enables the repo contents cache itself;
		// the command line wins over the rc file for the comparison run.
		if repoContents {
			startup = append(startup, "--experimental_remote_repo_contents_cache")
		} else {
			startup = append(startup, "--noexperimental_remote_repo_contents_cache")
		}
		inv := bazelrun.Invocation{Name: "T21-" + lane.Name + "-" + name, Host: h, Workspace: lr.ws, User: lr.user, Collect: true, Startup: startup, Bazel: bazelBinary(c.Env, h),
			Command: "build", Args: []string{"--repository_cache=" + repoCache, "--lockfile_mode=off", "--", "//absl/strings/..."}}
		o, err := bazelrun.Run(c, inv, filepath.Join(c.Dir(), inv.Name))
		if err != nil {
			return 0, nil, err
		}
		recordOutcome(c, "t21."+lane.Name+"."+name, o)
		if err := mustSucceed(o, inv.Name); err != nil {
			return 0, o, err
		}
		n, err := dirBytes(c, h, repoCache, lr.user)
		return n, o, err
	}
	if _, _, err := run("seed", true); err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	cached, fresh, err := run("fresh-with-cache", true)
	if err != nil {
		return err
	}
	full, _, err := run("fresh-without-cache", false)
	if err != nil {
		return err
	}
	c.NFR(nfr.AtMostBytes("NFR-T7", lane.Name+" client ("+target+")", float64(cached), 50e6,
		fmt.Sprintf("without the repo contents cache: %.0f MB", float64(full)/1e6)))
	if lane.Name == "linux" {
		if p, ok := c.Prior["T1"]; ok {
			if first, ok := p.Metrics["linux.cold-build.network_bytes_sent"]; ok && fresh.BEP != nil {
				c.NFR(nfr.Ratio("NFR-T2", "fresh Linux client vs first client uploads", float64(fresh.BEP.NetworkBytesSent), first.Value, 1, true,
					"BEP NetworkMetrics bytes sent (a fresh output base on the same VM stands in for a second client)"))
			}
		}
	}
	return nil
}

func writeAndPut(c *harness.Context, h remote.Host, local, dst, content string) error {
	if err := os.WriteFile(local, []byte(content), 0o644); err != nil {
		return err
	}
	return h.Put(c, local, dst)
}

// dirBytes sums the sizes of the files under dir on the host.
func dirBytes(c *harness.Context, h remote.Host, dir, user string) (int64, error) {
	var script string
	if h.OS() == remote.Windows {
		script = fmt.Sprintf("$s = (Get-ChildItem -Recurse -File -Force '%s' -ErrorAction SilentlyContinue | Measure-Object -Sum Length).Sum; if ($s) { $s } else { 0 }", dir)
	} else {
		script = fmt.Sprintf("find %s -type f -exec cat {} + 2>/dev/null | wc -c", quoteFor(h, dir))
	}
	res, err := h.Run(c, script, remote.Opts{User: user})
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(lastLine(string(res.Stdout))), 10, 64)
}

// laneAvailable reports whether the environment has the lane's client.
func laneAvailable(env *harness.Env, l Lane) bool {
	if l.Host == "dev-mac" {
		return env.Has(harness.RequiresMacHost)
	}
	return env.Has(harness.Requirement(l.Host))
}
