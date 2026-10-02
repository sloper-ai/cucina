// SPDX-License-Identifier: FSL-1.1-ALv2

package scenarios

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/canary"
	"github.com/sloper-ai/cucina/internal/hostd/sys"
	"github.com/sloper-ai/cucina/internal/ports"
	cproto "github.com/sloper-ai/cucina/internal/proto"
	"github.com/sloper-ai/cucina/slo"
	"github.com/sloper-ai/cucina/test/e2e/harness"
	"github.com/sloper-ai/cucina/test/e2e/infra"
	"github.com/sloper-ai/cucina/test/e2e/nfr"
	"github.com/sloper-ai/cucina/test/e2e/remote"
)

// ---------------------------------------------------------------- T13

func t13() *harness.Scenario {
	return &harness.Scenario{
		ID: "T13", Title: "macOS on the dev Mac: hostd (user mode), Tart VM on demand, Abseil build + test, idle shutdown, warm rerun, re-execution after restart, second VM from L2",
		Requires: []harness.Requirement{harness.RequiresMacHost, harness.RequiresCucinactl, harness.RequiresPrometheus},
		Envs:     []harness.EnvKind{harness.EnvAWS},
		Cost:     harness.CostLow, EstimateUSD: 1, Essential: true, Timeout: 4 * time.Hour, DependsOn: []string{"T0"},
		NFRs: []string{"NFR-P1", "NFR-T3", "NFR-T6", "NFR-M2", "NFR-T4"},
		Post: Guards,
		Run:  runT13,
	}
}

func hostdRSSSampler(ctx context.Context) func() float64 {
	var mu sync.Mutex
	peak := 0.0
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			out, err := exec.CommandContext(ctx, "sh", "-c", "ps -o rss= -p \"$(pgrep -x cucina-hostd | head -n 1)\"").Output()
			if err == nil {
				if kb, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64); err == nil {
					mu.Lock()
					peak = max(peak, kb*1024)
					mu.Unlock()
				}
			}
			select {
			case <-ctx.Done():
				close(done)
				return
			case <-t.C:
			}
		}
	}()
	return func() float64 {
		mu.Lock()
		defer mu.Unlock()
		return peak
	}
}

func wanReceived(c *harness.Context) (float64, bool) {
	return promScalar(c, `sum(cucina_hostd_wan_bytes_total{direction="received"})`, time.Time{})
}

