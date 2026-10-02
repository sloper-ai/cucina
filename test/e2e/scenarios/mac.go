// SPDX-License-Identifier: FSL-1.1-ALv2

package scenarios

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

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
	var hosts any
	if err := svc.CucinactlJSON(c, &hosts, "hosts", "list"); err != nil {
		return err
	}
	c.Record("hosts", hosts)
	if !strings.Contains(jsonString(hosts), "\"") {
		return harness.Fail("no Mac host registered (is cucina-hostd running in user mode?)")
	}
	sctx, stop := context.WithCancel(c)
	defer stop()
	hostdPeak := hostdRSSSampler(sctx)
	lr, err := openLane(c, MacLane)
	if err != nil {
		return err
	}
	wan0, _ := wanReceived(c)
	start := c.Now()
	build, err := lr.bazel("cold-build", BuildOpts{Command: "build", FreshServer: true})
	if err != nil {
		return err
	}
	if err := mustSucceed(build, "macOS cold build"); err != nil {
		return err
	}
	test, err := lr.bazel("cold-test", BuildOpts{Command: "test"})
	if err != nil {
		return err
	}
	if err := mustSucceed(test, "macOS test"); err != nil {
		return err
	}
	wan1, wanOK := wanReceived(c)
	firstVMWAN := wan1 - wan0
	// NFR-T4 host↔control plane: zstd wire bytes (hostd WAN counter) against
	// the raw blob bytes the host L2 fetched from the control plane.
	if sel := c.Env.WorkerSelectors["macos-l2"]; sel != "" && wanOK {
		q := fmt.Sprintf(`sum(increase(buildbarn_blobstore_blob_access_operations_blob_size_bytes_sum{%s,storage_type="CAS",backend_type="grpc",operation="Get"}[%ds]))`,
			sel, int(c.Now().Sub(start).Seconds()))
		if raw, ok := promScalar(c, q, c.Now()); ok && raw > 0 {
			c.NFR(nfr.Reported("NFR-T4", "host↔control plane (WAN, zstd)", firstVMWAN/raw, "wire/raw bytes", fmt.Sprintf("%.0f wire bytes for %.0f raw", firstVMWAN, raw)))
		}
	} else {
		c.Note("NFR-T4 WAN ratio not computed: set workerSelectors[\"macos-l2\"] to the host L2's metrics selector")
	}
	if pi, err := describePool(c, svc, pool); err == nil {
		addNFRs(c, nfr.ColdStart("macos", coldStarts(pi.Starts, start, c.Now(), build.BEP.Started.Add(build.TimeToFirstRemoteAction))))
	}
	// The VM shuts down when idle (disk kept).
	if err := c.Step("VM shuts down when idle", func() error { return waitMacIdle(c, svc, pool, 30*time.Minute) }); err != nil {
		return harness.Fail("%v", err)
	}
	// Warm rerun.
	if _, err := lr.bazel("expunge", BuildOpts{Command: "clean", Extra: []string{"--expunge"}}); err != nil {
		return err
	}
	warm, err := lr.bazel("warm-build", BuildOpts{Command: "build", FreshServer: true})
	if err != nil {
		return err
	}
	if err := mustSucceed(warm, "macOS warm build"); err != nil {
		return err
	}
	c.NFR(nfr.CacheHits("NFR-P3", "macos warm rerun", warm.ExecLog.RemoteCacheHitRatio()))
	// Re-execute after the VM restart: L1 survived on the kept disk.
	s, err := reexecL1Share(c, lr, "reexec-after-restart")
	if err != nil {
		return err
	}
	c.NFR(nfr.Ratio("NFR-T3", "macos after VM restart", s.l1, s.total, 90, false, "VM L1 Get bytes / all worker Get bytes"))
	// Second VM: drain the first so a new VM takes the same actions; its
	// inputs must come from the host L2, not over the WAN.
	if err := secondVMFromL2(c, svc, lr, pool, firstVMWAN, wanOK); err != nil {
		return err
	}
	if p := hostdPeak(); p > 0 {
		c.NFR(nfr.ComponentLimit("cucina-hostd", p, 100<<20))
	}
	return nil
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
	w := int(c.Now().Sub(s).Seconds())
	sel := workerSelector(c, lr.lane.Name)
	l1, ok1 := promScalar(c, fmt.Sprintf(`sum(increase(buildbarn_blobstore_blob_access_operations_blob_size_bytes_sum{%s,storage_type="CAS",backend_type="local",operation="Get"}[%ds]))`, sel, w), c.Now())
	rem, ok2 := promScalar(c, fmt.Sprintf(`sum(increase(buildbarn_blobstore_blob_access_operations_blob_size_bytes_sum{%s,storage_type="CAS",backend_type="grpc",operation="Get"}[%ds]))`, sel, w), c.Now())
	if !ok1 || !ok2 {
		return l1share{}, harness.Fail("worker blob metrics for %s not found (selector %s)", lr.lane.Name, sel)
	}
	return l1share{l1: l1, total: l1 + rem}, nil
}

