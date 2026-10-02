// SPDX-License-Identifier: FSL-1.1-ALv2

package scenarios

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/sloper-ai/cucina/internal/canary"
	"github.com/sloper-ai/cucina/slo"
	"github.com/sloper-ai/cucina/test/e2e/bazelrun"
	"github.com/sloper-ai/cucina/test/e2e/collect/awsinv"
	"github.com/sloper-ai/cucina/test/e2e/collect/bep"
	"github.com/sloper-ai/cucina/test/e2e/collect/execlog"
	"github.com/sloper-ai/cucina/test/e2e/harness"
	"github.com/sloper-ai/cucina/test/e2e/infra"
	"github.com/sloper-ai/cucina/test/e2e/nfr"
	"github.com/sloper-ai/cucina/test/e2e/remote"
)

// Worker metrics come from the EC2 workers' bb_worker endpoints, scraped by
// the chart's HTTP-SD job; its job name must contain "worker".
const workerSel = `job=~".*worker.*"`

// scaleInLimit bounds waiting for a pool to return to zero: idleTimeout +
// drainTimeout of the campaign pools plus EC2 termination.
const scaleInLimit = 25 * time.Minute

func addNFRs(c *harness.Context, rs []harness.NFRResult) {
	for _, r := range rs {
		c.NFR(r)
	}
}

// ---------------------------------------------------------------- baselines

// baselineScenario runs test/e2e/abseil/local-baseline.{sh,ps1} without
// remote execution: on the lane's client VM (speed-up, NFR-X2 outcomes) and,
// if the descriptor has one, on a worker-type baseline host
// (clients["<lane>-baseline"], NFR-P2's denominator).
func baselineScenario(id string, lane Lane) *harness.Scenario {
	req := []harness.Requirement{harness.RequiresAWS}
	if lane.Host != infra.DevMac {
		req = append(req, harness.Requirement(lane.Host))
	} else {
		req = []harness.Requirement{harness.RequiresMacHost}
	}
	return &harness.Scenario{
		ID: id, Title: "Local baseline (" + lane.Name + "): Abseil build + test without remote execution",
		Requires: req, Cost: harness.CostMedium, EstimateUSD: 3, Timeout: 4 * time.Hour, NFRs: []string{"NFR-P2", "NFR-X2"},
		Run: func(c *harness.Context) error {
			svc, err := infra.Of(c)
			if err != nil {
				return err
			}
			hosts := []string{lane.Host}
			if _, ok := c.Env.Clients[lane.Name+"-baseline"]; ok {
				hosts = append(hosts, lane.Name+"-baseline")
			}
			for _, hn := range hosts {
				h, err := svc.Host(c, hn)
				if err != nil {
					return err
				}
				user := svc.ClientUser(hn)
				ws := hjoin(h, h.WorkDir(), "abseil")
				if err := c.Step(hn+": prepare abseil", func() error {
					return bazelrun.PrepareAbseil(c, h, bazelrun.AbseilPin{Repo: c.Env.Abseil.Repo, Tag: c.Env.Abseil.Tag, Commit: c.Env.Abseil.Commit},
						ws, overlayDir(c.Env), lane.Toolchain, user)
				}); err != nil {
					return err
				}
				ur := bazelrun.UserRC{OS: h.OS(), RepositoryCache: hjoin(h, h.WorkDir(), "repository-cache"), Hermetic: lane.Toolchain == bazelrun.HermeticLLVM}
				if h.OS() == remote.Windows {
					if ur.VC, ur.VCFullVersion, ur.WinSDKFullVersion, err = bazelrun.DetectWindowsPins(c, h); err != nil {
						return err
					}
				}
				if err := bazelrun.WriteRCFiles(c, h, ws, ur, "# local baseline: no remote execution\n", c.Dir()); err != nil {
					return err
				}
				script, out, err := baselineScript(c, h, ws)
				if err != nil {
					return err
				}
				var res remote.Result
				if err := c.Step(hn+": local build + test", func() error {
					_, r, err := remote.RunJob(c, h, script, remote.Opts{User: user}, 30*time.Second, nil)
					res = r
					return err
				}); err != nil {
					return err
				}
				var b struct {
					BuildExit, TestExit                    int
					BuildSeconds, TestSeconds, WallSeconds float64
					CPUs                                   int
				}
				if err := json.Unmarshal([]byte(lastLine(string(res.Stdout))), &b); err != nil {
					return fmt.Errorf("%s: baseline output %q: %w", hn, lastLine(string(res.Stdout)), err)
				}
				key := "baseline." + hn
				c.Metric(key+".build_seconds", b.BuildSeconds, "s")
				c.Metric(key+".test_seconds", b.TestSeconds, "s")
				c.Metric(key+".build_exit", float64(b.BuildExit), "")
				c.Metric(key+".test_exit", float64(b.TestExit), "")
				c.Metric(key+".cpus", float64(b.CPUs), "")
				local := filepath.Join(c.Dir(), hn+"-bep.json")
				if err := h.Get(c, hjoin(h, out, "bep.json"), local); err == nil {
					if outcomes, err := testOutcomes(local); err == nil {
						c.Record(key+".tests", outcomes)
					}
				}
				if b.BuildExit != 0 {
					c.Note("%s: local build exited %d (recorded; the baseline is a comparison, not a gate)", hn, b.BuildExit)
				}
			}
			return nil
		},
	}
}