func runT13(c *harness.Context) error {
	svc, err := infra.Of(c)
	if err != nil {
		return err
	}
	pool := MacLane.Pool(c.Env)
	var hosts struct {
		Hosts []struct{ Serial, Phase string } `json:"hosts"`
	}
	if err := svc.CucinactlJSON(c, &hosts, "hosts", "list"); err != nil {
		return err
	}
	online := false
	for _, h := range hosts.Hosts {
		online = online || (h.Serial != "" && strings.EqualFold(h.Phase, v1alpha1.MacHostOnline))
	}
	if !online {
		return harness.Fail("no online Mac host registered (is cucina-hostd running in user mode?)")
	}
	initial, err := describePool(c, svc, pool)
	if err != nil {
		return err
	}
	if macRunning(initial) != 0 {
		return harness.Skip("T13 cold-start proof requires the Mac pool at zero")
	}
	sctx, stop := context.WithCancel(c)
	defer stop()
	hostdPeak := hostdRSSSampler(sctx)
	lr, err := openLane(c, MacLane)
	if err != nil {
		return err
	}
	wan0, wanStartOK := wanReceived(c)
	if !wanStartOK {
		return harness.Fail("missing initial Mac WAN counter")
	}
	start := c.Now()
	build, err := lr.bazel("cold-build", BuildOpts{Command: "build", FreshServer: true, ForceExecute: true})
	if err != nil {
		return err
	}
	if err := mustSucceed(build, "macOS cold build"); err != nil {
		return err
	}
	test, err := lr.bazel("cold-test", BuildOpts{Command: "test", ForceExecute: true})
	if err != nil {
		return err
	}
	if err := mustSucceed(test, "macOS test"); err != nil {
		return err
	}
	wan1, wanOK := wanReceived(c)
	firstVMWAN := wan1 - wan0
	if !wanOK || firstVMWAN <= 0 {
		return harness.Fail("missing or nonpositive first-VM WAN measurement")
	}
	first, err := macWorker(c, svc, pool, "")
	if err != nil {
		return err
	}
	// NFR-T4 host↔control plane: zstd wire bytes (hostd WAN counter) against
	// the raw blob bytes the host L2 fetched from the control plane.
	if sel := c.Env.WorkerSelectors["macos-l2"]; sel != "" && wanOK {
		q := slo.CASBytesIncrease(sel, "grpc", "Get", c.Now().Sub(start))
		if raw, ok := promScalar(c, q, c.Now()); ok && raw > 0 {
			c.NFR(nfr.Reported("NFR-T4", "host↔control plane (WAN, zstd)", firstVMWAN/raw, "wire/raw bytes", fmt.Sprintf("%.0f wire bytes for %.0f raw", firstVMWAN, raw)))
		}
	} else {
		c.Note("NFR-T4 WAN ratio not computed: set workerSelectors[\"macos-l2\"] to the host L2's metrics selector")
	}
	pi, err := describePool(c, svc, pool)
	if err != nil {
		return err
	}
	if build.BEP == nil || build.TimeToFirstRemoteAction <= 0 {
		return harness.Fail("missing macOS first remote action measurement")
	}
	samples := coldStarts(pi.Starts, start, c.Now(), build.BEP.Started.Add(build.TimeToFirstRemoteAction))
	if len(samples) == 0 {
		return harness.Fail("no macOS VM cold-start samples")
	}
	addNFRs(c, nfr.ColdStart("macos", samples))
	// The VM shuts down when idle (disk kept).
	if err := c.Step("VM shuts down when idle", func() error { return waitMacIdle(c, svc, pool, 30*time.Minute) }); err != nil {
		return harness.Fail("%v", err)
	}
	if err := requireMacVMState(c, svc, first, pool, "stopped"); err != nil {
		return err
	}
	// Warm rerun.
	clean, err := lr.bazel("expunge", BuildOpts{Command: "clean", Extra: []string{"--expunge"}})
	if err != nil {
		return err
	}
	if err := mustSucceed(clean, "macOS expunge"); err != nil {
		return err
	}
	warm, err := lr.bazel("warm-build", BuildOpts{Command: "build", FreshServer: true})
	if err != nil {
		return err
	}
	if err := mustSucceed(warm, "macOS warm build"); err != nil {
		return err
	}
	if warm.ExecLog == nil || warm.ExecLog.Spawns == 0 {
		return harness.Fail("warm macOS rebuild has no spawn evidence")
	}
	c.NFR(nfr.CacheHits("NFR-P3", "macos warm rerun", warm.ExecLog.RemoteCacheHitRatio()))
	if err := requireMacVMState(c, svc, first, pool, "stopped"); err != nil {
		return err
	}
	// Re-execute after the VM restart: L1 survived on the kept disk.
	s, err := reexecL1Share(c, lr, "reexec-after-restart")
	if err != nil {
		return err
	}
	restarted, err := macWorker(c, svc, pool, "")
	if err != nil {
		return err
	}
	if restarted != first {
		return harness.Fail("L1 proof requires the same kept VM after restart")
	}
	if err := requireMacVMState(c, svc, first, pool, "running"); err != nil {
		return err
	}
	c.NFR(nfr.Ratio("NFR-T3", "macos after VM restart", s.l1, s.total, 90, false, "VM L1 Get bytes / all worker Get bytes"))
	// Second VM: drain the first so a new VM takes the same actions; its
	// inputs must come from the host L2, not over the WAN.
	if err := secondVMFromL2(c, svc, lr, pool, firstVMWAN, wanOK); err != nil {
		return err
	}
	workerRSS, workerRSSPresent := promMax(c, workerRSSQuery(workerSelector(c, "macos")), start, c.Now())
	c.NFR(nfr.WorkerRSS(workerRSS, workerRSSPresent))
	if p := hostdPeak(); p > 0 {
		c.NFR(nfr.ComponentLimit("cucina-hostd", p, 100<<20))
	} else {
		return harness.Fail("hostd peak RSS was not measured")
	}
	return waitMacIdle(c, svc, pool, 30*time.Minute)
}

type l1share struct{ l1, total float64 }

func reexecL1Share(c *harness.Context, lr *laneRun, name string) (l1share, error) {
	s := c.Now()
	o, err := lr.bazel(name, BuildOpts{Command: "build", Extra: []string{"--noremote_accept_cached"}})
	if err == nil {
		err = mustSucceed(o, name)
	}
	if err != nil {
		return l1share{}, err
	}
	window := c.Now().Sub(s)
	sel := workerSelector(c, lr.lane.Name)
	l1, ok1 := promScalar(c, slo.CASBytesIncrease(sel, "local", "Get", window), c.Now())
	rem, ok2 := promScalar(c, slo.CASBytesIncrease(sel, "grpc", "Get", window), c.Now())
	if !ok1 || !ok2 || l1 < 0 || rem < 0 || l1+rem <= 0 {
		return l1share{}, harness.Fail("worker blob metrics for %s missing or have no input bytes (selector %s)", lr.lane.Name, sel)
	}
	return l1share{l1: l1, total: l1 + rem}, nil
}

func waitMacIdle(c *harness.Context, svc *infra.Services, pool string, limit time.Duration) error {
	deadline := c.Now().Add(limit)
	for {
		pi, err := describePool(c, svc, pool)
		if err != nil {
			return err
		}
		p, _ := pi.Raw["pool"].(map[string]any)
		stopped, _ := p["stopped"].(float64)
		if macRunning(pi) == 0 && stopped > 0 {
			return nil
		}
		if c.Now().After(deadline) {
			return fmt.Errorf("VMs of %s still running after %s", pool, limit)
		}
		if err := remote.RealSleep(c, 30*time.Second); err != nil {
			return err
		}
	}
}

