// SPDX-License-Identifier: FSL-1.1-ALv2

package scenarios

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/test/e2e/harness"
	"github.com/sloper-ai/cucina/test/e2e/infra"
)

// CLIExtendedOptions supplies the non-destructive inputs to the extended T20
// checks. Mutating fleet fixtures come only from Env.CLI, never from a discovered
// campaign worker/pool/host. ServiceAccount is the verified campaign writer's
// account (not its key); a NEW short-lived key is created for disposable profiles.
// PrivateRoot defaults to ~/.config/cucina and must be on the encrypted disk in
// acceptance. Tests supply their own temporary directory.
type CLIExtendedOptions struct {
	WorkerNode     string
	ServiceAccount string
	PrivateRoot    string
}

// CLIExtendedFixturePrefix names fixtures an operator provisions for this run.
// Pool: <prefix>-pool, max=0, min=0, instanceNames=[<prefix>], no workers.
// Host: <prefix>-host with labels cucina.sloper.ai/test-run=<runID> and
// cucina.sloper.ai/test-fixture=t20; every VM stopped, target <prefix>-vm.
// Operations: <prefix>-invocation; only the explicitly named operation is killed.
// Fixtures are not created/deleted by the checks (except enrollment records and
// credentials created by this invocation). In particular, no active campaign
// pool, host VM, bootstrap key or campaign key is deleted or revoked.
func CLIExtendedFixturePrefix(runID string) string {
	h := sha256.Sum256([]byte(runID))
	return "t20-" + hex.EncodeToString(h[:6])
}

func runT20Extended(c *harness.Context, svc *infra.Services, _ *laneRun, node, _ string) error {
	o := CLIExtendedOptions{WorkerNode: node}
	if control, err := campaignToken(c); err == nil {
		if account, ok := strings.CutPrefix(control.Subject, "sa:"); ok {
			o.ServiceAccount = account
		}
	}
	return RunCLIExtended(c, svc, o)
}

// These are command branches, not a claim that every option combination was run.
// In particular floor-clear does not prove positive-floor provisioning, and the
// named-operation kill does not prove queue-wide kill (which is unsafe on shared
// campaign queues). Other T20/T10/cross checks supply the remaining branches.
var cliExtendedBranches = []string{
	"pools scale-floor --min 0", "pools cordon", "pools cordon --undo", "pools gc --dry-run", "pools gc",
	"workers logs", "hosts drain", "hosts uncordon", "hosts re-image", "hosts diag",
	"hosts register", "hosts approve", "hosts remove", "hosts enroll-token create", "hosts enroll-token revoke",
	"ops watch", "ops kill", "config path", "config profiles", "config use", "config set", "config delete",
	"diag", "logout", "logout --all --forget", "credential-helper install",
}

const cliCommandLimit = 45 * time.Second
const cliOutputLimit = 4 << 20

// RunCLIExtended exercises public CLI commands and records observed outcomes, not
// planned coverage. It is also the boundary used by the deterministic Exec fake.
// Missing safe fixtures yield SKIP; any failed assertion, process failure, output
// limit, timeout or cleanup failure yields FAIL even when other checks were skipped.
func RunCLIExtended(c *harness.Context, svc *infra.Services, o CLIExtendedOptions) error {
	ctx, cancel := context.WithTimeout(c, 10*time.Minute)
	defer cancel()
	r := &cliExtended{ctx: ctx, c: c, svc: svc, opts: o, prefix: CLIExtendedFixturePrefix(c.Env.RunID), checks: map[string]harness.CheckResult{}}
	// A nonce distinguishes simultaneous invocations using the same campaign run
	// ID. It is a public ownership marker, not an authentication secret.
	r.marker = r.prefix + "-" + strings.ToLower(rand.Text()[:12])
	for _, name := range cliExtendedBranches {
		r.checks[name] = harness.CheckResult{Name: "cucinactl " + name, Kind: "cli", Skipped: "required fixture or earlier step unavailable"}
	}
	if c.Env.RunID == "" {
		r.gap(cliExtendedBranches, "run ID is required to isolate T20 fixtures")
		return r.finish()
	}
	var err error
	r.bin, err = svc.CucinactlPath()
	if err != nil {
		r.gap(cliExtendedBranches, "cucinactl binary unavailable")
		return r.finish()
	}
	r.schemas = filepath.Join(c.Env.RepoDir, "cli", "cucinactl", "schemas")
	if p := os.Getenv("CUCINA_CLI_RESULT_SCHEMA"); p != "" {
		r.schemas = filepath.Dir(p)
	}
	r.pool()
	r.workerLogs()
	if c.Env.Safety.AllowDestructive {
		r.enrollment()
		r.operations()
	} else {
		r.gap([]string{"hosts register", "hosts approve", "hosts remove", "hosts enroll-token create", "hosts enroll-token revoke", "ops watch", "ops kill"}, "destructive fixture checks not enabled")
	}
	root := o.PrivateRoot
	if root == "" {
		home, e := os.UserHomeDir()
		if e == nil {
			root = filepath.Join(home, ".config", "cucina")
		}
	}
	// Bundle contents and CLI token caches never enter the public/raw-artifact
	// directory. Remove only the randomly named directory created here.
	if root == "" || os.MkdirAll(root, 0o700) != nil {
		r.fail("diag", "cannot create private diagnostics/config directory")
	} else if r.private, err = os.MkdirTemp(root, "t20-cli-"); err != nil {
		r.fail("diag", "cannot create private diagnostics/config directory")
	} else {
		r.supportBundle()
		r.hostMaintenance()
		r.profiles()
		if err := os.RemoveAll(r.private); err != nil {
			r.fail("private files removed", "private fixture directory cleanup failed")
		}
	}
	return r.finish()
}