func baselineScript(c *harness.Context, h remote.Host, ws string) (script, out string, err error) {
	out = hjoin(h, h.WorkDir(), "baseline-out")
	name := "local-baseline.sh"
	if h.OS() == remote.Windows {
		name = "local-baseline.ps1"
	}
	dst := hjoin(h, h.WorkDir(), name)
	if err := h.Put(c, filepath.Join(overlayDir(c.Env), name), dst); err != nil {
		return "", "", err
	}
	if h.OS() == remote.Windows {
		return fmt.Sprintf("& '%s' -Workspace '%s' -Out '%s'", dst, ws, out), out, nil
	}
	cfg := ""
	if h.OS() == remote.Linux {
		cfg = " --config=hermetic"
	}
	return fmt.Sprintf("sh %s %s %s%s", quoteFor(h, dst), quoteFor(h, ws), quoteFor(h, out), cfg), out, nil
}

// testOutcomes reads label → overall status from a BEP JSON file.
func testOutcomes(path string) (map[string]string, error) {
	s, err := bep.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, t := range s.Tests {
		out[t.Label] = t.Status
	}
	return out, nil
}

// baselineWalls returns the local build wall times from the baseline
// scenario's results: worker-type host and client VM.
func baselineWalls(c *harness.Context, lane Lane) (worker, client time.Duration) {
	p, ok := c.Prior["baseline-"+lane.Name]
	if !ok {
		return 0, 0
	}
	sec := func(k string) time.Duration {
		if m, ok := p.Metrics[k]; ok {
			return time.Duration(m.Value * float64(time.Second))
		}
		return 0
	}
	return sec("baseline." + lane.Name + "-baseline.build_seconds"), sec("baseline." + lane.Host + ".build_seconds")
}

// ---------------------------------------------------------------- T1 / T4

func coldScenario(id string, lane Lane) *harness.Scenario {
	return &harness.Scenario{
		ID: id, Title: fmt.Sprintf("%s cold: empty cache, pool at zero, fresh output base; build + test", title(lane.Name)),
		Requires: []harness.Requirement{harness.RequiresAWS, harness.Requirement(lane.Host), harness.RequiresPrometheus, harness.RequiresCucinactl},
		Cost:     harness.CostHigh, EstimateUSD: 12, MaxInstances: 6, Essential: true, Timeout: 3 * time.Hour,
		DependsOn: []string{"T0"},
		NFRs:      []string{"NFR-P1", "NFR-P2", "NFR-T1", "NFR-T4", "NFR-T8", "NFR-M1", "NFR-M2", "NFR-C4"},
		Post:      append([]harness.Check{ZeroResidueCheck("pool back at zero")}, Guards...),
		Run:       func(c *harness.Context) error { return runCold(c, lane) },
	}
}

