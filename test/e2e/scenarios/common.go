// SPDX-License-Identifier: FSL-1.1-ALv2

// Package scenarios implements the §10.3 acceptance campaign (T0–T22), the
// local baselines and the canary scenarios on the test/e2e harness. Every
// scenario computes its pass criteria with package nfr and records the §10.4
// metrics; prerequisites the environment cannot meet yield SKIP with a
// reason. Scenarios talk to the system only through public interfaces:
// Bazel, cucinactl (--output json), kubectl/helm, Prometheus and
// tag-filtered AWS describes.
package scenarios

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sloper-ai/cucina/internal/cost"
	"github.com/sloper-ai/cucina/slo"
	"github.com/sloper-ai/cucina/test/e2e/bazelrun"
	"github.com/sloper-ai/cucina/test/e2e/collect/awsinv"
	"github.com/sloper-ai/cucina/test/e2e/collect/prom"
	"github.com/sloper-ai/cucina/test/e2e/collect/spend"
	"github.com/sloper-ai/cucina/test/e2e/harness"
	"github.com/sloper-ai/cucina/test/e2e/infra"
	"github.com/sloper-ai/cucina/test/e2e/remote"
)

// Lane is one client/pool pairing of the Abseil campaign.
type Lane struct {
	Name      string             // linux | windows | macos
	Host      string             // linux-client | windows-client | dev-mac
	PoolRole  string             // key in Env.Pools
	Platform  string             // cucinactl bazelrc --platform value
	Toolchain bazelrun.Toolchain // hermetic-llvm (Linux) or autodetect (MSVC, Xcode)
	OSName    string             // NFR-P1 target key
}

// RCConfig is the lane's configuration in test/e2e/abseil/abseil.bazelrc.
func (l Lane) RCConfig() string { return "lane-" + l.Name }

// Lanes of §10.3: Linux (T1–T3), Windows (T4–T6), macOS (T13).
var (
	LinuxLane   = Lane{Name: "linux", Host: "linux-client", PoolRole: "linux", Platform: "linux", Toolchain: bazelrun.HermeticLLVM, OSName: "linux"}
	WindowsLane = Lane{Name: "windows", Host: "windows-client", PoolRole: "windows", Platform: "windows", Toolchain: bazelrun.Autodetect, OSName: "windows"}
	MacLane     = Lane{Name: "macos", Host: infra.DevMac, PoolRole: "macos", Platform: "macos", Toolchain: bazelrun.Autodetect, OSName: "macos"}
)

// Pool returns the lane's pool name from the descriptor.
func (l Lane) Pool(env *harness.Env) string {
	if p := env.Pools[l.PoolRole]; p != "" {
		return p
	}
	return map[string]string{"linux": "linux-x86-64", "windows": "windows-x86-64", "macos": "macos-arm64-xcode27.0", "linux-arm64": "linux-aarch64"}[l.PoolRole]
}

// laneRun bundles what a lane's build needs.
type laneRun struct {
	c    *harness.Context
	svc  *infra.Services
	lane Lane
	host remote.Host
	ws   string // Abseil checkout on the host
	user string
}