func macRunning(pi PoolInfo) float64 {
	p, _ := pi.Raw["pool"].(map[string]any)
	n := 0.0
	for _, key := range []string{"registered", "busy", "idle", "launching", "draining"} {
		v, ok := p[key].(float64)
		if !ok {
			return -1
		} // absent telemetry is never idle
		n += v
	}
	return n
}

func macWorker(c *harness.Context, svc *infra.Services, pool, exclude string) (string, error) {
	var list struct {
		Workers []struct {
			Node, Pool, State string
			Drained           bool
		} `json:"workers"`
	}
	if err := svc.CucinactlJSON(c, &list, "workers", "list", "--pool", pool); err != nil {
		return "", err
	}
	var node string
	for _, w := range list.Workers {
		if w.Pool != pool || w.Node == exclude || w.Drained || strings.EqualFold(w.State, "stopped") {
			continue
		}
		if w.Node == "" || node != "" {
			return "", harness.Fail("Mac cache proof requires exactly one undrained VM in %s", pool)
		}
		node = w.Node
	}
	if node == "" {
		return "", harness.Fail("no running undrained Mac VM in %s", pool)
	}
	return node, nil
}

func requireMacVMState(c *harness.Context, svc *infra.Services, node, pool, state string) error {
	host, name, ok := strings.Cut(node, "/")
	if !ok {
		return harness.Fail("Mac worker node lacks host/VM identity")
	}
	var list struct {
		Hosts []struct {
			Name, Serial string
			VMs          []struct{ Name, Pool, State string } `json:"vms"`
		} `json:"hosts"`
	}
	if err := svc.CucinactlJSON(c, &list, "hosts", "list"); err != nil {
		return err
	}
	for _, h := range list.Hosts {
		if h.Serial != host && h.Name != host {
			continue
		}
		for _, v := range h.VMs {
			if v.Name == name && v.Pool == pool && strings.EqualFold(v.State, state) {
				return nil
			}
		}
	}
	return harness.Fail("kept Mac VM did not report %s", state)
}

func secondVMFromL2(c *harness.Context, svc *infra.Services, lr *laneRun, pool string, firstWAN float64, wanOK bool) error {
	// The previous re-execution already woke the retained first VM.
	first, err := macWorker(c, svc, pool, "")
	if err != nil {
		return err
	}
	if _, err := svc.Cucinactl(c, "workers", "drain", first, "--yes"); err != nil {
		return err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(c), 30*time.Second)
		defer cancel()
		_, err := svc.Cucinactl(ctx, "workers", "undrain", first)
		c.Check(harness.CheckResult{Name: "restore Mac worker drain", Kind: "cleanup", Pass: err == nil, Detail: errString(err)})
	}()
	wan0, beforeOK := wanReceived(c)
	if !wanOK || !beforeOK || firstWAN <= 0 {
		return harness.Fail("missing first/second VM WAN baseline")
	}
	o, err := lr.bazel("second-vm", BuildOpts{Command: "build", Extra: []string{"--noremote_accept_cached"}})
	if err == nil {
		err = mustSucceed(o, "build on the second VM")
	}
	if err != nil {
		return err
	}
	second, err := macWorker(c, svc, pool, first)
	if err != nil {
		return err
	}
	host1, _, ok1 := strings.Cut(first, "/")
	host2, _, ok2 := strings.Cut(second, "/")
	if !ok1 || !ok2 || host1 != host2 {
		return harness.Fail("second VM did not run on the first VM's host")
	}
	wan1, ok := wanReceived(c)
	if !ok || wan1 < wan0 {
		return harness.Fail("missing or reset second-VM WAN counter")
	}
	c.NFR(nfr.Ratio("NFR-T6", "second VM vs first VM WAN bytes", wan1-wan0, firstWAN, 10, true, "cucina_hostd_wan_bytes_total{direction=received}"))
	return nil
}

// ---------------------------------------------------------------- T14

func t14() *harness.Scenario {
	return &harness.Scenario{
		ID: "T14", Title: "Packaging and enrollment in fresh macOS 27 Tart VMs (pkg via MDM simulation, approval, token reuse, revocation, upgrade, uninstall)",
		Requires: []harness.Requirement{harness.RequiresMacHost, harness.RequiresHostdPkg, harness.RequiresCucinactl, harness.RequiresAWS},
		Envs:     []harness.EnvKind{harness.EnvAWS},
		Cost:     harness.CostLow, EstimateUSD: 0.5, Essential: true, Timeout: 3 * time.Hour,
		Run: runT14,
	}
}

// vm drives one throwaway Tart VM on the dev Mac.
type vm struct {
	c      *harness.Context
	name   string
	run    ports.Process
	sshKey string
	ip     string
}

func scenarioExec(c *harness.Context) ports.Exec {
	if svc, err := infra.Of(c); err == nil && svc.Exec != nil {
		return svc.Exec
	}
	return &sys.Exec{}
}

func tartCommand(c *harness.Context, args ...string) ports.Command {
	cmd := ports.Command{Path: "tart", Args: args, Env: os.Environ()}
	if c.Env.DevMac != nil && c.Env.DevMac.TartHome != "" {
		cmd.Env = append(cmd.Env, "TART_HOME="+c.Env.DevMac.TartHome)
	}
	return cmd
}