type cliExtended struct {
	ctx     context.Context
	c       *harness.Context
	svc     *infra.Services
	opts    CLIExtendedOptions
	bin     string
	prefix  string
	marker  string
	schemas string
	private string
	checks  map[string]harness.CheckResult
}

func (r *cliExtended) gap(names []string, why string) {
	for _, n := range names {
		x := r.checks[n]
		if !x.Pass && x.Detail == "" {
			x.Skipped = why
			r.checks[n] = x
		}
	}
}

func (r *cliExtended) fail(name, detail string) {
	r.checks[name] = harness.CheckResult{Name: "cucinactl " + name, Kind: "cli", Detail: detail}
}

func (r *cliExtended) check(name string, fn func() error) bool {
	if err := fn(); err != nil {
		r.fail(name, err.Error()) // all errors here are fixed, content-free diagnostics
		return false
	}
	if old, ok := r.checks[name]; !ok || old.Pass || old.Skipped != "" {
		r.checks[name] = harness.CheckResult{Name: "cucinactl " + name, Kind: "cli", Pass: true, Detail: "command and observable postcondition verified"}
	}
	return true
}

func (r *cliExtended) cleanup(name string, fn func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), cliCommandLimit)
	defer cancel()
	r.check("cleanup "+name, func() error { return fn(ctx) })
}

func (r *cliExtended) finish() error {
	keys := make([]string, 0, len(r.checks))
	for k := range r.checks {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	passed, failed, skipped := 0, 0, 0
	for _, k := range keys {
		v := r.checks[k]
		r.c.Check(v)
		switch {
		case v.Skipped != "":
			skipped++
		case v.Pass:
			passed++
		default:
			failed++
		}
	}
	r.c.Record("cliExtendedCoverage", map[string]int{"passed": passed, "failed": failed, "unavailable": skipped})
	r.c.Note("Extended T20 uses disposable fixtures; floor-clear is not positive-floor provisioning, named kill is not queue-wide kill. OIDC login/bazelrc variants and manual TUI feel have separate evidence.")
	if failed > 0 {
		return harness.Fail("extended T20: %d failed checks, %d unavailable (see individual results)", failed, skipped)
	}
	if skipped > 0 {
		return harness.Skip("extended T20 incomplete: %d command branches unavailable; no complete-command PASS", skipped)
	}
	return nil
}

// boundedCLIOutput limits memory as well as the returned output. Neither stdout,
// stderr nor stdin is ever included in error messages or the public report.
type boundedCLIOutput struct{ bytes.Buffer }

func (w *boundedCLIOutput) Write(p []byte) (int, error) {
	if len(p) > cliOutputLimit-w.Len() {
		return 0, errors.New("CLI output limit exceeded")
	}
	return w.Buffer.Write(p)
}

func (r *cliExtended) command(parent context.Context, env []string, stdin []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, cliCommandLimit)
	defer cancel()
	args = append(slices.Clone(args), "--output", "json", "--timeout", "30s")
	if r.svc.Exec != nil {
		res, err := r.svc.Exec.Run(ctx, ports.Command{Path: r.bin, Args: args, Env: env, Stdin: stdin})
		if ctx.Err() != nil {
			return nil, errors.New("CLI command canceled or timed out")
		}
		if err != nil {
			return nil, errors.New("CLI process execution failed; output withheld")
		}
		if res.ExitCode != 0 {
			return nil, fmt.Errorf("CLI exited %d; output withheld", res.ExitCode)
		}
		if len(res.Stdout) > cliOutputLimit {
			return nil, errors.New("CLI output limit exceeded")
		}
		return res.Stdout, nil
	}
	cmd := exec.CommandContext(ctx, r.bin, args...)
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = os.Environ()
	if len(env) > 0 {
		// An empty CUCINA_PROFILE is still a clap argument. Truly remove the
		// ambient selector and replaced settings from isolated local commands.
		cmd.Env = slices.DeleteFunc(cmd.Env, func(e string) bool {
			k, _, _ := strings.Cut(e, "=")
			return k == "CUCINA_PROFILE" || slices.ContainsFunc(env, func(v string) bool { return strings.HasPrefix(v, k+"=") })
		})
	}
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdin = bytes.NewReader(stdin)
	var out boundedCLIOutput
	cmd.Stdout, cmd.Stderr = &out, io.Discard
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, errors.New("CLI command canceled or timed out")
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("CLI exited %d; output withheld", exit.ExitCode())
		}
		return nil, errors.New("CLI execution or output limit failed; output withheld")
	}
	return out.Bytes(), nil
}