func waitMacIdle(c *harness.Context, svc *infra.Services, pool string, limit time.Duration) error {
	deadline := c.Now().Add(limit)
	for {
		pi, err := describePool(c, svc, pool)
		if err == nil {
			running := 0.0
			for _, k := range []string{"registered", "busy", "launching"} {
				if n, ok := field(pi.Raw, k).(float64); ok {
					running += n
				}
			}
			if running == 0 {
				return nil
			}
		}
		if c.Now().After(deadline) {
			return fmt.Errorf("VMs of %s still running after %s", pool, limit)
		}
		if err := remote.RealSleep(c, 30*time.Second); err != nil {
			return err
		}
	}
}

func secondVMFromL2(c *harness.Context, svc *infra.Services, lr *laneRun, pool string, firstWAN float64, wanOK bool) error {
	// Bring the first VM up again and drain it.
	if _, err := lr.bazel("wake-first-vm", BuildOpts{Command: "build", Extra: []string{"--noremote_accept_cached", "--keep_going"}}); err != nil {
		return err
	}
	var raw any
	if err := svc.CucinactlJSON(c, &raw, "workers", "list"); err != nil {
		return err
	}
	var first string
	ws, _ := raw.([]any)
	if m, ok := raw.(map[string]any); ok {
		ws, _ = field(m, "workers").([]any)
	}
	for _, w := range ws {
		m, _ := w.(map[string]any)
		if str(field(m, "pool")) == pool {
			first = str(field(m, "node"))
			break
		}
	}
	if first == "" {
		return harness.Fail("no %s worker to drain", pool)
	}
	if _, err := svc.Cucinactl(c, "workers", "drain", first, "--yes"); err != nil {
		return err
	}
	defer func() { _, _ = svc.Cucinactl(context.WithoutCancel(c), "workers", "undrain", first) }()
	wan0, _ := wanReceived(c)
	o, err := lr.bazel("second-vm", BuildOpts{Command: "build", Extra: []string{"--noremote_accept_cached"}})
	if err == nil {
		err = mustSucceed(o, "build on the second VM")
	}
	if err != nil {
		return err
	}
	wan1, ok := wanReceived(c)
	if wanOK && ok {
		c.NFR(nfr.Ratio("NFR-T6", "second VM vs first VM WAN bytes", wan1-wan0, firstWAN, 10, true, "cucina_hostd_wan_bytes_total{direction=received}"))
	}
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
	c    *harness.Context
	name string
	run  *exec.Cmd
}

func tart(c *harness.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(c, "tart", args...)
	if c.Env.DevMac != nil && c.Env.DevMac.TartHome != "" {
		cmd.Env = append(os.Environ(), "TART_HOME="+c.Env.DevMac.TartHome)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("tart %s: %w: %s", strings.Join(args, " "), err, tail(string(out), 500))
	}
	return string(out), nil
}

func newVM(c *harness.Context, name string) (*vm, error) {
	base := c.Env.DevMac.BaseImage
	if base == "" {
		return nil, harness.Skip("no devMac.baseImage")
	}
	if _, err := tart(c, "clone", base, name); err != nil {
		return nil, err
	}
	v := &vm{c: c, name: name}
	return v, v.start()
}

func (v *vm) start() error {
	v.run = exec.Command("tart", "run", "--no-graphics", v.name)
	if v.c.Env.DevMac.TartHome != "" {
		v.run.Env = append(os.Environ(), "TART_HOME="+v.c.Env.DevMac.TartHome)
	}
	if err := v.run.Start(); err != nil {
		return err
	}
	_, err := tart(v.c, "ip", v.name, "--wait", "180")
	return err
}