func tart(c *harness.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(c, 4*time.Minute)
	defer cancel()
	res, err := scenarioExec(c).Run(ctx, tartCommand(c, args...))
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("tart %s exited %d: %s", args[0], res.ExitCode, tail(string(res.Stderr), 500))
	}
	return string(res.Stdout), nil
}

func newVM(c *harness.Context, name, privateDir string) (*vm, error) {
	base := c.Env.DevMac.BaseImage
	if base == "" {
		return nil, harness.Skip("no devMac.baseImage")
	}
	out, err := tart(c, "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	var inventory []struct{ State string }
	if err := json.Unmarshal([]byte(out), &inventory); err != nil {
		return nil, err
	}
	running := 0
	for _, v := range inventory {
		if strings.EqualFold(v.State, "running") {
			running++
		}
	}
	if running >= 2 {
		return nil, harness.Skip("T14 needs a free macOS VM slot (two-VM host limit)")
	}
	if _, err := tart(c, "clone", base, name); err != nil {
		return nil, err
	}
	v := &vm{c: c, name: name}
	if err := v.start(); err != nil {
		_ = v.destroy()
		return nil, err
	}
	if err := v.bootstrapSSH(privateDir); err != nil {
		_ = v.destroy()
		return nil, err
	}
	return v, nil
}

func (v *vm) start() error {
	// Disposable package hosts follow the small-functional campaign limit,
	// regardless of the CPU/RAM configuration inherited from the golden image.
	if _, err := tart(v.c, "set", v.name, "--cpu", "2", "--memory", "8192"); err != nil {
		return err
	}
	var err error
	v.run, err = scenarioExec(v.c).Start(v.c, tartCommand(v.c, "run", "--no-graphics", v.name))
	if err != nil {
		return err
	}
	ip, err := tart(v.c, "ip", v.name, "--wait", "180")
	v.ip = strings.TrimSpace(ip)
	if err == nil && net.ParseIP(v.ip) == nil {
		return harness.Fail("Tart returned no guest IP")
	}
	return err
}

// Cirrus' guest RPC belongs to admin's GUI session. Bootstrap SSH before
// installing the package so auto-login changes and a no-login reboot cannot
// destroy the scenario's command channel (same approach as t14-vm.sh).
func (v *vm) bootstrapSSH(privateDir string) error {
	v.sshKey = filepath.Join(privateDir, v.name+"-ssh")
	r, err := scenarioExec(v.c).Run(v.c, ports.Command{Path: "ssh-keygen", Args: []string{"-q", "-t", "ed25519", "-N", "", "-f", v.sshKey}})
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return harness.Fail("cannot generate throwaway guest SSH key")
	}
	pub, err := os.ReadFile(v.sshKey + ".pub")
	if err != nil {
		return err
	}
	cmd := tartCommand(v.c, "exec", "-i", v.name, "/bin/sh", "-c", `umask 077; mkdir -p ~/.ssh; key=$(cat); test -n "$key" || exit 1; touch ~/.ssh/authorized_keys; grep -Fqx "$key" ~/.ssh/authorized_keys || printf '%s\n' "$key" >> ~/.ssh/authorized_keys; chmod 700 ~/.ssh; chmod 600 ~/.ssh/authorized_keys`)
	cmd.Stdin = pub
	ctx, cancel := context.WithTimeout(v.c, 3*time.Minute)
	defer cancel()
	for attempt := 0; attempt < 36; attempt++ {
		probe, stop := context.WithTimeout(ctx, 15*time.Second)
		r, err = scenarioExec(v.c).Run(probe, cmd)
		stop()
		if err == nil && r.ExitCode == 0 {
			return v.wait("true", 3*time.Minute)
		}
		if ctx.Err() != nil {
			break
		}
		if svc, err := infra.Of(v.c); err == nil && svc.Clock != nil {
			err = svc.Clock.Sleep(ctx, 5*time.Second)
			if err != nil {
				break
			}
		} else if err := remote.RealSleep(ctx, 5*time.Second); err != nil {
			break
		}
	}
	return harness.Fail("initial guest RPC did not accept the temporary SSH key within 3 minutes")
}

func (v *vm) sshCommand(script string) ports.Command {
	return ports.Command{Path: "ssh", Args: []string{"-i", v.sshKey, "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR", "-o", "ConnectTimeout=10", "-o", "BatchMode=yes", "admin@" + v.ip, "sudo -n sh -c " + shellQuote(script)}}
}

func (v *vm) exec(script string) (string, error) {
	ctx, cancel := context.WithTimeout(v.c, 4*time.Minute)
	defer cancel()
	r, err := scenarioExec(v.c).Run(ctx, v.sshCommand(script))
	if err != nil {
		return "", err
	}
	if r.ExitCode != 0 {
		return "", fmt.Errorf("guest command exited %d", r.ExitCode)
	}
	return string(r.Stdout), nil
}

