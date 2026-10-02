// SPDX-License-Identifier: FSL-1.1-ALv2

package scenarios

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sloper-ai/cucina/internal/canary"
	"github.com/sloper-ai/cucina/test/e2e/bazelrun"
	"github.com/sloper-ai/cucina/test/e2e/collect/execlog"
	"github.com/sloper-ai/cucina/test/e2e/harness"
	"github.com/sloper-ai/cucina/test/e2e/infra"
	"github.com/sloper-ai/cucina/test/e2e/remote"
)

// readOnlyCommands are the cucinactl commands whose --output json must parse
// (R-CLI-3; the mutating ones are exercised below with their own checks).
var readOnlyCommands = [][]string{
	{"status"}, {"pools", "list"}, {"workers", "list"}, {"hosts", "list"}, {"queues"}, {"ops", "list"},
	{"keys", "list"}, {"keys", "revocations"}, {"hosts", "enroll-token", "list"}, {"cost"}, {"images"}, {"whoami"}, {"config", "view"},
}

func t20() *harness.Scenario {
	return &harness.Scenario{
		ID: "T20", Title: "CLI/TUI: every cucinactl command against the live cluster, drain/undrain, key revocation, failed-action inspection, binaries on all clients, TUI in a PTY",
		Requires: []harness.Requirement{harness.RequiresCucinactl, harness.RequiresKubernetes, harness.RequiresAWS, harness.RequiresLinuxClient, harness.RequiresWindowsClient, harness.RequiresMacHost},
		Cost:     harness.CostLow, EstimateUSD: 1, MaxInstances: 4, Essential: true, Timeout: 2 * time.Hour,
		Post: Guards,
		Run:  runT20,
	}
}

func runT20(c *harness.Context) error {
	svc, err := infra.Of(c)
	if err != nil {
		return err
	}
	var bad []string
	check := func(name string, ok bool, detail string) {
		c.Check(harness.CheckResult{Name: name, Kind: "cli", Pass: ok, Detail: detail})
		if !ok {
			bad = append(bad, name+": "+detail)
		}
	}
	// 1. Read-only commands, JSON output.
	for _, args := range readOnlyCommands {
		var v any
		err := svc.CucinactlJSON(c, &v, args...)
		check("cucinactl "+strings.Join(args, " ")+" --output json", err == nil, errString(err))
	}
	pool := LinuxLane.Pool(c.Env)
	var pd any
	err = svc.CucinactlJSON(c, &pd, "pools", "describe", pool)
	check("cucinactl pools describe --output json", err == nil, errString(err))
	_, err = svc.Cucinactl(c, "completions", "bash")
	check("cucinactl completions bash", err == nil, errString(err))

	// 2. A small scale-out (with the TUI recording it) for drain/undrain.
	lr, err := openLane(c, LinuxLane)
	if err != nil {
		return err
	}
	check("bazel clients use the cucinactl credential helper", strings.Contains(readRC(c, lr), "--credential_helper="), "cucina.bazelrc")
	tui, stopTUI := startTUI(c, svc)
	done := make(chan error, 1)
	go func() {
		o, err := lr.bazel("scale-out", BuildOpts{Command: "build", Extra: []string{"--noremote_accept_cached"}})
		if err == nil {
			err = mustSucceed(o, "scale-out build")
		}
		done <- err
	}()
	node, err := busyWorker(c, svc, pool, 30*time.Minute)
	if err == nil {
		_, err1 := svc.Cucinactl(c, "workers", "drain", node, "--yes")
		check("cucinactl workers drain", err1 == nil, errString(err1))
		_, err2 := svc.Cucinactl(c, "workers", "undrain", node)
		check("cucinactl workers undrain", err2 == nil, errString(err2))
	} else {
		check("a worker to drain", false, err.Error())
	}
	buildErr := <-done
	transcript := stopTUI()
	if tui {
		ok, why := tuiShowsChanges(transcript, pool)
		check("TUI shows live queue and worker changes", ok, why)
	} else {
		check("TUI session started", false, "PTY or cucinactl unavailable; no TUI evidence")
	}
	if buildErr != nil {
		check("build while draining", false, buildErr.Error())
	}
	// 3. Create and revoke a service key.
	control, err := campaignToken(c)
	if err != nil {
		return err
	}
	key, id, err := createServiceKey(c, svc, control.Subject, "e2e-t20")
	if err != nil {
		return err
	}
	defer revokeCreatedKey(c, svc, id)
	_, err = svc.Cucinactl(c, "keys", "revoke", id, "--yes")
	check("cucinactl keys revoke", err == nil, errString(err))
	if err == nil {
		code, oauthErr, err := exchange(c, c.Env, key, canary.ServiceKeyTokenType)
		check("revoked service key cannot exchange", err == nil && code == 400 && oauthErr == "invalid_grant", fmt.Sprintf("HTTP %d %s", code, oauthErr))
		check("valid control remains accessible", casAccess(c, c.Env, control.Raw) == nil, "same REAPI endpoint")
	}
	// 4. Inspect a failed action.
	digest, err := failingAction(c, lr)
	if err != nil {
		check("a remotely failed action", false, err.Error())
	} else {
		var inspect struct {
			Result *struct {
				ExitCode int `json:"exit_code"`
				Stderr   *struct {
					Text string `json:"text"`
				} `json:"stderr"`
			} `json:"result"`
		}
		err := svc.CucinactlJSON(c, &inspect, "action", "inspect", digest)
		check("cucinactl action inspect (failed action)", err == nil && inspect.Result != nil && inspect.Result.ExitCode == 3 && inspect.Result.Stderr != nil && strings.Contains(inspect.Result.Stderr.Text, "boom"), errString(err))
	}
	// 5. The binaries run on every client.
	for _, h := range []string{"linux-client", "windows-client"} {
		if _, ok := c.Env.Clients[h]; !ok {
			return harness.Skip("T20 requires %s binary execution", h)
		}
		host, err := svc.Host(c, h)
		if err != nil {
			return err
		}
		bin, err := installCucinactl(c, svc, host)
		if err != nil {
			check("cucinactl on "+h, false, err.Error())
			continue
		}
		res, err := host.Run(c, quoteFor(host, bin)+" status --output json", remote.Opts{User: svc.ClientUser(h)})
		ok := err == nil && res.ExitCode == 0 && json.Valid(res.Stdout)
		check("cucinactl status on "+h, ok, errString(err))
	}
	extendedErr := runT20Extended(c, svc, lr, node, digest)
	if len(bad) > 0 {
		return harness.Fail("%d CLI checks failed: %s", len(bad), strings.Join(bad, "; "))
	}
	return extendedErr
}