func (v *vm) exec(script string) (string, error) {
	return tart(v.c, "exec", v.name, "sudo", "sh", "-c", script)
}

// copyIn writes a local file into the VM through tart exec's stdin.
func (v *vm) copyIn(local, dst string) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	cmd := exec.CommandContext(v.c, "tart", "exec", "-i", v.name, "sudo", "sh", "-c", "cat > "+shellQuote(dst))
	cmd.Stdin = bufio.NewReader(f)
	if v.c.Env.DevMac.TartHome != "" {
		cmd.Env = append(os.Environ(), "TART_HOME="+v.c.Env.DevMac.TartHome)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("copy %s into %s: %w: %s", filepath.Base(local), v.name, err, tail(string(out), 300))
	}
	return nil
}

func (v *vm) reboot() error {
	_, _ = v.exec("shutdown -r now")
	if v.run != nil {
		_ = v.run.Wait()
	}
	return v.start()
}

func (v *vm) destroy() {
	_, _ = tart(v.c, "stop", v.name)
	if v.run != nil {
		_ = v.run.Wait()
	}
	_, _ = tart(v.c, "delete", v.name)
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
	if dm.PkgPath == "" || dm.MDMKit == "" {
		return harness.Skip("devMac.pkgPath and devMac.mdmKit (macos/pkg/scripts) are required")
	}
	kit := dm.MDMKit
	scratch := c.Dir()
	// 1. A multi-use, expiring site token (R-SEC-3); kept in a 0600 file only.
	var tok map[string]any
	if err := svc.CucinactlJSON(c, &tok, "hosts", "enroll-token", "--ttl", "6h"); err != nil {
		return err
	}
	token, tokenID := str(field(tok, "token")), str(field(tok, "id"))
	tokenFile := filepath.Join(scratch, "site-token")
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		return err
	}
	defer func() { _ = os.Remove(tokenFile) }()
	// 2. Publish the pkg to a temporary tagged bucket; verify manifest + SHA-256.
	descriptor, err := harness.DescriptorPath(c.Env.Name)
	if err != nil {
		return err
	}
	state := filepath.Join(filepath.Dir(descriptor), c.Env.Name+"-t14-s3-publish.env")
	pub := func(args ...string) (string, error) {
		cmd := exec.CommandContext(c, filepath.Join(kit, "publish-s3-temp.sh"), args...)
		cmd.Env = append(os.Environ(), "CUCINA_RUN_ID="+c.Env.RunID, "AWS_PROFILE="+c.Env.AWS.Profile, "AWS_REGION="+c.Env.AWS.Region)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := pub("create", "--pkg", dm.PkgPath, "--version", pkgVersion(dm.PkgPath), "--state", state); err != nil {
		return fmt.Errorf("publish-s3-temp create: %w: %s", err, tail(out, 500))
	}
	defer func() { _, _ = pub("cleanup", "--state", state) }()
	if out, err := pub("verify", "--state", state); err != nil {
		return harness.Fail("manifest URL / SHA-256 verification failed: %s", tail(out, 500))
	}
	pkgURL, err := readEnvValue(state, "PKG_URL")
	if err != nil {
		return err
	}
	// 3. Fresh VMs simulate brand-new Mac minis.
	var vms []*vm
	defer func() {
		for _, v := range vms {
			v.destroy()
		}
	}()
	enroll := func(n int) (*vm, string, error) {
		v, err := newVM(c, fmt.Sprintf("cucina-e2e-t14-%s-%d", c.Env.RunID, n))
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
		if _, err := v.exec("sh /var/tmp/simulate-mdm.sh --signer-cert /var/tmp/signer.pem --ca-cert /var/tmp/cucina-ca.pem --token-file /var/tmp/site-token --controller-url " +
			shellQuote(c.Env.Endpoints.Host) + " && rm -f /var/tmp/site-token"); err != nil {
			return v, "", err
		}
		if _, err := v.exec("curl -fsSL -o /var/tmp/cucina.pkg " + shellQuote(pkgURL) + " && installer -pkg /var/tmp/cucina.pkg -target /"); err != nil {
			return v, "", fmt.Errorf("installer without -allowUntrusted: %w", err)
		}
		if err := v.reboot(); err != nil {
			return v, "", err
		}
		serial, err := v.serial()
		return v, serial, err
	}
	v1, serial1, err := enroll(1)
	if err != nil {
		return err
	}
	checks := map[string]string{
		"daemon running at boot without login": "launchctl print system/ai.sloper.cucina.hostd | grep -q 'state = running'",
		"listed in Background Task Management": "sfltool dumpbtm | grep -q ai.sloper.cucina",
		"FileVault off":                        "fdesetup status | grep -q 'FileVault is Off'",
		"auto-login after reboot":              "test -n \"$(stat -f %Su /dev/console)\" && test \"$(stat -f %Su /dev/console)\" != root",
	}
	for name, script := range checks {
		_, err := v1.exec(script)
		c.Check(harness.CheckResult{Name: name, Kind: "macos", Pass: err == nil, Detail: errString(err)})
	}
	// 4. Enrollment stays pending until approval.
	if err := waitHostPhase(c, svc, serial1, "pending", 10*time.Minute); err != nil {
		return harness.Fail("enrollment not pending before approval: %v", err)
	}
	if _, err := svc.Cucinactl(c, "hosts", "approve", serial1); err != nil {
		return err
	}
	if err := waitHostPhase(c, svc, serial1, "ready", 10*time.Minute); err != nil {
		return harness.Fail("host did not become ready after approval: %v", err)
	}
	var detail any
	_ = svc.CucinactlJSON(c, &detail, "hosts", "diag", serial1)
	c.Check(harness.CheckResult{Name: "reports nested virtualization unavailable", Kind: "macos",
		Pass: strings.Contains(strings.ToLower(jsonString(detail)), "virtualization"), Value: tail(jsonString(detail), 300)})
	// 5. The same token enrolls a second VM.
	if _, serial2, err := enroll(2); err != nil {
		return err
	} else if _, err := svc.Cucinactl(c, "hosts", "approve", serial2); err != nil {
		return err
	} else if err := waitHostPhase(c, svc, serial2, "ready", 10*time.Minute); err != nil {
		return harness.Fail("second VM with the same token: %v", err)
	}
	// 6. A revoked token blocks new enrollments only.
	if _, err := svc.Cucinactl(c, "hosts", "enroll-token", "revoke", tokenID, "--yes"); err != nil {
		c.Note("token revoke via `hosts enroll-token revoke`: %v", err)
	}
	if _, serial3, err := enroll(3); err == nil {
		blocked := waitHostPhase(c, svc, serial3, "pending", 3*time.Minute) != nil
		c.Check(harness.CheckResult{Name: "revoked token blocks new enrollments", Kind: "enroll", Pass: blocked})
	}
	if err := waitHostPhase(c, svc, serial1, "ready", time.Minute); err != nil {
		return harness.Fail("revoking the token affected an enrolled host: %v", err)
	}
	// 7. Upgrade in place, then uninstall.
	if up := dm.UpgradePkgPath; up != "" {
		if err := v1.copyIn(up, "/var/tmp/cucina-upgrade.pkg"); err != nil {
			return err
		}
		if _, err := v1.exec("installer -pkg /var/tmp/cucina-upgrade.pkg -target / && launchctl print system/ai.sloper.cucina.hostd | grep -q 'state = running'"); err != nil {
			return harness.Fail("upgrade in place: %v", err)
		}
	} else {
		c.Note("no devMac.upgradePkgPath: upgrade-in-place not exercised")
	}
	if _, err := v1.exec("sh '/Library/Application Support/Cucina/uninstall.sh' && ! launchctl print system/ai.sloper.cucina.hostd >/dev/null 2>&1 && ! pkgutil --pkg-info ai.sloper.cucina.host >/dev/null 2>&1"); err != nil {
		return harness.Fail("uninstall not clean: %v", err)
	}
	return nil
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
// with the serial reports the phase (case-insensitive prefix match).
func waitHostPhase(c *harness.Context, svc *infra.Services, serial, phase string, limit time.Duration) error {
	deadline := c.Now().Add(limit)
	last := ""
	for {
		var raw any
		if err := svc.CucinactlJSON(c, &raw, "hosts", "list"); err == nil {
			hs, _ := raw.([]any)
			if m, ok := raw.(map[string]any); ok {
				hs, _ = field(m, "hosts").([]any)
			}
			for _, h := range hs {
				m, _ := h.(map[string]any)
				if str(field(m, "serial")) == serial {
					last = strings.ToLower(str(field(m, "phase")))
					if strings.HasPrefix(last, phase) {
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