// copyIn writes a local file through SSH's stdin; no credential appears in argv.
func (v *vm) copyIn(local, dst string) error {
	data, err := os.ReadFile(local)
	if err != nil {
		return err
	}
	cmd := v.sshCommand("umask 077; cat > " + shellQuote(dst))
	cmd.Stdin = data
	ctx, cancel := context.WithTimeout(v.c, 4*time.Minute)
	defer cancel()
	res, err := scenarioExec(v.c).Run(ctx, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("copy %s into VM exited %d", filepath.Base(local), res.ExitCode)
	}
	return nil
}

// As in macos/pkg/scripts/t14-vm.sh restart_vm, reboot the guest and wait
// for its return. Tart itself normally remains running across a guest reboot.
func (v *vm) reboot() error {
	before, err := v.exec("sysctl -n kern.boottime")
	if err != nil || strings.TrimSpace(before) == "" {
		return fmt.Errorf("cannot read guest boot identity: %w", err)
	}
	ctx, cancel := context.WithTimeout(v.c, 7*time.Minute)
	defer cancel()
	// A guest-agent disconnect may interrupt shutdown's response. Only a new
	// boot identity can prove the reboot; never wait unboundedly on tart run.
	_, shutdownErr := v.exec("shutdown -r now")
	for {
		probe, stop := context.WithTimeout(ctx, 15*time.Second)
		ip, ipErr := scenarioExec(v.c).Run(probe, tartCommand(v.c, "ip", v.name))
		if ipErr == nil && ip.ExitCode == 0 && net.ParseIP(strings.TrimSpace(string(ip.Stdout))) != nil {
			v.ip = strings.TrimSpace(string(ip.Stdout))
		}
		res, err := scenarioExec(v.c).Run(probe, v.sshCommand("sysctl -n kern.boottime"))
		stop()
		if err == nil && res.ExitCode == 0 && strings.TrimSpace(string(res.Stdout)) != "" && strings.TrimSpace(string(res.Stdout)) != strings.TrimSpace(before) {
			return nil
		}
		if err := remote.RealSleep(ctx, 5*time.Second); err != nil {
			return harness.Fail("guest did not reboot within 7 minutes (shutdown: %v)", shutdownErr)
		}
	}
}

func (v *vm) destroy() error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(v.c), 90*time.Second)
	defer cancel()
	if v.run != nil {
		res, err := scenarioExec(v.c).Run(ctx, tartCommand(v.c, "stop", v.name, "--timeout", "60"))
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("tart stop exited %d", res.ExitCode)
		}
		if _, err := v.run.Wait(ctx); err != nil {
			return err
		}
		v.run = nil
	}
	res, err := scenarioExec(v.c).Run(ctx, tartCommand(v.c, "delete", v.name))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("tart delete exited %d", res.ExitCode)
	}
	return nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func (v *vm) serial() (string, error) {
	out, err := v.exec("ioreg -rd1 -c IOPlatformExpertDevice | awk -F'\"' '/IOPlatformSerialNumber/ {print $4}'")
	return strings.TrimSpace(lastLine(out)), err
}