func readRC(c *harness.Context, lr *laneRun) string {
	res, err := lr.host.Run(c, readFileScript(lr.host, hjoin(lr.host, lr.ws, "cucina.bazelrc")), remote.Opts{User: lr.user})
	if err != nil {
		return ""
	}
	return string(res.Stdout)
}

// startTUI records `cucinactl tui` in a pseudo-terminal (script(1)) on the
// dev Mac; stop quits it with "q" and returns the transcript.
func startTUI(c *harness.Context, svc *infra.Services) (bool, func() string) {
	bin, err := svc.CucinactlPath()
	if err != nil {
		return false, func() string { return "" }
	}
	file := filepath.Join(c.Dir(), "tui-transcript.txt")
	cmd := exec.CommandContext(c, "script", "-q", file, bin, "tui")
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "COLUMNS=160", "LINES=48")
	in, err := cmd.StdinPipe()
	if err != nil || cmd.Start() != nil {
		return false, func() string { return "" }
	}
	return true, func() string {
		_, _ = io.WriteString(in, "q")
		_ = in.Close()
		waited := make(chan struct{})
		go func() { _ = cmd.Wait(); close(waited) }()
		select {
		case <-waited:
		case <-time.After(20 * time.Second):
			_ = cmd.Process.Kill()
		}
		b, _ := os.ReadFile(file)
		_ = c.Artifact("tui-transcript", file)
		return string(b)
	}
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b[()][0-9A-Za-z]|\x1b[=>]`)

// tuiShowsChanges reports whether the recorded screens mention the pool and
// differ over time (frames are separated by full redraws).
func tuiShowsChanges(transcript, pool string) (bool, string) {
	plain := ansi.ReplaceAllString(transcript, "")
	if !strings.Contains(plain, pool) {
		return false, "the pool never appears on screen"
	}
	frames := strings.Split(plain, pool)
	distinct := map[string]bool{}
	for _, f := range frames {
		if len(f) > 40 {
			distinct[f[:40]] = true
		}
	}
	if len(distinct) < 2 {
		return false, "the screen never changed"
	}
	return true, fmt.Sprintf("%d distinct frames", len(distinct))
}

// failingAction builds a one-genrule workspace remotely whose action exits 3
// and returns its action digest ("hash/size") from the execution log.
func failingAction(c *harness.Context, lr *laneRun) (string, error) {
	h := lr.host
	ws := hjoin(h, h.WorkDir(), "failing-ws")
	files := map[string]string{
		"MODULE.bazel":  "module(name = \"failing\")\n",
		"BUILD.bazel":   "genrule(\n    name = \"boom\",\n    outs = [\"boom.txt\"],\n    cmd = \"echo boom >&2; exit 3\",\n)\n",
		".bazelversion": "9.2.0\n",
	}
	for name, content := range files {
		local := filepath.Join(c.Dir(), "failing-"+name)
		if err := os.WriteFile(local, []byte(content), 0o644); err != nil {
			return "", err
		}
		if err := h.Put(c, local, hjoin(h, ws, name)); err != nil {
			return "", err
		}
	}
	inv := bazelrun.Invocation{Name: "T20-failing", Host: h, Workspace: ws, User: lr.user, Collect: true,
		Startup: []string{"--bazelrc=" + hjoin(h, lr.ws, "cucina.bazelrc")}, Command: "build", Args: []string{"--", "//:boom"}}
	o, err := bazelrun.Run(c, inv, filepath.Join(c.Dir(), "failing"))
	if err != nil {
		return "", err
	}
	if o.ExitCode == 0 {
		return "", fmt.Errorf("the failing genrule succeeded")
	}
	l, err := execlog.ReadFile(o.ExecLogPath)
	if err != nil {
		return "", err
	}
	for _, s := range l.Spawns {
		if s.Remote() && s.ExitCode == 3 && !s.ActionDigest.IsZero() {
			return s.ActionDigest.String(), nil
		}
	}
	return "", fmt.Errorf("no failed remote spawn in the execution log")
}