// contract checks the versioned envelope and top-level required fields against the
// CLI's checked-in PUBLIC schema. This is deliberately not a JSON Schema engine;
// typed decoding and the per-command state assertions below check semantics.
func (r *cliExtended) contract(raw []byte, schema string, dst any) error {
	b, err := os.ReadFile(filepath.Join(r.schemas, schema+".v1.schema.json"))
	if err != nil {
		return errors.New("public CLI JSON schema unavailable")
	}
	var rule struct {
		Required   []string `json:"required"`
		Properties map[string]struct {
			Const string `json:"const"`
			Type  any    `json:"type"`
			Ref   string `json:"$ref"`
		} `json:"properties"`
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(b, &rule) != nil || len(rule.Required) == 0 || rule.Properties["schema"].Const != schema+".v1" {
		return errors.New("invalid public CLI schema")
	}
	if json.Unmarshal(raw, &doc) != nil || doc == nil {
		return errors.New("CLI response is not a JSON object")
	}
	var version string
	if json.Unmarshal(doc["schema"], &version) != nil || version != rule.Properties["schema"].Const {
		return errors.New("CLI response uses the wrong schema")
	}
	for _, field := range rule.Required {
		if _, ok := doc[field]; !ok {
			return errors.New("CLI response omits a required public field")
		}
		if bytes.Equal(doc[field], []byte("null")) {
			p := rule.Properties[field]
			allowsNull := p.Type == nil && p.Ref == ""
			switch typ := p.Type.(type) {
			case string:
				allowsNull = typ == "null"
			case []any:
				allowsNull = slices.Contains(typ, any("null"))
			}
			if !allowsNull {
				return errors.New("CLI response has null in a non-nullable field")
			}
		}
	}
	if dst != nil && json.Unmarshal(raw, dst) != nil {
		return errors.New("CLI response has invalid field types")
	}
	return nil
}

func (r *cliExtended) json(ctx context.Context, schema string, dst any, args ...string) error {
	b, err := r.command(ctx, nil, nil, args...)
	if err != nil {
		return err
	}
	return r.contract(b, schema, dst)
}

func (r *cliExtended) ack(ctx context.Context, env []string, action, target string, args ...string) error {
	var out struct {
		OK             bool `json:"ok"`
		Action, Target string
	}
	b, err := r.command(ctx, env, nil, args...)
	if err != nil {
		return err
	}
	if err := r.contract(b, "result", &out); err != nil {
		return err
	}
	if !out.OK || out.Action != action || out.Target != target {
		return errors.New("CLI acknowledgement did not confirm the requested operation")
	}
	return nil
}

type cliFixturePool struct {
	Pool struct {
		Name, Provider                                                     string
		MinRunning                                                         int `json:"min_running"`
		Max, Desired, Launching, Registered, Busy, Idle, Draining, Stopped int
		Paused                                                             bool
	} `json:"pool"`
	Spec struct {
		InstanceNames []string `json:"instanceNames"`
		Capacity      struct {
			MinRunning int `json:"minRunning"`
			Max        int
		} `json:"capacity"`
		Tart *struct {
			HostSelector map[string]string `json:"hostSelector"`
		} `json:"tart"`
	} `json:"spec"`
	Workers []json.RawMessage `json:"workers"`
}

// Safety-critical reads must distinguish an explicitly disabled/idle resource
// from a missing/null field. Ordinary Go zero-value decoding cannot do that.
func (p *cliFixturePool) UnmarshalJSON(b []byte) error {
	type plain cliFixturePool
	var value plain
	if err := json.Unmarshal(b, &value); err != nil {
		return err
	}
	var raw struct {
		Pool map[string]json.RawMessage
		Spec struct{ Capacity map[string]json.RawMessage }
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	for _, key := range []string{"max", "min_running", "paused", "desired", "launching", "registered", "busy", "idle", "draining", "stopped"} {
		if len(raw.Pool[key]) == 0 || bytes.Equal(raw.Pool[key], []byte("null")) {
			return errors.New("incomplete pool safety snapshot")
		}
	}
	for _, key := range []string{"max", "minRunning"} {
		if len(raw.Spec.Capacity[key]) == 0 || bytes.Equal(raw.Spec.Capacity[key], []byte("null")) {
			return errors.New("incomplete pool capacity spec")
		}
	}
	*p = cliFixturePool(value)
	return nil
}

// observe permits informer lag, but never treats a timeout or an unavailable
// source as evidence. Every retry uses the caller's bounded process context.
func (r *cliExtended) observe(parent context.Context, read func(context.Context) (bool, error)) error {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	for {
		ok, err := read(ctx)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		t := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			t.Stop()
			return errors.New("CLI postcondition was not observed before its deadline")
		case <-t.C:
		}
	}
}

func (r *cliExtended) pool() {
	branches := cliExtendedBranches[:5]
	f := r.c.Env.CLI
	if !r.c.Env.Safety.AllowDestructive || f == nil || f.Pool == "" {
		r.gap(branches, "dedicated disabled T20 pool not declared or destructive checks disabled")
		return
	}
	if f.Pool != r.prefix+"-pool" {
		r.gap(branches, "pool is not run-scoped; refused to touch it")
		return
	}
	for _, p := range r.c.Env.Pools {
		if p == f.Pool {
			r.gap(branches, "fixture aliases an active campaign pool; refused to touch it")
			return
		}
	}
	var before cliFixturePool
	if err := r.json(r.ctx, "pool-describe", &before, "pools", "describe", f.Pool); err != nil {
		r.fail(branches[0], err.Error())
		return
	}
	p := before.Pool
	if p.Name != f.Pool || p.Provider != "ec2" || p.Max != 0 || p.MinRunning != 0 ||
		p.Desired != 0 || p.Launching != 0 || p.Registered != 0 || p.Busy != 0 || p.Idle != 0 || p.Draining != 0 || p.Stopped != 0 ||
		len(before.Workers) != 0 || before.Spec.Capacity.Max != 0 || before.Spec.Capacity.MinRunning != 0 ||
		!slices.Equal(before.Spec.InstanceNames, []string{r.prefix}) {
		r.gap(branches, "fixture pool must be disabled, empty, EC2 and use only its isolated T20 instance name")
		return
	}
	defer r.cleanup("pool cordon restored", func(ctx context.Context) error {
		args, action := []string{"pools", "cordon", f.Pool, "--yes"}, "cordon pool"
		if !p.Paused {
			args, action = append(args, "--undo"), "uncordon pool"
		}
		if err := r.ack(ctx, nil, action, f.Pool, args...); err != nil {
			return err
		}
		return r.observe(ctx, func(ctx context.Context) (bool, error) {
			var got cliFixturePool
			if err := r.json(ctx, "pool-describe", &got, "pools", "describe", f.Pool); err != nil {
				return false, err
			}
			return got.Pool.Paused == p.Paused && got.Pool.Max == 0, nil
		})
	})
	if !r.check(branches[0], func() error {
		var out struct {
			Pool       string
			MinRunning int     `json:"min_running"`
			ExpiresAt  *string `json:"expires_at"`
		}
		if err := r.json(r.ctx, "pool-floor", &out, "pools", "scale-floor", f.Pool, "--min", "0"); err != nil {
			return err
		}
		if out.Pool != f.Pool || out.MinRunning != 0 || out.ExpiresAt != nil {
			return errors.New("floor-clear response did not clear the floor")
		}
		return nil
	}) {
		return
	}
	for _, c := range []struct {
		name, action string
		paused       bool
	}{{branches[1], "cordon pool", true}, {branches[2], "uncordon pool", false}} {
		if !r.check(c.name, func() error {
			args := []string{"pools", "cordon", f.Pool, "--yes"}
			if !c.paused {
				args = append(args, "--undo")
			}
			if err := r.ack(r.ctx, nil, c.action, f.Pool, args...); err != nil {
				return err
			}
			return r.observe(r.ctx, func(ctx context.Context) (bool, error) {
				var got cliFixturePool
				if err := r.json(ctx, "pool-describe", &got, "pools", "describe", f.Pool); err != nil {
					return false, err
				}
				return got.Pool.Paused == c.paused && got.Pool.Max == 0, nil
			})
		}) {
			return
		}
	}
	for _, dry := range []bool{true, false} {
		name := "pools gc"
		args := []string{"pools", "gc", f.Pool, "--yes"}
		if dry {
			name += " --dry-run"
			args = append(args, "--dry-run")
		}
		if !r.check(name, func() error {
			var out struct {
				Pool            string
				DryRun          bool `json:"dry_run"`
				Deleted, Errors []string
			}
			if err := r.json(r.ctx, "pool-gc", &out, args...); err != nil {
				return err
			}
			// A never-enabled fixture cannot legitimately own resources. Do not turn
			// an unexpectedly nonempty dry run into a destructive deletion.
			if out.Pool != f.Pool || out.DryRun != dry || len(out.Errors) != 0 || len(out.Deleted) != 0 {
				return errors.New("disabled fixture GC was not empty and error-free; deletion refused")
			}
			return nil
		}) {
			return
		}
	}
}

func (r *cliExtended) workerLogs() {
	if r.opts.WorkerNode == "" {
		r.gap([]string{"workers logs"}, "no worker node observed by T20")
		return
	}
	r.check("workers logs", func() error {
		b, err := r.command(r.ctx, nil, nil, "workers", "logs", r.opts.WorkerNode, "--unit", "bb-worker", "--tail", "20")
		if err != nil {
			return err
		}
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(make([]byte, 4096), cliOutputLimit)
		seen := false
		for sc.Scan() {
			var out struct {
				Text string `json:"text"`
			}
			if err := r.contract(sc.Bytes(), "log", &out); err != nil {
				return err
			}
			seen = seen || strings.TrimSpace(out.Text) != ""
		}
		if sc.Err() != nil || !seen {
			return errors.New("worker log stream yielded no nonempty valid log chunk")
		}
		return nil
	})
}

type cliFixtureHost struct {
	Serial, Name, Site, Phase string
	Cordoned, Approved        bool
	RunningVMs                int `json:"running_vms"`
	Labels                    map[string]string
	VMs                       []struct {
		Name, Pool, State, Image, Generation string
		Registered                           bool
	} `json:"vms"`
}

func (h *cliFixtureHost) UnmarshalJSON(b []byte) error {
	type plain cliFixtureHost
	var value plain
	if err := json.Unmarshal(b, &value); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	for _, k := range []string{"serial", "name", "cordoned", "running_vms", "approved", "vms", "labels"} {
		if len(raw[k]) == 0 || bytes.Equal(raw[k], []byte("null")) {
			return errors.New("incomplete host safety snapshot")
		}
	}
	*h = cliFixtureHost(value)
	return nil
}

func (r *cliExtended) hosts(ctx context.Context) ([]cliFixtureHost, error) {
	var out struct{ Hosts []cliFixtureHost }
	err := r.json(ctx, "host-list", &out, "hosts", "list")
	return out.Hosts, err
}

func (r *cliExtended) hostState(ctx context.Context, serial string, want bool) error {
	return r.observe(ctx, func(ctx context.Context) (bool, error) {
		hs, err := r.hosts(ctx)
		return slices.ContainsFunc(hs, func(h cliFixtureHost) bool { return h.Serial == serial && h.Cordoned == want }), err
	})
}

func (r *cliExtended) hostMaintenance() {
	branches := []string{"hosts drain", "hosts uncordon", "hosts re-image", "hosts diag"}
	f := r.c.Env.CLI
	if !r.c.Env.Safety.AllowDestructive || f == nil || f.Host == "" || f.VM == "" {
		r.gap(branches, "dedicated idle host and VM not declared or destructive checks disabled")
		return
	}
	hs, err := r.hosts(r.ctx)
	if err != nil {
		r.fail(branches[0], err.Error())
		return
	}
	var host *cliFixtureHost
	for i := range hs {
		h := &hs[i]
		if h.Serial == f.Host || h.Name == f.Host {
			host = h
			break
		}
	}
	if host == nil || host.Name != r.prefix+"-host" || host.Labels["cucina.sloper.ai/test-run"] != r.c.Env.RunID ||
		host.Labels["cucina.sloper.ai/test-fixture"] != "t20" || host.RunningVMs != 0 || !host.Approved ||
		(host.Phase != "Online" && host.Phase != "Cordoned") || f.VM != r.prefix+"-vm" {
		r.gap(branches, "host is not an approved run-owned idle fixture; maintenance refused")
		return
	}
	found := false
	for _, vm := range host.VMs {
		if vm.State != "stopped" || vm.Registered || !strings.HasPrefix(vm.Name, r.prefix+"-") || !strings.HasPrefix(vm.Pool, r.prefix+"-") {
			r.gap(branches, "fixture host contains non-disposable or non-stopped VMs")
			return
		}
		found = found || vm.Name == f.VM
	}
	if !found {
		r.gap(branches, "dedicated reimage VM absent")
		return
	}
	// Uncordoning must not make this fixture eligible for an enabled campaign
	// pool (empty selectors match every host). Never edit campaign selectors.
	var pools struct {
		Pools []struct {
			Name, Provider string
			Max            int
		}
	}
	if err := r.json(r.ctx, "pool-list", &pools, "pools", "list"); err != nil {
		r.fail(branches[0], err.Error())
		return
	}
	for _, p := range pools.Pools {
		if p.Provider != "tart" || p.Max == 0 {
			continue
		}
		var detail cliFixturePool
		if err := r.json(r.ctx, "pool-describe", &detail, "pools", "describe", p.Name); err != nil {
			r.fail(branches[0], err.Error())
			return
		}
		matches := true
		if detail.Spec.Tart != nil {
			for k, v := range detail.Spec.Tart.HostSelector {
				matches = matches && host.Labels[k] == v
			}
		}
		if matches {
			r.gap(branches, "fixture host matches an enabled Tart pool; uncordon would expose campaign capacity")
			return
		}
	}
	serial, original := host.Serial, host.Cordoned
	defer r.cleanup("host cordon restored", func(ctx context.Context) error {
		verb, action := "uncordon", "uncordon host"
		if original {
			verb, action = "drain", "drain host"
		}
		if err := r.ack(ctx, nil, action, serial, "hosts", verb, serial, "--yes"); err != nil {
			return err
		}
		return r.hostState(ctx, serial, original)
	})
	if !r.check("hosts drain", func() error {
		if err := r.ack(r.ctx, nil, "drain host", serial, "hosts", "drain", serial, "--yes"); err != nil {
			return err
		}
		return r.hostState(r.ctx, serial, true)
	}) {
		return
	}
	r.check("hosts re-image", func() error {
		return r.ack(r.ctx, nil, "re-image host", serial, "hosts", "re-image", serial, "--vm", f.VM, "--yes")
	})
	r.check("hosts diag", func() error { return r.download("host-diag", false, "hosts", "diag", serial) })
	r.check("hosts uncordon", func() error {
		if err := r.ack(r.ctx, nil, "uncordon host", serial, "hosts", "uncordon", serial); err != nil {
			return err
		}
		return r.hostState(r.ctx, serial, false)
	})
}

func (r *cliExtended) enrollment() {
	// The serial is synthetic and run-scoped; listing FIRST prevents this run
	// from taking over/removing an already registered record, even on a rerun.
	serial := "T20" + strings.ToUpper(strings.TrimPrefix(r.marker, r.prefix+"-"))
	hs, err := r.hosts(r.ctx)
	if err != nil {
		r.fail("hosts register", err.Error())
		return
	}
	if slices.ContainsFunc(hs, func(h cliFixtureHost) bool { return h.Serial == serial }) {
		r.gap([]string{"hosts register", "hosts approve", "hosts remove"}, "synthetic serial already exists; existing host left untouched")
	} else {
		r.serialLifecycle(serial)
	}
	r.enrollTokenLifecycle()
}

func (r *cliExtended) serialLifecycle(serial string) {
	owned := false
	defer r.cleanup("registered host removed", func(ctx context.Context) error {
		if !owned {
			return errors.New("registration outcome ambiguous; no owned host ID confirmed")
		}
		hs, err := r.hosts(ctx)
		if err != nil {
			return err
		}
		for _, h := range hs {
			if h.Serial == serial {
				if h.Site != r.prefix || h.Labels["cucina.sloper.ai/test-run"] != r.c.Env.RunID {
					return errors.New("refusing cleanup of a host without fixture ownership")
				}
				return r.removeSerial(ctx, serial)
			}
		}
		return nil
	})
	if !r.check("hosts register", func() error {
		var out struct {
			Registered []string `json:"registered"`
			Already    []string `json:"already_present"`
		}
		err := r.json(r.ctx, "host-register", &out, "hosts", "register", serial, "--site", r.prefix,
			"--label", "cucina.sloper.ai/test-run="+r.c.Env.RunID, "--label", "cucina.sloper.ai/test-fixture=t20")
		// Resolve the state even after a lost reply, so cleanup does not leak a
		// successfully registered host. Labels, site, and absence-before bind it.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), cliCommandLimit)
		defer cancel()
		_ = r.observe(ctx, func(ctx context.Context) (bool, error) {
			hs, readErr := r.hosts(ctx)
			owned = slices.ContainsFunc(hs, func(h cliFixtureHost) bool {
				return h.Serial == serial && h.Site == r.prefix && h.Labels["cucina.sloper.ai/test-run"] == r.c.Env.RunID
			})
			return owned, readErr
		})
		if err != nil {
			return err
		}
		if !owned || !slices.Equal(out.Registered, []string{serial}) || len(out.Already) != 0 {
			return errors.New("new run-owned host registration not observed")
		}
		return nil
	}) {
		return
	}
	if !r.check("hosts approve", func() error {
		if err := r.ack(r.ctx, nil, "approve host", serial, "hosts", "approve", serial); err != nil {
			return err
		}
		return r.observe(r.ctx, func(ctx context.Context) (bool, error) {
			hs, err := r.hosts(ctx)
			return slices.ContainsFunc(hs, func(h cliFixtureHost) bool { return h.Serial == serial && h.Approved }), err
		})
	}) {
		return
	}
	r.check("hosts remove", func() error { return r.removeSerial(r.ctx, serial) })
}

func (r *cliExtended) removeSerial(ctx context.Context, serial string) error {
	if err := r.ack(ctx, nil, "remove host", serial, "hosts", "remove", serial, "--yes"); err != nil {
		return err
	}
	return r.observe(ctx, func(ctx context.Context) (bool, error) {
		hs, err := r.hosts(ctx)
		return !slices.ContainsFunc(hs, func(h cliFixtureHost) bool { return h.Serial == serial }), err
	})
}

func (r *cliExtended) enrollTokenLifecycle() {
	type token struct {
		ID, Site, Description string
		Revoked               bool
	}
	list := func(ctx context.Context) ([]token, error) {
		var out struct{ Tokens []token }
		err := r.json(ctx, "enroll-token-list", &out, "hosts", "enroll-token", "list")
		return out.Tokens, err
	}
	before, err := list(r.ctx)
	if err != nil {
		r.fail("hosts enroll-token create", err.Error())
		return
	}
	owned := func(t token) bool {
		return t.ID != "" && t.Site == r.marker && t.Description == r.marker &&
			!slices.ContainsFunc(before, func(old token) bool { return old.ID == t.ID })
	}
	revoked := func(ctx context.Context, id string) error {
		return r.observe(ctx, func(ctx context.Context) (bool, error) {
			got, err := list(ctx)
			return slices.ContainsFunc(got, func(t token) bool { return t.ID == id && t.Revoked }), err
		})
	}
	var id string
	defer r.cleanup("created enrollment token revoked", func(ctx context.Context) error {
		// Resolve ambiguous create outcomes by this invocation's unique marker,
		// never by trusting an arbitrary response ID or revoking a prior record.
		var candidates []token
		if err := r.observe(ctx, func(ctx context.Context) (bool, error) {
			got, err := list(ctx)
			candidates = nil
			for _, t := range got {
				if owned(t) {
					candidates = append(candidates, t)
				}
			}
			return len(candidates) > 0, err
		}); err != nil {
			return err
		}
		for _, t := range candidates {
			if !t.Revoked {
				if err := r.ack(ctx, nil, "revoke enrollment token", t.ID, "hosts", "enroll-token", "revoke", t.ID, "--yes"); err != nil {
					return err
				}
			}
			if err := revoked(ctx, t.ID); err != nil {
				return err
			}
		}
		return nil
	})
	if !r.check("hosts enroll-token create", func() error {
		var out struct {
			ID, Token string
			ExpiresAt string `json:"expires_at"`
		}
		if err := r.json(r.ctx, "enroll-token", &out, "hosts", "enroll-token", "create", "--site", r.marker,
			"--ttl", "10m", "--max-hosts", "1", "--description", r.marker); err != nil {
			return err
		}
		if out.ID == "" || out.Token == "" || slices.ContainsFunc(before, func(t token) bool { return t.ID == out.ID }) {
			return errors.New("new enrollment token ID/secret missing or ID predates this run")
		}
		// Only the new ID is retained; the secret never goes to reports or files.
		id = out.ID
		return r.observe(r.ctx, func(ctx context.Context) (bool, error) {
			got, err := list(ctx)
			return slices.ContainsFunc(got, func(t token) bool { return t.ID == id && owned(t) && !t.Revoked }), err
		})
	}) {
		return
	}
	r.check("hosts enroll-token revoke", func() error {
		if err := r.ack(r.ctx, nil, "revoke enrollment token", id, "hosts", "enroll-token", "revoke", id, "--yes"); err != nil {
			return err
		}
		return revoked(r.ctx, id)
	})
}

type cliFixtureOperation struct {
	Name, Stage    string
	Invocation     string `json:"invocation_id"`
	ToolInvocation string `json:"tool_invocation_id"`
}

func (r *cliExtended) operations() {
	f := r.c.Env.CLI
	if f == nil || f.Operation == "" || f.InvocationID != r.prefix+"-invocation" {
		r.gap([]string{"ops watch", "ops kill"}, "dedicated run-scoped invocation and disposable operation not declared")
		return
	}
	find := func(ctx context.Context) (*cliFixtureOperation, error) {
		var out struct{ Operations []cliFixtureOperation }
		if err := r.json(ctx, "operation-list", &out, "ops", "list", "--invocation", f.InvocationID, "--limit", "1000"); err != nil {
			return nil, err
		}
		for _, op := range out.Operations {
			if op.Invocation != f.InvocationID && op.ToolInvocation != f.InvocationID {
				return nil, errors.New("operation escaped its isolated invocation filter")
			}
			if op.Name == f.Operation {
				return &op, nil
			}
		}
		return nil, nil
	}
	op, err := find(r.ctx)
	if err != nil {
		r.fail("ops watch", err.Error())
		return
	}
	if op == nil || (op.Stage != "queued" && op.Stage != "executing") {
		r.gap([]string{"ops watch", "ops kill"}, "declared disposable operation is not live in its isolated invocation")
		return
	}
	defer r.cleanup("disposable operation ended", func(ctx context.Context) error {
		op, err := find(ctx)
		if err != nil {
			return err
		}
		if op == nil || op.Stage == "completed" {
			return nil
		}
		return r.ack(ctx, nil, "kill", "operation "+f.Operation, "ops", "kill", f.Operation, "--yes", "--message", "T20 fixture cleanup")
	})
	r.check("ops watch", func() error {
		b, err := r.command(r.ctx, nil, nil, "ops", "watch", "--invocation", f.InvocationID, "--count", "1")
		if err != nil {
			return err
		}
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(make([]byte, 4096), cliOutputLimit)
		seen := false
		for sc.Scan() {
			var ev struct {
				Kind      string
				Operation *cliFixtureOperation
			}
			if err := r.contract(sc.Bytes(), "operation-event", &ev); err != nil {
				return err
			}
			if ev.Kind == "reconnecting" {
				continue
			}
			if (ev.Kind != "added" && ev.Kind != "changed" && ev.Kind != "removed") || ev.Operation == nil ||
				(ev.Operation.Invocation != f.InvocationID && ev.Operation.ToolInvocation != f.InvocationID) {
				return errors.New("operation watch did not yield a valid isolated operation event")
			}
			seen = seen || ev.Operation.Name == f.Operation
		}
		if sc.Err() != nil || !seen {
			return errors.New("operation watch ended without the declared operation (reconnection is not evidence)")
		}
		return nil
	})
	r.check("ops kill", func() error {
		if err := r.ack(r.ctx, nil, "kill", "operation "+f.Operation, "ops", "kill", f.Operation, "--yes", "--message", "T20 disposable fixture"); err != nil {
			return err
		}
		got, err := find(r.ctx)
		if err != nil {
			return err
		}
		if got != nil && got.Stage != "completed" {
			return errors.New("killed operation is still live")
		}
		return nil
	})
}

func (r *cliExtended) supportBundle() {
	r.check("diag", func() error { return r.download("support", true, "diag", "--include-logs") })
}

func (r *cliExtended) download(name string, archive bool, args ...string) error {
	path := filepath.Join(r.private, name+".bundle")
	var out struct {
		Path  string
		Bytes int64
	}
	if err := r.json(r.ctx, "file", &out, append(args, "--file", path)...); err != nil {
		return err
	}
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || out.Path != path || out.Bytes <= 0 || out.Bytes > 32<<20 || st.Size() != out.Bytes {
		return errors.New("diagnostic download did not produce the declared nonempty bounded file")
	}
	if !archive {
		return nil
	} // HostService supplies redacted bytes, not necessarily tar.gz.
	f, err := os.Open(path)
	if err != nil {
		return errors.New("cannot read private support bundle")
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return errors.New("support bundle is not gzip")
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(io.LimitReader(gz, 32<<20))
	version := false
	for count := 0; count < 1000; count++ {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			if !version {
				return errors.New("support bundle lacks version metadata")
			}
			return nil
		}
		if err != nil || h.Typeflag != tar.TypeReg || h.Size < 0 || h.Size > 32<<20 {
			return errors.New("invalid or oversized support archive")
		}
		if h.Name == "cucina-support/version.json" {
			b, err := io.ReadAll(io.LimitReader(tr, 64<<10))
			if err != nil || !json.Valid(b) {
				return errors.New("invalid support bundle version metadata")
			}
			version = true
		}
	}
	return errors.New("support bundle entry limit exceeded")
}

func (r *cliExtended) profiles() {
	branches := []string{"config path", "config profiles", "config use", "config set", "config delete", "logout", "logout --all --forget", "credential-helper install"}
	account := r.opts.ServiceAccount
	if !r.c.Env.Safety.AllowDestructive || account == "" || account == "break-glass" || r.c.Env.Endpoints.STS == "" {
		r.gap(branches, "verified non-bootstrap account/STS or permission to create a disposable key unavailable")
		return
	}
	type keyInfo struct {
		ID                   string `json:"key_id"`
		Account, Description string
		Revoked              bool
	}
	list := func(ctx context.Context) ([]keyInfo, error) {
		var out struct{ Keys []keyInfo }
		err := r.json(ctx, "service-key-list", &out, "keys", "list", "--account", account)
		return out.Keys, err
	}
	before, err := list(r.ctx)
	if err != nil {
		r.fail("config profiles", err.Error())
		return
	}
	owned := func(k keyInfo) bool {
		return k.ID != "" && k.Account == account && k.Description == r.marker &&
			!slices.ContainsFunc(before, func(old keyInfo) bool { return old.ID == k.ID })
	}
	// Register cleanup BEFORE the create request: a timeout can lose a successful
	// response. Unique description + account + preexisting IDs resolve ownership.
	defer r.cleanup("profile service key revoked", func(ctx context.Context) error {
		var candidates []keyInfo
		if err := r.observe(ctx, func(ctx context.Context) (bool, error) {
			got, err := list(ctx)
			candidates = nil
			for _, k := range got {
				if owned(k) {
					candidates = append(candidates, k)
				}
			}
			return len(candidates) > 0, err
		}); err != nil {
			return err
		}
		for _, k := range candidates {
			if !k.Revoked {
				if err := r.ack(ctx, nil, "revoke service key", k.ID, "keys", "revoke", k.ID, "--yes"); err != nil {
					return err
				}
			}
			if err := r.observe(ctx, func(ctx context.Context) (bool, error) {
				got, err := list(ctx)
				return slices.ContainsFunc(got, func(g keyInfo) bool { return g.ID == k.ID && g.Revoked }), err
			}); err != nil {
				return err
			}
		}
		return nil
	})
	var key struct {
		ID           string `json:"key_id"`
		Account, Key string
	}
	if err := r.json(r.ctx, "service-key", &key, "keys", "create", "--account", account, "--ttl", "10m", "--description", r.marker); err != nil {
		r.fail("config profiles", err.Error())
		return
	}
	if key.ID == "" || key.Key == "" || key.Account != account || slices.ContainsFunc(before, func(k keyInfo) bool { return k.ID == key.ID }) {
		r.fail("config profiles", "new isolated service key not returned; refusing to revoke an existing key")
		return
	}
	if err := r.observe(r.ctx, func(ctx context.Context) (bool, error) {
		got, err := list(ctx)
		return slices.ContainsFunc(got, func(k keyInfo) bool { return k.ID == key.ID && owned(k) && !k.Revoked }), err
	}); err != nil {
		r.fail("config profiles", err.Error())
		return
	}
	configDir := filepath.Join(r.private, "profiles")
	env := []string{"CUCINA_CONFIG_DIR=" + configDir, "CUCINA_OUTPUT=json"}
	local := func(ctx context.Context, schema string, out any, stdin []byte, args ...string) error {
		b, err := r.command(ctx, env, stdin, args...)
		if err != nil {
			return err
		}
		return r.contract(b, schema, out)
	}
	var profiles struct {
		ConfigDir string  `json:"config_dir"`
		Current   *string `json:"current_profile"`
		Profiles  []struct {
			Name       string
			LoggedIn   bool   `json:"logged_in"`
			Instance   string `json:"instance_name"`
			Management string
			Remote     string  `json:"remote_executor"`
			CA         *string `json:"ca_file"`
			Store      string  `json:"credential_store"`
		}
	}
	readProfiles := func() error { return local(r.ctx, "config", &profiles, nil, "config", "profiles") }
	names := []string{r.prefix + "-a", r.prefix + "-b"}
	for _, name := range names {
		args := []string{"login", r.c.Env.Endpoints.STS, "--profile", name, "--key", "-", "--credential-store", "file"}
		if r.c.Env.Endpoints.CAFile != "" {
			args = append(args, "--ca-file", r.c.Env.Endpoints.CAFile)
		}
		var out struct {
			Profile, Method string
			Subject         *string
		}
		if err := local(r.ctx, "login", &out, []byte(key.Key), args...); err != nil {
			r.fail("config profiles", err.Error())
			return
		}
		if out.Profile != name || out.Method != "service-key" || out.Subject == nil || *out.Subject != "sa:"+account {
			r.fail("config profiles", "isolated login did not establish the disposable profile identity")
			return
		}
	}
	r.check("config path", func() error {
		var out struct{ Path string }
		if err := local(r.ctx, "path", &out, nil, "config", "path"); err != nil {
			return err
		}
		if out.Path != configDir {
			return errors.New("config command escaped its private directory")
		}
		return nil
	})
	if !r.check("config profiles", func() error {
		if err := readProfiles(); err != nil {
			return err
		}
		if profiles.ConfigDir != configDir || len(profiles.Profiles) != 2 {
			return errors.New("disposable profiles not observed or ambient profiles leaked into the check")
		}
		for _, p := range profiles.Profiles {
			if !slices.Contains(names, p.Name) || !p.LoggedIn || p.Store != "file" {
				return errors.New("expected logged-in file-backed profiles not observed")
			}
		}
		return nil
	}) {
		return
	}
	if !r.check("config use", func() error {
		if err := r.ack(r.ctx, env, "use profile", names[1], "config", "use", names[1]); err != nil {
			return err
		}
		if err := readProfiles(); err != nil {
			return err
		}
		if profiles.Current == nil || *profiles.Current != names[1] {
			return errors.New("current profile did not change")
		}
		return nil
	}) {
		return
	}
	r.check("config set", func() error {
		for _, field := range []struct{ Key, Value string }{
			{"instance-name", r.prefix}, {"management", r.c.Env.Endpoints.Management},
			{"remote-executor", r.c.Env.Endpoints.RemoteExecution}, {"ca-file", r.c.Env.Endpoints.CAFile}, {"credential-store", "file"},
		} {
			if err := r.ack(r.ctx, env, "set", names[1]+"."+field.Key, "config", "set", field.Key, field.Value); err != nil {
				return err
			}
		}
		if err := readProfiles(); err != nil {
			return err
		}
		for _, p := range profiles.Profiles {
			if p.Name != names[1] {
				continue
			}
			ca := ""
			if p.CA != nil {
				ca = *p.CA
			}
			if p.Instance != r.prefix || p.Management != r.c.Env.Endpoints.Management || p.Remote != r.c.Env.Endpoints.RemoteExecution || p.Store != "file" || ca != r.c.Env.Endpoints.CAFile {
				return errors.New("profile settings did not round-trip")
			}
			return nil
		}
		return errors.New("configured profile vanished")
	})
	r.check("credential-helper install", func() error {
		dir := filepath.Join(r.private, "helper")
		var out struct {
			OK             bool
			Action, Target string
		}
		if err := local(r.ctx, "result", &out, nil, "credential-helper", "install", "--dir", dir); err != nil {
			return err
		}
		st, err := os.Lstat(out.Target)
		if !out.OK || out.Action != "install credential helper" || filepath.Dir(out.Target) != dir || err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
			return errors.New("credential helper installation not observed inside its private directory")
		}
		return nil
	})
	r.check("logout", func() error {
		if err := r.ack(r.ctx, env, "logout", names[0], "logout", "--profile", names[0]); err != nil {
			return err
		}
		if err := readProfiles(); err != nil {
			return err
		}
		for _, p := range profiles.Profiles {
			if p.Name == names[0] && !p.LoggedIn {
				return nil
			}
		}
		return errors.New("logout did not clear the disposable session")
	})
	r.check("config delete", func() error {
		if err := r.ack(r.ctx, env, "delete profile", names[0], "config", "delete", names[0]); err != nil {
			return err
		}
		if err := readProfiles(); err != nil {
			return err
		}
		if len(profiles.Profiles) != 1 || profiles.Profiles[0].Name != names[1] {
			return errors.New("profile deletion did not preserve the other isolated profile")
		}
		return nil
	})
	r.check("logout --all --forget", func() error {
		if err := r.ack(r.ctx, env, "logout", names[1], "logout", "--all", "--forget"); err != nil {
			return err
		}
		if err := readProfiles(); err != nil {
			return err
		}
		if len(profiles.Profiles) != 0 || profiles.Current != nil {
			return errors.New("logout --all --forget did not remove disposable profiles")
		}
		return nil
	})
}