func runT14(c *harness.Context) error {
	svc, err := infra.Of(c)
	if err != nil {
		return err
	}
	dm := c.Env.DevMac
	if dm == nil || dm.PkgPath == "" || dm.UpgradePkgPath == "" || dm.MDMKit == "" || dm.BaseImage == "" {
		return harness.Skip("T14 requires initial and distinct upgrade packages, devMac.baseImage and devMac.mdmKit")
	}
	if c.Env.Endpoints.Enrollment == "" || c.Env.Endpoints.CAFile == "" || c.Env.AWS == nil {
		return harness.Skip("T14 requires a TLS enrollment endpoint, CA and temporary AWS publishing capability")
	}
	before, err := os.ReadFile(dm.PkgPath)
	if err != nil {
		return err
	}
	after, err := os.ReadFile(dm.UpgradePkgPath)
	if err != nil {
		return err
	}
	if sha256.Sum256(before) == sha256.Sum256(after) {
		return harness.Skip("T14 upgrade package is identical to the initial package")
	}
	kit := dm.MDMKit
	// Secrets stay off the bulk artifact volume, even during failed runs.
	descriptor, err := harness.DescriptorPath(c.Env.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(descriptor), 0o700); err != nil {
		return err
	}
	privateDir, err := os.MkdirTemp(filepath.Dir(descriptor), "t14-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(privateDir) }()
	// 1. Three possible enrollments prevents the max-hosts limit being mistaken
	// for revocation. Both installed hosts use the same explicit site.
	site := "e2e-" + c.Env.RunID
	token, tokenID, err := siteToken(c, svc, site)
	if err != nil {
		return err
	}
	tokenRevoked := false
	defer func() {
		if !tokenRevoked {
			revokeSiteToken(c, svc, tokenID)
		}
	}()
	tokenFile := filepath.Join(privateDir, "site-token")
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		return err
	}
	// 2. Publish the pkg to a temporary tagged bucket; verify manifest + SHA-256.
	state := filepath.Join(privateDir, "publish.env")
	pub := func(ctx context.Context, args ...string) error {
		res, err := scenarioExec(c).Run(ctx, ports.Command{Path: filepath.Join(kit, "publish-s3-temp.sh"), Args: args,
			Env: append(os.Environ(), "CUCINA_RUN_ID="+c.Env.RunID, "AWS_PROFILE="+c.Env.AWS.Profile, "AWS_REGION="+c.Env.AWS.Region)})
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("temporary package publisher exited %d", res.ExitCode)
		}
		return nil
	}
	defer func() {
		if !fileExists(state) {
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(c), 5*time.Minute)
		defer cancel()
		err := pub(ctx, "cleanup", "--state", state)
		c.Check(harness.CheckResult{Name: "temporary package bucket removed", Kind: "cleanup", Pass: err == nil, Detail: errString(err)})
	}()
	if err := pub(c, "create", "--pkg", dm.PkgPath, "--version", pkgVersion(dm.PkgPath), "--state", state); err != nil {
		return err
	}
	if err := pub(c, "verify", "--state", state); err != nil {
		return harness.Fail("manifest URL / SHA-256 verification failed: %v", err)
	}
	pkgURL, err := readEnvValue(state, "PKG_URL")
	if err != nil {
		return err
	}
	// 3. Fresh VMs simulate brand-new Mac minis.
	var vms []*vm
	defer func() {
		for _, v := range vms {
			if v == nil {
				continue
			}
			err := v.destroy()
			c.Check(harness.CheckResult{Name: "throwaway VM removed", Kind: "cleanup", Pass: err == nil, Detail: errString(err)})
		}
	}()
	enroll := func(n int) (*vm, string, error) {
		v, err := newVM(c, fmt.Sprintf("cucina-e2e-t14-%s-%d", c.Env.RunID, n), privateDir)
		if err != nil {
			return nil, "", err
		}
		vms = append(vms, v)
		for local, dst := range map[string]string{
			filepath.Join(kit, "simulate-mdm.sh"): "/var/tmp/simulate-mdm.sh",
			c.Env.Endpoints.CAFile:                "/var/tmp/cucina-ca.pem",
			tokenFile:                             "/var/tmp/site-token",
			signerCert(dm):                        "/var/tmp/signer.pem",
		} {
			if err := v.copyIn(local, dst); err != nil {
				return v, "", err
			}
		}
		if _, err := v.exec("sh /var/tmp/simulate-mdm.sh --no-local-copy --signer-cert /var/tmp/signer.pem --ca-cert /var/tmp/cucina-ca.pem --token-file /var/tmp/site-token --site " + shellQuote(site) + " --controller-url " +
			shellQuote(c.Env.Endpoints.Enrollment) + " && rm -f /var/tmp/site-token"); err != nil {
			return v, "", err
		}
		if _, err := v.exec("curl -fsSL -o /var/tmp/cucina.pkg " + shellQuote(pkgURL) + " && installer -pkg /var/tmp/cucina.pkg -target /"); err != nil {
			return v, "", fmt.Errorf("installer without -allowUntrusted: %w", err)
		}
		if err := v.reboot(); err != nil {
			return v, "", err
		}
		serial, err := v.serial()
		if err == nil && serial == "" {
			err = harness.Fail("fresh VM returned no hardware serial")
		}
		return v, serial, err
	}
	v1, serial1, err := enroll(1)
	if err != nil {
		return err
	}
	checks := map[string]string{
		"daemon running after reboot":          "launchctl print system/ai.sloper.cucina.hostd | grep -q 'state = running'",
		"listed in Background Task Management": "sfltool dumpbtm | grep -q ai.sloper.cucina",
		"FileVault off":                        "fdesetup status | grep -q 'FileVault is Off'",
		"auto-login after reboot":              "test \"$(stat -f %Su /dev/console)\" = cucina",
	}
	for name, script := range checks {
		if err := v1.wait(script, 2*time.Minute); err != nil {
			return harness.Fail("%s: %v", name, err)
		}
	}
	if _, err := v1.exec("sysadminctl -autologin off"); err != nil {
		return err
	}
	if err := v1.reboot(); err != nil {
		return err
	}
	if err := v1.wait("test \"$(stat -f %Su /dev/console)\" = root && launchctl print system/ai.sloper.cucina.hostd | grep -q 'state = running'", 2*time.Minute); err != nil {
		return harness.Fail("daemon must start without a login: %v", err)
	}
	if _, err := v1.exec("/usr/local/cucina/bin/cucina-host-setup autologin --reset-password"); err != nil {
		return err
	}
	if err := v1.reboot(); err != nil {
		return err
	}
	if err := v1.wait(checks["auto-login after reboot"], 2*time.Minute); err != nil {
		return err
	}
	// 4. Enrollment stays pending until approval.
	if err := waitHostPhase(c, svc, serial1, v1alpha1.MacHostPending, 10*time.Minute); err != nil {
		return harness.Fail("enrollment not pending before approval: %v", err)
	}
	if _, err := svc.Cucinactl(c, "hosts", "approve", serial1); err != nil {
		return err
	}
	if err := waitHostPhase(c, svc, serial1, v1alpha1.MacHostOnline, 10*time.Minute); err != nil {
		return harness.Fail("host did not become online after approval: %v", err)
	}
	diagFile := filepath.Join(c.Dir(), "host-diagnostics.json")
	if _, err := svc.Cucinactl(c, "hosts", "diag", serial1, "--file", diagFile); err != nil {
		return err
	}
	data, err := os.ReadFile(diagFile)
	if err != nil {
		return err
	}
	var detail struct {
		Virtualization struct {
			Available *bool  `json:"available"`
			Reason    string `json:"reason"`
			HVSupport *int   `json:"hv_support"`
		} `json:"virtualization"`
	}
	if err := json.NewDecoder(strings.NewReader(string(data))).Decode(&detail); err != nil {
		return err
	}
	v := detail.Virtualization
	c.Check(harness.CheckResult{Name: "reports nested virtualization unavailable", Kind: "macos",
		Pass:   v.Available != nil && !*v.Available && v.Reason == "nested-macos" && v.HVSupport != nil,
		Detail: "explicit virtualization capability from the downloaded host diagnostics"})
	// 5. The same token enrolls a second, distinct VM.
	v2, serial2, err := enroll(2)
	if err != nil {
		return err
	}
	if serial2 == serial1 {
		return harness.Fail("fresh VMs have the same hardware serial")
	}
	if err := waitHostPhase(c, svc, serial2, v1alpha1.MacHostPending, 10*time.Minute); err != nil {
		return err
	}
	if _, err := svc.Cucinactl(c, "hosts", "approve", serial2); err != nil {
		return err
	}
	if err := waitHostPhase(c, svc, serial2, v1alpha1.MacHostOnline, 10*time.Minute); err != nil {
		return err
	}
	// Release the second slot before creating the third host. Never three
	// concurrently running macOS guests, including on the developer's Mac.
	if err := v2.destroy(); err != nil {
		return err
	}
	vms[1] = nil
	// 6. An explicit TOKEN_INVALID response proves denial; timeouts do not.
	revokedAt := c.Now()
	if _, err := svc.Cucinactl(c, "hosts", "enroll-token", "revoke", tokenID, "--yes"); err != nil {
		return err
	}
	tokenRevoked = true
	_, serial3, err := enroll(3)
	if err != nil {
		return err
	}
	if serial3 == serial1 || serial3 == serial2 {
		return harness.Fail("third VM is not a new serial")
	}
	if err := checkRevokedEnrollment(c, svc, token, site, serial3); err != nil {
		return err
	}
	if err := waitHostHeartbeat(c, svc, serial1, revokedAt, 3*time.Minute); err != nil {
		return harness.Fail("enrolled host did not heartbeat after token revocation: %v", err)
	}
	// 7. The upgrade must change the installed receipt version and preserve
	// the enrolled host identity; an omitted or identical package is not proof.
	versionCmd := "pkgutil --pkg-info-plist ai.sloper.cucina.host | plutil -extract pkg-version raw -o - -"
	oldVersion, err := v1.exec(versionCmd)
	if err != nil || strings.TrimSpace(oldVersion) == "" {
		return harness.Fail("initial package has no installed version")
	}
	if err := v1.copyIn(dm.UpgradePkgPath, "/var/tmp/cucina-upgrade.pkg"); err != nil {
		return err
	}
	upgraded := c.Now()
	if _, err := v1.exec("installer -pkg /var/tmp/cucina-upgrade.pkg -target /"); err != nil {
		return harness.Fail("upgrade in place: %v", err)
	}
	newVersion, err := v1.exec(versionCmd)
	if err != nil || strings.TrimSpace(newVersion) == "" || strings.TrimSpace(newVersion) == strings.TrimSpace(oldVersion) {
		return harness.Fail("upgrade did not change the installed package version")
	}
	if err := v1.wait(checks["daemon running after reboot"], 2*time.Minute); err != nil {
		return err
	}
	if err := waitHostHeartbeat(c, svc, serial1, upgraded, 3*time.Minute); err != nil {
		return err
	}
	if _, err := v1.exec("/usr/local/cucina/bin/cucina-host-uninstall --yes && ! launchctl print system/ai.sloper.cucina.hostd >/dev/null 2>&1 && ! pkgutil --pkg-info ai.sloper.cucina.host >/dev/null 2>&1"); err != nil {
		return harness.Fail("uninstall not clean: %v", err)
	}
	return nil
}