func sep(h remote.Host) string {
	if h.OS() == remote.Windows {
		return `\`
	}
	return "/"
}

func hjoin(h remote.Host, parts ...string) string { return strings.Join(parts, sep(h)) }

// overlayDir is test/e2e/abseil in the dev Mac checkout.
func overlayDir(env *harness.Env) string {
	return filepath.Join(env.RepoDir, "test", "e2e", "abseil")
}

// openLane connects to the lane's host, prepares the Abseil checkout with the
// overlay, installs cucinactl, logs it in with the campaign service key and
// writes user.bazelrc + cucina.bazelrc (`cucinactl bazelrc --platform …`).
func openLane(c *harness.Context, lane Lane, extraRC ...string) (*laneRun, error) {
	svc, err := infra.Of(c)
	if err != nil {
		return nil, err
	}
	if c.Env.Abseil.Tag == "" || c.Env.Abseil.Commit == "" {
		return nil, harness.Skip("the environment descriptor pins no Abseil tag/commit")
	}
	h, err := svc.Host(c, lane.Host)
	if err != nil {
		return nil, err
	}
	lr := &laneRun{c: c, svc: svc, lane: lane, host: h, ws: hjoin(h, h.WorkDir(), "abseil"), user: svc.ClientUser(lane.Host)}
	if err := c.Step(lane.Name+": prepare abseil", func() error {
		return prepareAbseil(c, svc, h, lr.ws, lane.Toolchain, lr.user)
	}); err != nil {
		return nil, err
	}
	cli, err := installCucinactl(c, svc, h)
	if err != nil {
		return nil, err
	}
	var rc string
	if err := c.Step(lane.Name+": cucinactl bazelrc", func() error {
		res, err := h.Run(c, fmt.Sprintf("%s bazelrc --platform %s --ci --disk-cache=none --helper-path %s", quoteFor(h, cli), lane.Platform, argFor(h, credentialHelper(h))), remote.Opts{User: lr.user})
		if err != nil {
			return err
		}
		if err := res.Err(); err != nil {
			return err
		}
		rc = string(res.Stdout)
		return nil
	}); err != nil {
		return nil, err
	}
	checkClientFlags(c, lane, rc)
	rc += "\n# Added by the Cucina e2e harness: the lane's configuration in abseil.bazelrc.\ncommon --config=" + lane.RCConfig() + "\n"
	rc += strings.Join(extraRC, "\n") + "\n"
	user := bazelrun.UserRC{OS: h.OS(), RepositoryCache: hjoin(h, h.WorkDir(), "repository-cache")}
	if h.OS() == remote.Windows {
		if err := c.Step("windows: detect MSVC/SDK pins", func() error {
			var err error
			user.VC, user.VCFullVersion, user.WinSDKFullVersion, err = bazelrun.DetectWindowsPins(c, h)
			return err
		}); err != nil {
			return nil, err
		}
		c.Record("windowsPins", map[string]string{"BAZEL_VC_FULL_VERSION": user.VCFullVersion, "BAZEL_WINSDK_FULL_VERSION": user.WinSDKFullVersion})
	}
	if err := bazelrun.WriteRCFiles(c, h, lr.ws, user, rc, c.Dir()); err != nil {
		return nil, err
	}
	return lr, nil
}

// clientFlags are the §10.2 client flags `cucinactl bazelrc` must emit (UC9,
// R-DATA-1); each entry lists accepted spellings.
var clientFlags = [][]string{
	{"--remote_executor=grpcs://"},
	{"--remote_instance_name="},
	{"--remote_download_minimal", "--remote_download_outputs=minimal", "--remote_download_toplevel", "--remote_download_outputs=toplevel"},
	{"--remote_cache_compression"},
	{"--jobs=200"},
	{"--remote_retries=10"},
	{"--remote_retry_max_delay=30s"},
	{"--grpc_keepalive_time=30s"},
	{"--noremote_local_fallback", "--remote_local_fallback=false"},
	{"--credential_helper="},
}

// forbiddenFlags must never appear (§10.2, R-DATA-1).
var forbiddenFlags = []string{"--experimental_remote_merkle_tree_cache", "--experimental_remote_cache_chunking", "--remote_header=Authorization"}

// checkClientFlags records whether the emitted .bazelrc carries the §10.2
// client flags (a failed check fails the scenario).
func checkClientFlags(c *harness.Context, lane Lane, rc string) {
	var missing, bad []string
	for _, alts := range clientFlags {
		found := false
		for _, a := range alts {
			found = found || strings.Contains(rc, a)
		}
		if !found {
			missing = append(missing, alts[0])
		}
	}
	for _, f := range forbiddenFlags {
		if strings.Contains(rc, f) {
			bad = append(bad, f)
		}
	}
	c.Check(harness.CheckResult{Name: lane.Name + ": cucinactl bazelrc emits the §10.2 client flags", Kind: "cli",
		Pass: len(missing) == 0 && len(bad) == 0, Detail: fmt.Sprintf("missing %v, forbidden %v", missing, bad)})
}

func argFor(h remote.Host, s string) string {
	if h.OS() == remote.Windows {
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func quoteFor(h remote.Host, s string) string {
	if h.OS() == remote.Windows {
		return "& " + argFor(h, s)
	}
	return argFor(h, s)
}

func credentialHelper(h remote.Host) string {
	name := "cucina-credential-helper"
	if h.OS() == remote.Windows {
		name += ".exe"
	}
	return hjoin(h, h.WorkDir(), "bin", name)
}

// installCucinactl puts the right cucinactl build on the host (once per
// scenario run), logs in with the campaign service key (transferred over the
// port-forwarding path, never inline in an SSM command), and returns its path.
func installCucinactl(c *harness.Context, svc *infra.Services, h remote.Host) (string, error) {
	if h.Name() == infra.DevMac {
		if err := svc.VerifyLocalProfile(c); err != nil {
			return "", err
		}
		cli, err := svc.CucinactlPath()
		if err != nil {
			return "", err
		}
		r, err := h.Run(c, quoteFor(h, cli)+" credential-helper install --dir "+argFor(h, hjoin(h, h.WorkDir(), "bin")), remote.Opts{})
		if err != nil {
			return "", err
		}
		return cli, r.Err()
	}
	local := c.Env.Cucinactl[h.OS()]
	if local == "" {
		return "", harness.Skip("no cucinactl binary for %s in the environment descriptor", h.OS())
	}
	name := "cucinactl"
	if h.OS() == remote.Windows {
		name += ".exe"
	}
	dst := hjoin(h, h.WorkDir(), "bin", name)
	if err := c.Step(h.Name()+": install cucinactl", func() error {
		if err := h.Put(c, local, dst); err != nil {
			return err
		}
		if h.OS() != remote.Windows {
			res, err := h.Run(c, "chmod 0755 "+quoteFor(h, dst)+" && chmod 0755 "+quoteFor(h, h.WorkDir()+"/bin"), remote.Opts{})
			if err == nil {
				err = res.Err()
			}
			return err
		}
		return nil
	}); err != nil {
		return "", err
	}
	r, err := h.Run(c, quoteFor(h, dst)+" credential-helper install --dir "+argFor(h, hjoin(h, h.WorkDir(), "bin")), remote.Opts{})
	if err != nil {
		return "", err
	}
	if err = r.Err(); err != nil {
		return "", err
	}
	return dst, loginWithKey(c, svc, h, dst)
}

// loginWithKey logs cucinactl in with the campaign's service key. The key
// file travels only through Put's port-forwarding path (bulk threshold 0 is
// forced by putPrivate) and is removed afterwards.
func loginWithKey(c *harness.Context, svc *infra.Services, h remote.Host, cli string) error {
	keyFile := c.Env.Secrets.ServiceKeyFile
	if keyFile == "" {
		return harness.Skip("no service key file in the environment descriptor (secrets.serviceKeyFile)")
	}
	cl := c.Env.Clients[h.Name()]
	sts := cl.STS
	if sts == "" {
		sts = c.Env.Endpoints.STS
	}
	return c.Step(h.Name()+": cucinactl login --key FILE", func() error {
		return infra.LoginClient(c, h, infra.ClientLogin{CLI: cli, STS: sts, KeyFile: keyFile, CAFile: c.Env.Endpoints.CAFile, User: svc.ClientUser(h.Name()), RemoteExecution: cl.RemoteExecution, Management: cl.Management})
	})
}

// prepareAbseil uploads the @cucina_platforms module and prepares the
// host's Abseil checkout with the overlay for the toolchain.
func prepareAbseil(c *harness.Context, svc *infra.Services, h remote.Host, ws string, tc bazelrun.Toolchain, user string) error {
	if h.OS() == remote.Linux && user != "" {
		r, err := h.Run(c, "mkdir -p "+argFor(h, h.WorkDir())+" && chown "+argFor(h, user)+" "+argFor(h, h.WorkDir()), remote.Opts{})
		if err != nil {
			return err
		}
		if err = r.Err(); err != nil {
			return err
		}
	}
	platforms, err := uploadPlatforms(c, svc, h)
	if err != nil {
		return err
	}
	ov, err := abseilOverlay(c.Env, h, tc, platforms)
	if err != nil {
		return err
	}
	return bazelrun.PrepareAbseil(c, h, bazelrun.AbseilPin{Repo: c.Env.Abseil.Repo, Tag: c.Env.Abseil.Tag, Commit: c.Env.Abseil.Commit}, ws, ov, user)
}

// abseilOverlay renders the Abseil overlay for a host: the MODULE.bazel
// addition from tools/xplat/cucina_platforms.MODULE.bazel (generated by the
// cross tooling) with the host's copy of bazel/platforms, and the llvm
// module version pinned in platforms/targets.json.
func abseilOverlay(env *harness.Env, h remote.Host, tc bazelrun.Toolchain, platformsDir string) (bazelrun.Overlay, error) {
	segment, err := os.ReadFile(filepath.Join(env.RepoDir, filepath.FromSlash(bazelrun.ModuleSegment)))
	if err != nil {
		return bazelrun.Overlay{}, err
	}
	var llvm string
	if tc == bazelrun.HermeticLLVM {
		t, err := loadTargets(env)
		if err != nil {
			return bazelrun.Overlay{}, err
		}
		llvm = t.HermeticLLVM.Version
	}
	if h.OS() == remote.Windows {
		platformsDir = strings.ReplaceAll(platformsDir, `\`, "/") // a Starlark string; Bazel accepts C:/…
	}
	mod, err := bazelrun.ModuleOverlay(tc, segment, platformsDir, llvm)
	if err != nil {
		return bazelrun.Overlay{}, err
	}
	return bazelrun.Overlay{Dir: overlayDir(env), Module: mod}, nil
}

// uploadPlatforms copies bazel/platforms (the @cucina_platforms module) to
// the host; the Abseil overlay depends on it through local_path_override.
func uploadPlatforms(c *harness.Context, svc *infra.Services, h remote.Host) (string, error) {
	src := filepath.Join(c.Env.RepoDir, "bazel", "platforms")
	if _, err := os.Stat(filepath.Join(src, "MODULE.bazel")); err != nil {
		return "", fmt.Errorf("%s is not a module yet: %w", src, err)
	}
	dst := hjoin(h, h.WorkDir(), "cucina_platforms")
	if h.Name() == infra.DevMac {
		return src, nil
	}
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		return h.Put(c, p, hjoin(h, dst, strings.ReplaceAll(rel, "/", sep(h))))
	})
	return dst, err
}

// BuildOpts tune one Abseil invocation.
type BuildOpts struct {
	// Context optionally scopes cancellation to this invocation only.
	Context     context.Context
	Command     string   // build | test
	Extra       []string // extra flags
	FreshServer bool
	// ForceExecute bypasses output/action/test caches with a new output base
	// and requires evidence of remote execution (not merely a cache hit).
	ForceExecute bool
	// OutputBase isolates concurrent invocations in one checkout.
	OutputBase string
	// NoRemoteRC runs without cucina.bazelrc (local baseline).
	NoRemoteRC bool
}

var forcedExecutionSequence atomic.Uint64

// bazel runs one Abseil invocation on the lane and records its outcome.
func (lr *laneRun) bazel(name string, o BuildOpts) (*bazelrun.Outcome, error) {
	c, h := lr.c, lr.host
	for _, f := range o.Extra {
		if f == "--noremote_accept_cached" || f == "--remote_accept_cached=false" {
			o.ForceExecute = true
		}
	}
	if o.ForceExecute && o.Command != "clean" {
		o.OutputBase = hjoin(h, h.WorkDir(), "ob", fmt.Sprintf("%s-%d-%d", c.Scenario.ID, c.Result.Started.UnixNano(), forcedExecutionSequence.Add(1)))
		o.Extra = append(append([]string{}, o.Extra...), "--noremote_accept_cached", "--nocache_test_results")
	}
	var startup []string
	if !o.NoRemoteRC {
		startup = append(startup, "--bazelrc="+hjoin(h, lr.ws, "cucina.bazelrc"))
	}
	if o.OutputBase != "" {
		startup = append(startup, "--output_base="+o.OutputBase)
	}
	args := append([]string{}, o.Extra...)
	if o.Command != "clean" {
		args = append(args, "--")
		args = append(args, bazelrun.AbseilTargets...)
	}
	inv := bazelrun.Invocation{
		Name: c.Scenario.ID + "-" + name, Host: h, Workspace: lr.ws, Startup: startup, Command: o.Command, Args: args,
		FreshServer: o.FreshServer, Collect: o.Command != "clean", User: lr.user, Bazel: bazelBinary(c.Env, h),
	}
	var out *bazelrun.Outcome
	err := c.Step(lr.lane.Name+": bazel "+o.Command+" ("+name+")", func() error {
		var err error
		ctx := o.Context
		if ctx == nil {
			ctx = c
		}
		out, err = bazelrun.Run(ctx, inv, filepath.Join(c.Dir(), name))
		return err
	})
	if out != nil {
		recordOutcome(c, lr.lane.Name+"."+name, out)
	}
	if err == nil && o.ForceExecute && o.Command != "clean" {
		err = requireRemoteExecution(out, name)
	}
	return out, err
}

func requireRemoteExecution(o *bazelrun.Outcome, what string) error {
	if o == nil || o.ExecLog == nil || o.ExecLog.RemoteExecutions == 0 {
		return harness.Fail("%s: no remote-execution evidence (missing execution log or zero executed remote spawns)", what)
	}
	return nil
}

// namespace is the release namespace ("cucina" when the descriptor has no
// Kubernetes section).
func namespace(env *harness.Env) string {
	if env.Kubernetes != nil && env.Kubernetes.Namespace != "" {
		return env.Kubernetes.Namespace
	}
	return "cucina"
}

// bazelBinary is the Bazel client to run on a host ("" = bazel/bazel.exe on
// PATH; the dev Mac uses the descriptor's Bazelisk path).
func bazelBinary(env *harness.Env, h remote.Host) string {
	if h.Name() == infra.DevMac && env.DevMac != nil {
		return env.DevMac.Bazel
	}
	return ""
}

// recordOutcome stores an invocation's summaries and headline metrics.
func recordOutcome(c *harness.Context, key string, o *bazelrun.Outcome) {
	c.Record(key, o)
	c.Metric(key+".wall_seconds", o.Wall.Seconds(), "s")
	c.Metric(key+".exit_code", float64(o.ExitCode), "")
	if o.TimeToFirstRemoteAction > 0 {
		c.Metric(key+".time_to_first_remote_action_seconds", o.TimeToFirstRemoteAction.Seconds(), "s")
	}
	if o.PeakServerRSSBytes > 0 {
		c.Metric(key+".bazel_server_peak_rss_bytes", float64(o.PeakServerRSSBytes), "bytes")
	}
	if b := o.BEP; b != nil {
		c.Metric(key+".remote_executions", float64(b.RemoteExecutions()), "")
		c.Metric(key+".remote_cache_hits", float64(b.RemoteCacheHits()), "")
		c.Metric(key+".critical_path_seconds", b.CriticalPath.Seconds(), "s")
		c.Metric(key+".network_bytes_received", float64(b.NetworkBytesReceived), "bytes")
		c.Metric(key+".network_bytes_sent", float64(b.NetworkBytesSent), "bytes")
		if r := b.RetriedTests(); len(r) > 0 {
			c.Note("%s: tests that needed Bazel's retries: %s", key, strings.Join(r, ", "))
		}
	}
	for _, f := range []string{"bep.bin", "exec.log.zst", "profile.json.gz"} {
		if p := filepath.Join(o.LocalDir, f); fileExists(p) {
			_ = c.Artifact(key+"/"+f, p)
		}
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// mustSucceed turns a failed Bazel invocation into a scenario failure.
func mustSucceed(o *bazelrun.Outcome, what string) error {
	if o == nil {
		return harness.Fail("%s: no outcome", what)
	}
	if !o.Succeeded() {
		return harness.Fail("%s: bazel exited %d", what, o.ExitCode)
	}
	return nil
}

// ------------------------------------------------------------------ pools

// PoolStart is one VM start as the management API reports it (StartLatency).
type PoolStart struct {
	VM            string        `json:"vm"`
	Launched      time.Time     `json:"launched"`
	ToRunning     time.Duration `json:"toRunning"`
	ToRegistered  time.Duration `json:"toRegistered"`
	ToFirstAction time.Duration `json:"toFirstAction"`
	Path          string        `json:"path"`
}

// PoolInfo is the subset of `cucinactl pools describe --output json` used.
type PoolInfo struct {
	Raw     map[string]any `json:"-"`
	Workers int            `json:"workers"`
	Starts  []PoolStart    `json:"starts"`
}

// UnmarshalJSON decodes the CLI's pool-describe.v1 contract. Missing pool
// counters/arrays are not a zero-sized fleet. Nullable start latencies stay
// unmeasured until the controller reports them.
func (pi *PoolInfo) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw["schema"] != "pool-describe.v1" {
		return fmt.Errorf("expected pool-describe.v1 response")
	}
	pool, ok := raw["pool"].(map[string]any)
	if !ok {
		return fmt.Errorf("pool-describe.v1 missing pool")
	}
	for _, k := range []string{"desired", "launching", "registered", "busy", "idle", "draining"} {
		if _, ok := pool[k].(float64); !ok {
			return fmt.Errorf("pool-describe.v1 missing numeric pool.%s", k)
		}
	}
	workers, ok := raw["workers"].([]any)
	if !ok {
		return fmt.Errorf("pool-describe.v1 missing workers array")
	}
	starts, ok := raw["starts"].([]any)
	if !ok {
		return fmt.Errorf("pool-describe.v1 missing starts array")
	}
	*pi = PoolInfo{Raw: raw, Workers: len(workers)}
	for _, s := range starts {
		m, ok := s.(map[string]any)
		if !ok {
			return fmt.Errorf("pool-describe.v1 malformed start")
		}
		ps := PoolStart{VM: str(m["vm"]), Path: str(m["path"]), ToRunning: dur(m["to_running_seconds"]), ToRegistered: dur(m["to_registered_seconds"]), ToFirstAction: dur(m["to_first_action_seconds"])}
		if m["launched"] != nil {
			var err error
			ps.Launched, err = time.Parse(time.RFC3339Nano, str(m["launched"]))
			if err != nil {
				return fmt.Errorf("invalid launch timestamp: %w", err)
			}
		}
		pi.Starts = append(pi.Starts, ps)
	}
	return nil
}

func describePool(c *harness.Context, svc *infra.Services, pool string) (PoolInfo, error) {
	var pi PoolInfo
	err := svc.CucinactlJSON(c, &pi, "pools", "describe", pool)
	return pi, err
}

// field looks a key up as given, in camelCase and in CLI pool/proto summary.
func field(m map[string]any, key string) any {
	if m == nil {
		return nil
	}
	if v, ok := m[key]; ok {
		return v
	}
	if v, ok := m[camel(key)]; ok {
		return v
	}
	for _, container := range []string{"pool", "summary"} {
		if s, ok := m[container].(map[string]any); ok {
			if v := field(s, key); v != nil {
				return v
			}
		}
	}
	return nil
}

func camel(s string) string {
	parts := strings.Split(s, "_")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func dur(v any) time.Duration {
	switch x := v.(type) {
	case string:
		if d, err := time.ParseDuration(x); err == nil {
			return d
		}
		if f, err := strconv.ParseFloat(strings.TrimSuffix(x, "s"), 64); err == nil {
			return time.Duration(f * float64(time.Second))
		}
	case float64:
		return time.Duration(x * float64(time.Second))
	case map[string]any:
		sec, _ := x["seconds"].(float64)
		ns, _ := x["nanos"].(float64)
		return time.Duration(sec)*time.Second + time.Duration(ns)
	}
	return 0
}

// coldStarts returns the cold-start samples of VMs launched in [from, to]:
// the controller's launch → first action, plus, for the first VM, the delay
// between the client submitting its first action and the launch (the
// controller's reaction), so the sample spans "Execute submitted with the
// pool at zero → first action executing" (NFR-P1).
func coldStarts(starts []PoolStart, from, to, firstSubmit time.Time) []time.Duration {
	var out []time.Duration
	var first time.Time
	for _, s := range starts {
		if s.Launched.Before(from) || s.Launched.After(to) {
			continue
		}
		if s.ToFirstAction <= 0 {
			return nil
		} // partial launch samples cannot prove a maximum
		if first.IsZero() || s.Launched.Before(first) {
			first = s.Launched
		}
	}
	for _, s := range starts {
		if s.Launched.Before(from) || s.Launched.After(to) || s.ToFirstAction <= 0 {
			continue
		}
		d := s.ToFirstAction
		if s.Launched.Equal(first) && !firstSubmit.IsZero() && s.Launched.After(firstSubmit) {
			d += s.Launched.Sub(firstSubmit)
		}
		out = append(out, d)
	}
	return out
}

// ------------------------------------------------------------------ AWS

// sampled runs fn while sampling the tag-filtered EC2 inventory, then records
// the integrated usage, its cost (internal/cost via spend) and the instance
// timeline. Without the aws capability fn just runs.
func sampled(c *harness.Context, every time.Duration, fn func() error) (awsinv.UsageSummary, error) {
	svc, err := infra.Of(c)
	if err != nil {
		return awsinv.UsageSummary{}, err
	}
	if svc.EC2 == nil {
		if c.Env.Has(harness.RequiresAWS) {
			return awsinv.UsageSummary{}, fmt.Errorf("AWS inventory unavailable")
		}
		return awsinv.UsageSummary{}, fn()
	}
	sctx, cancel := context.WithCancel(c)
	smp := svc.EC2.StartSampler(sctx, every)
	runErr := fn()
	snaps, sampleErrs := smp.Stop()
	cancel()
	u := awsinv.Integrate(snaps)
	if sampleErrs > 0 {
		c.Check(harness.CheckResult{Name: "AWS lifecycle sampling complete", Kind: "aws", Pass: false, Detail: fmt.Sprintf("%d describe errors; lifecycle/cost evidence incomplete", sampleErrs)})
	}
	c.Record("aws", u)
	for k, v := range u.InstanceSeconds {
		c.Metric("aws.instance_seconds."+k, v, "s")
	}
	c.Metric("aws.max_instances", float64(u.MaxInstances), "")
	c.Metric("aws.ebs_gib_hours", u.VolumeGiBHours, "GiB-h")
	if c.Result.Cost.InstanceSeconds == nil {
		c.Result.Cost.InstanceSeconds = map[string]float64{}
	}
	for k, v := range u.InstanceSeconds {
		c.Result.Cost.InstanceSeconds[k] += v
	}
	c.Result.Cost.EBSGBHours += u.VolumeGiBHours
	bill := spend.Price(cost.Usage{Launches: spend.Launches(u.Lifecycles)}, c.Now())
	if len(bill.Unpriced) > 0 {
		c.Result.Cost.Unpriced = append(c.Result.Cost.Unpriced, bill.Unpriced...)
		c.Check(harness.CheckResult{Name: "complete instance pricing", Kind: "cost", Detail: fmt.Sprintf("no verified price for %v; partial cost is not total spend", bill.Unpriced)})
	}
	for _, l := range bill.Lines {
		c.AddCost(l.Category+":"+l.Detail, l.Amount.USD())
	}
	for _, l := range u.Lifecycles {
		c.Event(l.ID, "launched ("+l.Pool+", "+l.Type+")", l.Launched)
		if !l.Running.IsZero() {
			c.Event(l.ID, "running", l.Running)
		}
		if !l.Gone.IsZero() {
			c.Event(l.ID, "terminated (gone)", l.Gone)
		}
	}
	return u, runErr
}

// residue describes what is left of the campaign's worker resources.
func residue(c *harness.Context) (awsinv.Residue, error) {
	svc, err := infra.Of(c)
	if err != nil {
		return awsinv.Residue{}, err
	}
	if svc.EC2 == nil {
		return awsinv.Residue{}, harness.Skip("no aws capability")
	}
	s, err := svc.EC2.Describe(c)
	if err != nil {
		return awsinv.Residue{}, err
	}
	return s.Residue(), nil
}

// waitZero polls until no worker resources remain (or the deadline passes)
// and returns how long it took.
func waitZero(c *harness.Context, limit time.Duration) (time.Duration, awsinv.Residue, error) {
	start := c.Now()
	deadline := start.Add(limit)
	for {
		r, err := residue(c)
		if err != nil {
			return 0, r, err
		}
		if r.Zero() {
			return c.Now().Sub(start), r, nil
		}
		if c.Now().After(deadline) {
			return c.Now().Sub(start), r, nil
		}
		if err := remote.RealSleep(c, 15*time.Second); err != nil {
			return 0, r, err
		}
	}
}

// ------------------------------------------------------------------ checks

// PromCheck evaluates an slo.Threshold as a post-condition.
func PromCheck(name string, th slo.Threshold) harness.Check {
	return harness.CheckFunc{Name: name, Kind: "promql", Fn: func(ctx context.Context, c *harness.Context) harness.CheckResult {
		r := harness.CheckResult{Query: th.String()}
		svc, err := infra.Of(c)
		if err != nil {
			r.Skipped = err.Error()
			return r
		}
		p, err := svc.Prom(c)
		if err != nil {
			r.Skipped = err.Error()
			return r
		}
		up, ok, err := p.Scalar(ctx, `max(up{job=~".*controller.*"})`, time.Time{})
		if err != nil || !ok || up != 1 {
			r.Detail = fmt.Sprintf("controller scrape unavailable (present=%v, up=%g, error=%v)", ok, up, err)
			return r
		}
		v, ok, err := p.Scalar(ctx, th.Query, time.Time{})
		switch {
		case err != nil:
			r.Detail = "query failed: " + err.Error()
		case !ok:
			r.Detail = "required Prometheus query returned no data"
		default:
			r.Value = strconv.FormatFloat(v, 'g', 6, 64)
			r.Pass = th.Holds(v)
			if !r.Pass {
				r.Detail = fmt.Sprintf("%g violates %s %g", v, th.Op, th.Value)
			}
		}
		return r
	}}
}

// ZeroResidueCheck asserts NFR-C1's zero-scale condition with tag-filtered
// describes.
func ZeroResidueCheck(name string) harness.Check {
	return harness.CheckFunc{Name: name, Kind: "aws", Fn: func(_ context.Context, c *harness.Context) harness.CheckResult {
		r, err := residue(c)
		if err != nil {
			return harness.CheckResult{Detail: "inventory unavailable: " + err.Error()}
		}
		return harness.CheckResult{Query: "tag-filtered describe-instances/volumes/network-interfaces/addresses", Value: r.String(), Pass: r.Zero(), Detail: r.String()}
	}}
}

// Guards are the production guards every scenario re-checks afterwards
// (R-TEST-7): no invariant violations, no cost leak.
var Guards = []harness.Check{
	PromCheck("no invariant violations", slo.NoInvariantViolations),
	PromCheck("no idle instances with an empty queue", slo.NoCostLeak),
}

// promMax returns the maximum of a query over [from, to].
func promMax(c *harness.Context, q string, from, to time.Time) (float64, bool) {
	svc, err := infra.Of(c)
	if err != nil {
		c.Check(harness.CheckResult{Name: "required metrics source", Kind: "promql", Query: q, Detail: err.Error()})
		return 0, false
	}
	p, err := svc.Prom(c)
	if err != nil {
		c.Check(harness.CheckResult{Name: "required metrics source", Kind: "promql", Query: q, Detail: err.Error()})
		return 0, false
	}
	step := max(15*time.Second, to.Sub(from)/200)
	ss, err := p.Range(c, q, from, to, step)
	if err != nil {
		c.Check(harness.CheckResult{Name: "required metrics query", Kind: "promql", Query: q, Pass: false, Detail: err.Error()})
		return 0, false
	}
	v, ok := promMaxOf(ss)
	if !ok {
		c.Check(harness.CheckResult{Name: "required metrics samples", Kind: "promql", Query: q, Pass: false, Detail: "no data"})
	}
	return v, ok
}

func promScalar(c *harness.Context, q string, at time.Time) (float64, bool) {
	svc, err := infra.Of(c)
	if err != nil {
		c.Check(harness.CheckResult{Name: "required metrics source", Kind: "promql", Query: q, Detail: err.Error()})
		return 0, false
	}
	p, err := svc.Prom(c)
	if err != nil {
		c.Check(harness.CheckResult{Name: "required metrics source", Kind: "promql", Query: q, Detail: err.Error()})
		return 0, false
	}
	v, ok, err := p.Scalar(c, q, at)
	if err != nil {
		c.Check(harness.CheckResult{Name: "required metrics query", Kind: "promql", Query: q, Pass: false, Detail: err.Error()})
		return 0, false
	}
	if !ok {
		c.Check(harness.CheckResult{Name: "required metrics samples", Kind: "promql", Query: q, Pass: false, Detail: "no data"})
	}
	return v, ok
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func promMaxOf(ss []prom.Series) (float64, bool) { return prom.MaxOverRange(ss) }

// poolInventory narrows the EC2 inventory to one pool.
func poolInventory(svc *infra.Services, pool string) *awsinv.Inventory {
	inv := *svc.EC2
	inv.Tags = maps.Clone(svc.EC2.Tags)
	inv.Tags["cucina:pool"] = pool
	return &inv
}

// materializedBytes sums the files Bazel wrote into the output tree on the
// client: with Build without the Bytes these are the outputs actually
// downloaded (plus small local files), the numerator of NFR-T1.
func (lr *laneRun) materializedBytes() (int64, error) {
	h := lr.host
	var script string
	if h.OS() == remote.Windows {
		script = fmt.Sprintf(`$ErrorActionPreference = 'Continue'
Set-Location %s
$p = (& bazel.exe --bazelrc=cucina.bazelrc info output_path 2>$null | Select-Object -Last 1)
(Get-ChildItem -Recurse -File -Force $p -ErrorAction SilentlyContinue | Measure-Object -Sum Length).Sum`, "'"+lr.ws+"'")
	} else {
		script = fmt.Sprintf(`cd %s && p=$(bazel --bazelrc=cucina.bazelrc info output_path 2>/dev/null | tail -n 1) && find "$p" -type f -print0 2>/dev/null | xargs -0 cat 2>/dev/null | wc -c`, quoteFor(h, lr.ws))
	}
	res, err := h.Run(lr.c, script, remote.Opts{User: lr.user})
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(lastLine(string(res.Stdout))), 10, 64)
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// aquerySummary runs `bazel aquery --output=summary` for the Abseil targets
// (§10.2 "count actions") and returns the action count per mnemonic.
func (lr *laneRun) aquerySummary() (map[string]int, string, error) {
	h := lr.host
	var script string
	if h.OS() == remote.Windows {
		script = fmt.Sprintf(`$ErrorActionPreference = 'Continue'
Set-Location '%s'
& bazel.exe --bazelrc=cucina.bazelrc aquery --output=summary //absl/... 2>$null`, lr.ws)
	} else {
		bz := "bazel"
		if b := bazelBinary(lr.c.Env, h); b != "" {
			bz = b
		}
		script = fmt.Sprintf("cd %s && %s --bazelrc=cucina.bazelrc aquery --output=summary //absl/... 2>/dev/null", quoteFor(h, lr.ws), quoteFor(h, bz))
	}
	res, err := h.Run(lr.c, script, remote.Opts{User: lr.user, Timeout: 30 * time.Minute})
	if err != nil {
		return nil, "", err
	}
	out := string(res.Stdout)
	return parseAquerySummary(out), out, res.Err()
}