func runCold(c *harness.Context, lane Lane) error {
	svc, err := infra.Of(c)
	if err != nil {
		return err
	}
	pool := lane.Pool(c.Env)
	if r, err := poolInventory(svc, pool).Describe(c); err == nil && len(r.Instances) > 0 {
		return harness.Fail("pool %s is not at zero before the cold build (%d instances)", pool, len(r.Instances))
	}
	lr, err := openLane(c, lane)
	if err != nil {
		return err
	}
	start := c.Now()
	var build, test *bazelrun.Outcome
	usage, err := sampled(c, 10*time.Second, func() error {
		var err error
		if build, err = lr.bazel("cold-build", BuildOpts{Command: "build", FreshServer: true}); err != nil {
			return err
		}
		if err := mustSucceed(build, "cold build"); err != nil {
			return err
		}
		if test, err = lr.bazel("cold-test", BuildOpts{Command: "test"}); err != nil {
			return err
		}
		return mustSucceed(test, "cold test")
	})
	end := c.Now()
	if build != nil && build.BEP != nil && build.BEP.RemoteCacheHitRatio() > 0.01 {
		c.Note("the cache was not empty: %.1f%% remote cache hits on the cold build", 100*build.BEP.RemoteCacheHitRatio())
	}
	if err != nil {
		return err
	}

	// NFR-P1: cold-start samples of this scale-out.
	firstSubmit := build.BEP.Started.Add(build.TimeToFirstRemoteAction)
	var samples []time.Duration
	if pi, err := describePool(c, svc, pool); err == nil {
		samples = coldStarts(pi.Starts, start, end, firstSubmit)
		c.Record("starts", pi.Starts)
	} else {
		c.Note("pools describe: %v", err)
	}
	addNFRs(c, nfr.ColdStart(lane.OSName, samples))
	var coldStart time.Duration
	if len(samples) > 0 {
		coldStart = samples[0]
		for _, s := range samples {
			coldStart = min(coldStart, s)
		}
	}
	// NFR-P2 against the local baselines.
	worker, client := baselineWalls(c, lane)
	addNFRs(c, nfr.ColdBuild(lane.OSName, build.Wall, coldStart, worker, client))
	if worker == 0 {
		c.Note("NFR-P2 not evaluated for %s: run baseline-%s with a %s-baseline host first", lane.Name, lane.Name, lane.Name)
	}
	// NFR-T1: downloaded output bytes vs total action output bytes.
	if mb, err := lr.materializedBytes(); err == nil && build.ExecLog != nil {
		c.NFR(nfr.Ratio("NFR-T1", lane.Name, float64(mb), float64(build.ExecLog.OutputBytes), 10, true,
			fmt.Sprintf("materialized output tree vs execution-log output bytes; host NIC received %d bytes", build.BEP.NetworkBytesReceived)))
	}
	// NFR-T8: worker CAS fetches vs compile input roots (virtual build directories).
	if build.ExecLogPath != "" {
		if l, err := execlog.ReadFile(build.ExecLogPath); err == nil {
			var compileInputs int64
			for _, s := range l.Spawns {
				if s.Remote() && execlog.Classify(s.Mnemonic) == execlog.ClassCompileLink && strings.HasPrefix(s.Mnemonic, "CppCompile") {
					compileInputs += s.InputBytes
				}
			}
			q := fmt.Sprintf(`sum(increase(buildbarn_blobstore_blob_access_operations_blob_size_bytes_sum{%s,storage_type="CAS",backend_type="grpc",operation="Get"}[%ds]))`,
				workerSel, int(end.Sub(start).Seconds()))
			if fetched, ok := promScalar(c, q, end); ok {
				c.NFR(nfr.Ratio("NFR-T8", lane.Name, fetched, float64(compileInputs), 20, true, "worker CAS Get bytes from L3 vs CppCompile input-root bytes (all remote actions' fetches counted: conservative)"))
			}
		}
	}
	// NFR-M1 under load and NFR-M2 component peaks.
	ns := namespace(c.Env)
	if v, ok := promMax(c, fmt.Sprintf(`%s{namespace=%q}`, slo.ControlPlaneRSS, ns), start, end); ok {
		oom, _ := promScalar(c, fmt.Sprintf(`sum(%s{namespace=%q}) or vector(0)`, slo.OOMKills, ns), end)
		addNFRs(c, nfr.ControlPlaneMemory(0, v, int(oom)))
	}
	if v, ok := promMax(c, fmt.Sprintf(`max(%s{namespace=%q,pod=~".*controller.*"})`, slo.PodRSS, ns), start, end); ok {
		c.NFR(nfr.ComponentLimit("cucina-controller", v, 256<<20))
	}
	if v, ok := promMax(c, fmt.Sprintf(`max(process_resident_memory_bytes{%s})`, workerSelector(c, lane.Name)), start, end); ok {
		c.NFR(nfr.ComponentLimit("bb_worker", v, 1<<30))
	}
	// NFR-T4: the endpoint advertises zstd; Bazel uses --remote_cache_compression.
	compression(c, lr)
	// §10.4 control-plane/worker view of the build and §10.2 action counts.
	promSnapshot(c, lane.Name, start, end)
	if counts, _, err := lr.aquerySummary(); err == nil {
		c.Record(lane.Name+".aquery_summary", counts)
		c.Metric(lane.Name+".actions_total", float64(counts["total"]), "")
	} else {
		c.Note("aquery --output=summary: %v", err)
	}
	// NFR-C4: what the controller launched.
	costDefaults(c, svc, usage.Lifecycles)
	// Back to zero.
	took, r, err := waitZero(c, scaleInLimit)
	if err != nil {
		return err
	}
	c.Metric("scale_to_zero_seconds", took.Seconds(), "s")
	if !r.Zero() {
		return harness.Fail("pool did not return to zero within %s: %s", scaleInLimit, r)
	}
	return nil
}