func (v *vm) wait(script string, limit time.Duration) error {
	ctx, cancel := context.WithTimeout(v.c, limit)
	defer cancel()
	for {
		probe, stop := context.WithTimeout(ctx, 15*time.Second)
		r, err := scenarioExec(v.c).Run(probe, v.sshCommand(script))
		stop()
		if err == nil && r.ExitCode == 0 {
			return nil
		}
		if err := remote.RealSleep(ctx, 5*time.Second); err != nil {
			return err
		}
	}
}

func siteToken(c *harness.Context, svc *infra.Services, site string) (string, string, error) {
	var tok struct{ ID, Token string }
	if err := svc.CucinactlJSON(c, &tok, "hosts", "enroll-token", "create", "--site", site, "--ttl", "6h", "--max-hosts", "3"); err != nil {
		return "", "", err
	}
	if tok.Token == "" || tok.ID == "" {
		return "", "", harness.Fail("enroll-token create returned no token/id")
	}
	return tok.Token, tok.ID, nil
}

func revokeSiteToken(c *harness.Context, svc *infra.Services, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c), 30*time.Second)
	defer cancel()
	_, err := svc.Cucinactl(ctx, "hosts", "enroll-token", "revoke", id, "--yes")
	c.Check(harness.CheckResult{Name: "temporary site token revoked", Kind: "cleanup", Pass: err == nil, Detail: errString(err)})
}