// parseAquerySummary reads Bazel 9's `aquery --output=summary`: the
// "N total actions." line and the indented "Mnemonics:" section.
func parseAquerySummary(out string) map[string]int {
	counts := map[string]int{}
	section := ""
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if n, ok := strings.CutSuffix(t, " total actions."); ok {
			if v, err := strconv.Atoi(n); err == nil {
				counts["total"] = v
			}
			continue
		}
		if strings.HasSuffix(t, ":") && !strings.HasPrefix(line, " ") {
			section = strings.TrimSuffix(t, ":")
			continue
		}
		if section != "Mnemonics" || !strings.HasPrefix(line, " ") {
			continue
		}
		k, v, ok := strings.Cut(t, ":")
		if n, err := strconv.Atoi(strings.TrimSpace(v)); ok && err == nil {
			counts[k] = n
		}
	}
	return counts
}

// promSnapshot records the control-plane and worker view of [from, to]
// (§10.4): queue depth/time per platform, workers by state, worker staging
// overhead, AC hit ratio, blob bytes per tier, pod and worker memory/CPU,
// storage retention. Every query lands in the result (Prometheus recorder).
func promSnapshot(c *harness.Context, lane string, from, to time.Time) {
	sel := workerSelector(c, lane)
	ns := namespace(c.Env)
	queries := map[string]string{
		"queue_depth_max":         "max(" + slo.QueueDepth + ")",
		"queue_time_p95_max_s":    "max(" + slo.QueueTimeP95 + ")",
		"workers_busy_max":        `sum(` + slo.WorkersByState + `{state="busy"})`,
		"fetch_inputs_p50_s":      slo.FetchInputsP50,
		"upload_outputs_p50_s":    slo.UploadOutputsP50,
		"ac_hit_ratio":            slo.ACHitRatio,
		"cas_get_bytes_per_s":     `sum(` + slo.BlobBytesRate + `{storage_type="CAS",operation="Get"})`,
		"cas_put_bytes_per_s":     `sum(` + slo.BlobBytesRate + `{storage_type="CAS",operation="Put"})`,
		"control_plane_rss_bytes": fmt.Sprintf(`%s{namespace=%q}`, slo.ControlPlaneRSS, ns),
		"control_plane_cpu_cores": fmt.Sprintf(`sum(rate(container_cpu_usage_seconds_total{namespace=%q,container!=""}[5m]))`, ns),
		"worker_rss_bytes_max":    fmt.Sprintf(`max(process_resident_memory_bytes{%s})`, sel),
		"worker_cpu_cores_max":    fmt.Sprintf(`max(rate(process_cpu_seconds_total{%s}[5m]))`, sel),
		"cas_retention_s_min":     "min(" + slo.CASRetention + ")",
	}
	keys := make([]string, 0, len(queries))
	for k := range queries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v, ok := promMax(c, queries[k], from, to); ok {
			c.Metric("prom."+lane+"."+k, v, "")
		}
	}
	w := int(to.Sub(from).Seconds())
	for name, op := range map[string]string{"frontend_cas_get_bytes": "Get", "frontend_cas_put_bytes": "Put"} {
		q := fmt.Sprintf(`sum(increase(buildbarn_blobstore_blob_access_operations_blob_size_bytes_sum{job=~".*frontend.*",storage_type="CAS",operation=%q}[%ds]))`, op, w)
		if v, ok := promScalar(c, q, to); ok {
			c.Metric("prom."+lane+"."+name, v, "bytes")
		}
	}
}