// compression records NFR-T4 evidence for the client↔frontend path: Bazel
// sends --remote_cache_compression (from cucina.bazelrc) and the endpoint
// advertises ZSTD (GetCapabilities through the canary probe). The wire ratio
// itself is not observable server-side (ADR 1005); the host↔control-plane
// path is measured in T13 from hostd's WAN counters.
func compression(c *harness.Context, lr *laneRun) {
	res, err := lr.host.Run(c, readFileScript(lr.host, hjoin(lr.host, lr.ws, "cucina.bazelrc")), remote.Opts{User: lr.user})
	flag := err == nil && strings.Contains(string(res.Stdout), "--remote_cache_compression")
	advertised := false
	if ts, err := stsToken(c); err == nil {
		r := (&canary.Probe{Kind: canary.KindCache, Endpoint: endpoint(c.Env), Tokens: ts, Timeout: time.Minute}).Run(c)
		for _, x := range r.Compressors {
			advertised = advertised || x == "ZSTD"
		}
	}
	c.NFR(harness.NFRResult{ID: "NFR-T4", Subject: lr.lane.Name + " client↔frontend", Measured: b2f(flag && advertised), Unit: "zstd negotiated",
		Target: "zstd active", Pass: flag && advertised,
		Detail: fmt.Sprintf("--remote_cache_compression in cucina.bazelrc: %v; endpoint advertises ZSTD: %v", flag, advertised)})
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func readFileScript(h remote.Host, p string) string {
	if h.OS() == remote.Windows {
		return "Get-Content -Raw '" + p + "'"
	}
	return "cat " + quoteFor(h, p)
}

// costDefaults evaluates NFR-C4 on what was launched: gp3 root volumes, no
// EIPs, public IPv4 only where no other egress path exists.
func costDefaults(c *harness.Context, svc *infra.Services, lcs []awsinv.Lifecycle) {
	var bad []string
	maxRoot := 0
	for _, l := range lcs {
		for _, v := range l.Volumes {
			maxRoot = max(maxRoot, int(v.GiB))
			if v.IOPS > 3000 || v.MiBps > 125 {
				bad = append(bad, fmt.Sprintf("%s: %d IOPS/%d MiB/s above the gp3 baseline", v.ID, v.IOPS, v.MiBps))
			}
		}
		if l.PublicIP {
			bad = append(bad, l.ID+": public IPv4 (needs the R-DATA-4 fallback ADR)")
		}
	}
	if len(lcs) == 0 {
		return
	}
	c.NFR(harness.NFRResult{ID: "NFR-C4", Subject: "launched workers", Measured: float64(len(bad)), Unit: "violations", Target: "0", Pass: len(bad) == 0,
		Detail: fmt.Sprintf("largest root volume %d GiB; %s", maxRoot, strings.Join(bad, "; "))})
}

// ---------------------------------------------------------------- T2 / T5

func warmScenario(id, dep string, lane Lane) *harness.Scenario {
	return &harness.Scenario{
		ID: id, Title: title(lane.Name) + " warm: bazel clean --expunge, then build + test again",
		Requires: []harness.Requirement{harness.RequiresAWS, harness.Requirement(lane.Host)},
		Cost:     harness.CostLow, EstimateUSD: 1, Essential: true, Timeout: 90 * time.Minute, DependsOn: []string{dep},
		NFRs: []string{"NFR-P3", "NFR-M3"},
		Post: append([]harness.Check{ZeroResidueCheck("no worker launched")}, Guards...),
		Run: func(c *harness.Context) error {
			lr, err := openLane(c, lane)
			if err != nil {
				return err
			}
			if _, err := lr.bazel("expunge", BuildOpts{Command: "clean", Extra: []string{"--expunge"}}); err != nil {
				return err
			}
			var build, test *bazelrun.Outcome
			usage, err := sampled(c, 10*time.Second, func() error {
				var err error
				if build, err = lr.bazel("warm-build", BuildOpts{Command: "build", FreshServer: true}); err != nil {
					return err
				}
				if err := mustSucceed(build, "warm build"); err != nil {
					return err
				}
				if test, err = lr.bazel("warm-test", BuildOpts{Command: "test"}); err != nil {
					return err
				}
				return mustSucceed(test, "warm test")
			})
			if err != nil {
				return err
			}
			launched := 0
			for _, l := range usage.Lifecycles {
				if l.Pool == lane.Pool(c.Env) {
					launched++
				}
			}
			cold := coldWall(c, dep, lane)
			hits := build.BEP.RemoteCacheHitRatio()
			if build.ExecLog != nil {
				hits = build.ExecLog.RemoteCacheHitRatio()
			}
			addNFRs(c, nfr.WarmRebuild(lane.OSName, hits, launched, build.Wall, cold))
			// NFR-M3: Bazel server peak RSS with BwoB (this build) vs full downloads.
			if _, err := lr.bazel("expunge-2", BuildOpts{Command: "clean", Extra: []string{"--expunge"}}); err != nil {
				return err
			}
			full, err := lr.bazel("warm-build-download-all", BuildOpts{Command: "build", FreshServer: true, Extra: []string{"--remote_download_outputs=all"}})
			if err != nil {
				return err
			}
			if build.PeakServerRSSBytes > 0 {
				c.NFR(nfr.ClientPeakRSS(lane.OSName, build.PeakServerRSSBytes, full.PeakServerRSSBytes))
			} else {
				c.NFR(nfr.Reported("NFR-M3", lane.OSName, float64(build.BEP.PeakPostGCHeapBytes)/(1<<20), "MiB heap (BEP MemoryMetrics)",
					fmt.Sprintf("full downloads: %.0f MiB heap", float64(full.BEP.PeakPostGCHeapBytes)/(1<<20))))
			}
			return nil
		},
	}
}

func coldWall(c *harness.Context, dep string, lane Lane) time.Duration {
	if p, ok := c.Prior[dep]; ok {
		if m, ok := p.Metrics[lane.Name+".cold-build.wall_seconds"]; ok {
			return time.Duration(m.Value * float64(time.Second))
		}
	}
	return 0
}

// ---------------------------------------------------------------- T3 / T6

func l1Scenario(id, dep string, lane Lane) *harness.Scenario {
	return &harness.Scenario{
		ID: id, Title: title(lane.Name) + " L1/L3: re-execute with --noremote_accept_cached while workers are up, then after scale-to-zero",
		Requires: []harness.Requirement{harness.RequiresAWS, harness.Requirement(lane.Host), harness.RequiresPrometheus},
		Cost:     harness.CostHigh, EstimateUSD: 10, MaxInstances: 6, Essential: true, Timeout: 3 * time.Hour, DependsOn: []string{dep},
		NFRs: []string{"NFR-T3", "NFR-P4"},
		Post: append([]harness.Check{ZeroResidueCheck("instances terminated with their volumes")}, Guards...),
		Run: func(c *harness.Context) error {
			lr, err := openLane(c, lane)
			if err != nil {
				return err
			}
			reexec := func(name string) (*bazelrun.Outcome, time.Time, time.Time, error) {
				start := c.Now()
				o, err := lr.bazel(name, BuildOpts{Command: "build", Extra: []string{"--noremote_accept_cached"}})
				if err == nil {
					err = mustSucceed(o, name)
				}
				return o, start, c.Now(), err
			}
			l1Share := func(from, to time.Time) (float64, float64, bool) {
				w := int(to.Sub(from).Seconds())
				sel := workerSelector(c, lane.Name)
				local, ok1 := promScalar(c, fmt.Sprintf(`sum(increase(buildbarn_blobstore_blob_access_operations_blob_size_bytes_sum{%s,storage_type="CAS",backend_type="local",operation="Get"}[%ds]))`, sel, w), to)
				grpcBytes, ok2 := promScalar(c, fmt.Sprintf(`sum(increase(buildbarn_blobstore_blob_access_operations_blob_size_bytes_sum{%s,storage_type="CAS",backend_type="grpc",operation="Get"}[%ds]))`, sel, w), to)
				return local, local + grpcBytes, ok1 && ok2
			}
			// First run: the cold build's workers are still up (warm L1).
			_, err = sampled(c, 10*time.Second, func() error {
				// Warm the pool first: a short re-execution that scales it out.
				if _, _, _, err := reexec("warm-up"); err != nil {
					return err
				}
				o, s, e, err := reexec("reexec-warm-l1")
				if err != nil {
					return err
				}
				if l, total, ok := l1Share(s, e); ok {
					c.NFR(nfr.Ratio("NFR-T3", lane.Name+" L1 within one scale-out", l, total, 90, false, "worker CAS Get bytes served by the local L1"))
				}
				if o.ExecLog != nil {
					addNFRs(c, nfr.QueueAndOverhead(lane.Name+" warm L1", o.ExecLog.Queue.P95, o.ExecLog.WorkerOverhead.P50, o.ExecLog.WorkerOverhead.P95))
				}
				return nil
			})
			if err != nil {
				return err
			}
			// Scale to zero: instances must be gone with their volumes.
			took, r, err := waitZero(c, scaleInLimit)
			if err != nil {
				return err
			}
			c.Metric("scale_to_zero_seconds", took.Seconds(), "s")
			if !r.Zero() {
				return harness.Fail("not at zero after %s: %s", took, r)
			}
			// Second run: inputs come from the same-AZ L3.
			_, err = sampled(c, 10*time.Second, func() error {
				_, s, e, err := reexec("reexec-after-zero")
				if err != nil {
					return err
				}
				if l, total, ok := l1Share(s, e); ok {
					c.NFR(nfr.Reported("NFR-T3", lane.Name+" after scale-to-zero (from L3)", 100*(1-l/maxf(total, 1)), "% of input bytes from L3",
						fmt.Sprintf("%.0f bytes from L3 in %s (%.1f MB/s); same-AZ private path: no transfer charge", total-l, e.Sub(s).Round(time.Second), (total-l)/1e6/e.Sub(s).Seconds())))
				}
				return nil
			})
			return err
		},
	}
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// title capitalises an ASCII lane name ("linux" → "Linux").
func title(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