func checkRevokedEnrollment(c *harness.Context, svc *infra.Services, token, site, serial string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		return err
	}
	ep := canary.Endpoint{Target: strings.TrimPrefix(c.Env.Endpoints.Enrollment, "https://"), CAFile: c.Env.Endpoints.CAFile}
	conn, err := ep.Dial("")
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	client := cucinav1.NewEnrollmentServiceClient(conn)
	request := &cucinav1.EnrollHostRequest{Protocol: &cucinav1.ProtocolVersion{Major: cproto.Major, Minor: cproto.Minor},
		SiteToken: token, SerialNumber: serial, Hostname: "t14", CsrPem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}), Facts: &cucinav1.HostFacts{Site: site}}
	probe := func() (*cucinav1.EnrollHostResponse, error) {
		ctx, cancel := context.WithTimeout(c, 30*time.Second)
		defer cancel()
		return client.EnrollHost(ctx, request)
	}
	r, err := probe()
	if err != nil {
		return fmt.Errorf("revoked enrollment probe failed instead of denying: %w", err)
	}
	if r.GetStatus() != cucinav1.EnrollHostResponse_STATUS_TOKEN_INVALID || len(r.GetCertificatePem()) != 0 {
		return harness.Fail("revoked token must return TOKEN_INVALID without a certificate; got %s", r.GetStatus())
	}
	// A fresh token on the exact same endpoint/site/serial must reach pending
	// approval, ruling out a wrong route, broken CSR, outage or unknown site.
	control, id, err := siteToken(c, svc, site)
	if err != nil {
		return err
	}
	defer revokeSiteToken(c, svc, id)
	request.SiteToken = control
	r, err = probe()
	if err != nil {
		return err
	}
	if r.GetStatus() != cucinav1.EnrollHostResponse_STATUS_PENDING || len(r.GetCertificatePem()) != 0 {
		return harness.Fail("valid enrollment control must be pending approval, got %s", r.GetStatus())
	}
	return nil
}

func waitHostHeartbeat(c *harness.Context, svc *infra.Services, serial string, after time.Time, limit time.Duration) error {
	ctx, cancel := context.WithTimeout(c, limit)
	defer cancel()
	for {
		var list struct {
			Hosts []struct {
				Serial, Phase string
				LastHeartbeat *time.Time `json:"last_heartbeat"`
			} `json:"hosts"`
		}
		if err := svc.CucinactlJSON(ctx, &list, "hosts", "list"); err != nil {
			return err
		}
		for _, h := range list.Hosts {
			if h.Serial == serial && strings.EqualFold(h.Phase, v1alpha1.MacHostOnline) && h.LastHeartbeat != nil && h.LastHeartbeat.After(after) {
				return nil
			}
		}
		if err := remote.RealSleep(ctx, 5*time.Second); err != nil {
			return err
		}
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return tail(err.Error(), 300)
}

func signerCert(dm *harness.DevMacEnv) string {
	if dm.SignerCert != "" {
		return dm.SignerCert
	}
	return filepath.Join(filepath.Dir(dm.PkgPath), "signer.pem")
}

func pkgVersion(pkg string) string {
	base := strings.TrimSuffix(filepath.Base(pkg), ".pkg")
	if i := strings.LastIndex(base, "-"); i >= 0 {
		return base[i+1:]
	}
	return "0.0.0"
}

func readEnvValue(path, key string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, key+"="); ok {
			return strings.Trim(v, `"'`), nil
		}
	}
	return "", fmt.Errorf("%s not in %s", key, path)
}

// waitHostPhase polls `cucinactl hosts list --output json` until the host
// with the serial reports the exact phase (case-insensitive).
func waitHostPhase(c *harness.Context, svc *infra.Services, serial, phase string, limit time.Duration) error {
	deadline := c.Now().Add(limit)
	last := ""
	for {
		var raw any
		if err := svc.CucinactlJSON(c, &raw, "hosts", "list"); err != nil {
			return err
		} else {
			hs, _ := raw.([]any)
			if m, ok := raw.(map[string]any); ok {
				hs, _ = field(m, "hosts").([]any)
			}
			for _, h := range hs {
				m, _ := h.(map[string]any)
				if str(field(m, "serial")) == serial {
					last = strings.ToLower(str(field(m, "phase")))
					if strings.EqualFold(last, phase) {
						return nil
					}
				}
			}
		}
		if c.Now().After(deadline) {
			return fmt.Errorf("host %s phase %q, want %q", serial, last, phase)
		}
		if err := remote.RealSleep(c, 15*time.Second); err != nil {
			return err
		}
	}
}
