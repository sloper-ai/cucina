// SPDX-License-Identifier: FSL-1.1-ALv2

package scenarios_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/fakes"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/test/e2e/harness"
	"github.com/sloper-ai/cucina/test/e2e/infra"
	"github.com/sloper-ai/cucina/test/e2e/scenarios"
)

// extendedCLI is a stateful fake of the public CLI protocol, installed on the
// maintained Exec fake. Responses are built from the CLI's checked-in schemas;
// it simulates fleet intent, operations and local profiles, not RPC call order.
type extendedCLI struct {
	t             *testing.T
	schemaDir     string
	prefix        string
	run           string
	pool          string
	poolPaused    bool
	poolMax       int
	stalePool     *bool
	orphan        bool
	hosts         map[string]map[string]any
	tokens        map[string]map[string]any
	keys          map[string]map[string]any
	profiles      map[string]map[string]any
	current       *string
	operationLive bool
	mutated       map[string]bool
	injected      string
	cancel        context.CancelFunc
	privateDirs   []string
}

func extendedContext(t *testing.T, fault string) (*harness.Context, *infra.Services, *extendedCLI, scenarios.CLIExtendedOptions) {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	require.True(t, ok)
	repo := filepath.Clean(filepath.Join(filepath.Dir(here), "../../.."))
	schemaDir := filepath.Join(repo, "cli/cucinactl/schemas")
	if path := os.Getenv("CUCINA_CLI_RESULT_SCHEMA"); path != "" {
		schemaDir = filepath.Dir(path)
	}
	clock := fakes.NewClock(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))
	ex := fakes.NewExec(clock, fakes.NewRand(20))
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	run := "synthetic-cli-run"
	prefix := scenarios.CLIExtendedFixturePrefix(run)
	f := &extendedCLI{t: t, schemaDir: schemaDir, prefix: prefix, run: run, pool: prefix + "-pool", poolPaused: true,
		hosts: map[string]map[string]any{}, tokens: map[string]map[string]any{}, keys: map[string]map[string]any{},
		profiles: map[string]map[string]any{}, operationLive: true, mutated: map[string]bool{}, injected: fault, cancel: cancel}
	f.hosts["FIXTURE123"] = map[string]any{"serial": "FIXTURE123", "name": prefix + "-host", "site": prefix,
		"phase": "Cordoned", "approved": true, "cordoned": true, "running_vms": 0,
		"labels": map[string]string{"cucina.sloper.ai/test-run": run, "cucina.sloper.ai/test-fixture": "t20"},
		"vms":    []any{map[string]any{"name": prefix + "-vm", "pool": prefix + "-mac", "state": "stopped", "registered": false}}}
	f.keys["campaign-key"] = map[string]any{"key_id": "campaign-key", "account": "writer", "description": "campaign", "revoked": false}
	f.tokens["campaign-token"] = map[string]any{"id": "campaign-token", "site": "campaign", "revoked": false}
	env := &harness.Env{Name: "contract", RunID: run, RepoDir: repo, ArtifactsDir: t.TempDir(),
		Cucinactl: map[string]string{runtime.GOOS: "cucinactl"}, Safety: harness.Safety{AllowDestructive: true},
		Endpoints: harness.Endpoints{STS: "https://cucina.example.com", Management: "cucina.example.com:8444", RemoteExecution: "grpcs://cucina.example.com:443"},
		Pools:     map[string]string{"linux": "campaign-pool"},
		CLI:       &harness.CLIFixtures{Pool: f.pool, Host: "FIXTURE123", VM: prefix + "-vm", InvocationID: prefix + "-invocation", Operation: "disposable-operation"}}
	c := &harness.Context{Context: ctx, Env: env, Scenario: &harness.Scenario{ID: "T20"}, Result: &harness.Result{RunID: run}, Now: clock.Now,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	svc := &infra.Services{Env: env, Exec: ex}
	c.Services = svc
	ex.Handle("cucinactl", f.runCommand)
	return c, svc, f, scenarios.CLIExtendedOptions{WorkerNode: "fixture-worker", ServiceAccount: "writer", PrivateRoot: t.TempDir()}
}

// schemaExample supplies only required fields with valid empty values. Production
// checks still assert meaningful state changes; empty-but-valid data cannot PASS.
func schemaExample(t *testing.T, root, rule map[string]any) any {
	t.Helper()
	if ref, ok := rule["$ref"].(string); ok {
		v := any(root)
		for _, key := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			v = v.(map[string]any)[key]
		}
		return schemaExample(t, root, v.(map[string]any))
	}
	if v, ok := rule["const"]; ok {
		return v
	}
	if v, ok := rule["enum"].([]any); ok {
		return v[0]
	}
	if _, ok := rule["anyOf"]; ok {
		return nil
	}
	if _, ok := rule["type"].([]any); ok {
		return nil
	}
	switch rule["type"] {
	case "object":
		out := map[string]any{}
		props, _ := rule["properties"].(map[string]any)
		fields, _ := rule["required"].([]any)
		for _, field := range fields {
			out[field.(string)] = schemaExample(t, root, props[field.(string)].(map[string]any))
		}
		return out
	case "array":
		return []any{}
	case "integer", "number":
		return 0
	case "boolean":
		return false
	case "string":
		return ""
	}
	return nil
}

func (f *extendedCLI) document(schema string, values map[string]any) map[string]any {
	b, err := os.ReadFile(filepath.Join(f.schemaDir, schema+".v1.schema.json"))
	require.NoError(f.t, err)
	var rule map[string]any
	require.NoError(f.t, json.Unmarshal(b, &rule))
	out := schemaExample(f.t, rule, rule).(map[string]any)
	for k, v := range values {
		out[k] = v
	}
	return out
}

func (f *extendedCLI) response(schema string, values map[string]any) ports.ExecResult {
	out := f.document(schema, values)
	if f.injected == "missing schema" && schema == "pool-floor" {
		delete(out, "schema")
	}
	b, err := json.Marshal(out)
	require.NoError(f.t, err)
	if f.injected == "missing host cordon" && schema == "host-list" {
		var copy map[string]any
		require.NoError(f.t, json.Unmarshal(b, &copy))
		for _, host := range copy["hosts"].([]any) {
			delete(host.(map[string]any), "cordoned")
		}
		b, err = json.Marshal(copy)
		require.NoError(f.t, err)
	}
	return ports.ExecResult{Stdout: append(b, '\n')}
}

func (f *extendedCLI) ack(action, target string) ports.ExecResult {
	return f.response("result", map[string]any{"ok": true, "action": action, "target": target, "message": ""})
}

func cliFlag(args []string, key string) string {
	for i, arg := range args {
		if arg == key && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func cliValues(m map[string]map[string]any) []any {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := []any{}
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}

func (f *extendedCLI) runCommand(ctx context.Context, cmd ports.Command) (ports.ExecResult, error) {
	if ctx.Err() != nil {
		return ports.ExecResult{}, ctx.Err()
	}
	args := cmd.Args
	require.NotEmpty(f.t, args)
	if f.injected == "blocked command" && args[0] == "diag" {
		<-ctx.Done()
		return ports.ExecResult{}, ctx.Err()
	}
	if f.injected == "sensitive failure" && args[0] == "diag" {
		return ports.ExecResult{ExitCode: 6, Stdout: []byte("private-output-marker"), Stderr: []byte("private-output-marker")}, errors.New("private-output-marker")
	}
	if f.injected == "oversize output" && args[0] == "workers" {
		return ports.ExecResult{Stdout: bytes.Repeat([]byte("x"), 4<<20+1)}, nil
	}
	switch args[0] {
	case "pools":
		switch args[1] {
		case "describe":
			paused := f.poolPaused
			if f.stalePool != nil {
				paused, f.stalePool = *f.stalePool, nil
			}
			pool := map[string]any{"name": f.pool, "provider": "ec2", "max": f.poolMax, "min_running": 0, "paused": paused,
				"desired": 0, "launching": 0, "registered": 0, "busy": 0, "idle": 0, "draining": 0, "stopped": 0}
			if f.injected == "missing pool maximum" {
				delete(pool, "max")
			}
			if f.injected == "missing pool paused" {
				delete(pool, "paused")
			}
			return f.response("pool-describe", map[string]any{
				"pool":    pool,
				"spec":    map[string]any{"instanceNames": []string{f.prefix}, "capacity": map[string]int{"max": f.poolMax, "minRunning": 0}},
				"workers": []any{},
			}), nil
		case "list":
			pools := []any{}
			if f.injected == "host matches campaign pool" {
				pools = append(pools, map[string]any{"name": "campaign-macos", "provider": "tart", "max": 2})
			}
			return f.response("pool-list", map[string]any{"pools": pools}), nil
		case "scale-floor":
			require.Equal(f.t, "0", cliFlag(args, "--min"), "T20 must never cause standing worker cost")
			f.mutated["pool:"+args[2]] = true
			return f.response("pool-floor", map[string]any{"pool": args[2], "min_running": 0, "expires_at": nil}), nil
		case "cordon":
			f.mutated["pool:"+args[2]] = true
			if f.injected == "stale pool observation" {
				prev := f.poolPaused
				f.stalePool = &prev
			}
			if f.injected != "cordon no effect" {
				f.poolPaused = !slices.Contains(args, "--undo")
			}
			if f.injected == "cancellation" && slices.Contains(args, "--undo") {
				f.cancel()
				f.injected = ""
			}
			if !slices.Contains(args, "--undo") {
				return f.ack("cordon pool", args[2]), nil
			}
			return f.ack("uncordon pool", args[2]), nil
		case "gc":
			dry := slices.Contains(args, "--dry-run")
			deleted := []string{}
			if f.orphan {
				deleted = []string{"volume unexpected-fixture-resource"}
			}
			if !dry {
				f.mutated["gc:"+args[2]] = true
			}
			return f.response("pool-gc", map[string]any{"pool": args[2], "dry_run": dry, "deleted": deleted, "errors": []string{}}), nil
		}
	case "workers":
		return f.response("log", map[string]any{"text": "synthetic worker record\n"}), nil
	case "hosts":
		switch args[1] {
		case "list":
			return f.response("host-list", map[string]any{"hosts": cliValues(f.hosts)}), nil
		case "register":
			serial := args[2]
			require.NotContains(f.t, f.hosts, serial)
			f.hosts[serial] = map[string]any{"serial": serial, "name": strings.ToLower(serial), "site": cliFlag(args, "--site"),
				"labels": map[string]string{"cucina.sloper.ai/test-run": f.run}, "approved": true, "cordoned": false, "running_vms": 0, "vms": []any{}}
			f.mutated["registered:"+serial] = true
			if f.injected == "lost registration response" {
				return ports.ExecResult{}, errors.New("lost response")
			}
			return f.response("host-register", map[string]any{"registered": []string{serial}, "already_present": []string{}}), nil
		case "approve":
			f.hosts[args[2]]["approved"] = true
			return f.ack("approve host", args[2]), nil
		case "remove":
			if f.injected == "cleanup failure" {
				return ports.ExecResult{ExitCode: 6}, nil
			}
			delete(f.hosts, args[2])
			return f.ack("remove host", args[2]), nil
		case "enroll-token":
			switch args[2] {
			case "list":
				return f.response("enroll-token-list", map[string]any{"tokens": cliValues(f.tokens)}), nil
			case "create":
				id := "new-token"
				if f.injected == "existing token ID" {
					id = "campaign-token"
				} else {
					f.tokens[id] = map[string]any{"id": id, "site": cliFlag(args, "--site"), "description": cliFlag(args, "--description"), "revoked": false}
				}
				if f.injected == "lost token response" {
					return ports.ExecResult{}, errors.New("lost response")
				}
				return f.response("enroll-token", map[string]any{"id": id, "token": "private-token-marker", "expires_at": "2026-10-02T12:10:00Z"}), nil
			case "revoke":
				f.tokens[args[3]]["revoked"] = true
				f.mutated["token:"+args[3]] = true
				return f.ack("revoke enrollment token", args[3]), nil
			}
		case "drain":
			f.hosts[args[2]]["cordoned"] = true
			f.mutated["host:"+args[2]] = true
			return f.ack("drain host", args[2]), nil
		case "uncordon":
			f.hosts[args[2]]["cordoned"] = false
			f.mutated["host:"+args[2]] = true
			return f.ack("uncordon host", args[2]), nil
		case "re-image":
			f.mutated["vm:"+cliFlag(args, "--vm")] = true
			return f.ack("re-image host", args[2]), nil
		case "diag":
			return f.download(args, false), nil
		}
	case "ops":
		op := map[string]any{"name": "disposable-operation", "stage": "executing", "invocation_id": f.prefix + "-invocation"}
		switch args[1] {
		case "list":
			ops := []any{}
			if f.operationLive {
				ops = append(ops, op)
			}
			return f.response("operation-list", map[string]any{"operations": ops, "next_page_token": ""}), nil
		case "watch":
			if f.injected == "empty watch" {
				return ports.ExecResult{}, nil
			}
			if f.injected == "reconnection only" {
				return f.response("operation-event", map[string]any{"kind": "reconnecting", "operation": nil}), nil
			}
			if f.injected == "foreign operation" {
				op["invocation_id"] = "campaign-build"
			}
			return f.response("operation-event", map[string]any{"kind": "added", "operation": op}), nil
		case "kill":
			require.Equal(f.t, "disposable-operation", args[2])
			require.NotContains(f.t, args, "--queue-without-workers", "shared queue kills are unsafe")
			f.operationLive = false
			f.mutated["operation:"+args[2]] = true
			return f.ack("kill", "operation "+args[2]), nil
		}
	case "keys":
		switch args[1] {
		case "list":
			return f.response("service-key-list", map[string]any{"keys": cliValues(f.keys)}), nil
		case "create":
			id := "new-key"
			if f.injected == "existing service key ID" {
				id = "campaign-key"
			} else {
				f.keys[id] = map[string]any{"key_id": id, "account": "writer", "description": cliFlag(args, "--description"), "revoked": false}
			}
			if f.injected == "lost key response" {
				return ports.ExecResult{}, errors.New("lost response")
			}
			return f.response("service-key", map[string]any{"key_id": id, "account": "writer", "key": "private-key-marker"}), nil
		case "revoke":
			if f.injected != "key revoke no effect" {
				f.keys[args[2]]["revoked"] = true
			}
			f.mutated["key:"+args[2]] = true
			return f.ack("revoke service key", args[2]), nil
		}
	case "diag":
		return f.download(args, true), nil
	case "login", "config", "logout", "credential-helper":
		return f.local(cmd), nil
	}
	return ports.ExecResult{ExitCode: 2}, nil
}

func (f *extendedCLI) download(args []string, archive bool) ports.ExecResult {
	path := cliFlag(args, "--file")
	data := []byte("synthetic host diagnostics")
	if archive {
		var b bytes.Buffer
		gz := gzip.NewWriter(&b)
		tarw := tar.NewWriter(gz)
		body := []byte(`{"version":"test","protocol":"1.0"}`)
		require.NoError(f.t, tarw.WriteHeader(&tar.Header{Name: "cucina-support/version.json", Mode: 0o600, Size: int64(len(body))}))
		_, err := tarw.Write(body)
		require.NoError(f.t, err)
		require.NoError(f.t, tarw.Close())
		require.NoError(f.t, gz.Close())
		data = b.Bytes()
		if f.injected == "invalid archive" {
			data = []byte("not a gzip archive")
		}
	}
	require.NoError(f.t, os.WriteFile(path, data, 0o600))
	return f.response("file", map[string]any{"path": path, "bytes": len(data)})
}

func (f *extendedCLI) local(cmd ports.Command) ports.ExecResult {
	dir := ""
	for _, env := range cmd.Env {
		if value, ok := strings.CutPrefix(env, "CUCINA_CONFIG_DIR="); ok {
			dir = value
		}
	}
	require.NotEmpty(f.t, dir, "local commands must never affect the caller's profiles")
	f.privateDirs = append(f.privateDirs, dir)
	args := cmd.Args
	switch args[0] {
	case "login":
		require.Equal(f.t, "-", cliFlag(args, "--key"))
		require.Equal(f.t, "file", cliFlag(args, "--credential-store"))
		require.Equal(f.t, "private-key-marker", string(cmd.Stdin))
		require.NotContains(f.t, strings.Join(args, " "), "private-key-marker", "secrets belong on stdin, never argv")
		name := cliFlag(args, "--profile")
		f.profiles[name] = map[string]any{"name": name, "logged_in": true, "credential_store": "file", "instance_name": "main"}
		f.current = &name
		return f.response("login", map[string]any{"profile": name, "method": "service-key", "subject": "sa:writer"})
	case "config":
		switch args[1] {
		case "path":
			return f.response("path", map[string]any{"path": dir})
		case "profiles":
			return f.response("config", map[string]any{"config_dir": dir, "current_profile": f.current, "profiles": cliValues(f.profiles)})
		case "use":
			name := args[2]
			f.current = &name
			return f.ack("use profile", name)
		case "set":
			key := strings.ReplaceAll(args[2], "-", "_")
			if key == "ca_file" && args[3] == "" {
				f.profiles[*f.current][key] = nil
			} else {
				f.profiles[*f.current][key] = args[3]
			}
			return f.ack("set", *f.current+"."+args[2])
		case "delete":
			delete(f.profiles, args[2])
			return f.ack("delete profile", args[2])
		}
	case "logout":
		if slices.Contains(args, "--all") {
			names := make([]string, 0, len(f.profiles))
			for name := range f.profiles {
				names = append(names, name)
				delete(f.profiles, name)
			}
			slices.Sort(names)
			f.current = nil
			return f.ack("logout", strings.Join(names, ","))
		}
		name := cliFlag(args, "--profile")
		f.profiles[name]["logged_in"] = false
		return f.ack("logout", name)
	case "credential-helper":
		target := filepath.Join(cliFlag(args, "--dir"), "cucina-credential-helper")
		require.NoError(f.t, os.MkdirAll(filepath.Dir(target), 0o700))
		require.NoError(f.t, os.WriteFile(target, []byte("synthetic helper"), 0o700))
		return f.ack("install credential helper", target)
	}
	return ports.ExecResult{ExitCode: 2}
}

// Guards: T20/R-CLI-3, R-TEST-8d and §12 — evidence comes from observed CLI state,
// fixtures isolate mutations, cleanup survives failures/cancellation, and no
// credentials/diagnostic contents enter report output. The fake is deliberately
// stateful so success-shaped no-ops, malformed replies and reconnection messages
// cannot masquerade as acceptance results.
func TestCLIExtendedEvidenceAndCleanup(t *testing.T) {
	for _, tc := range []struct {
		name string
		want harness.Status
	}{
		{"complete", harness.StatusPass}, {"missing fixtures", harness.StatusSkip}, {"destructive disabled", harness.StatusSkip},
		{"campaign pool alias", harness.StatusSkip}, {"enabled pool", harness.StatusSkip}, {"host matches campaign pool", harness.StatusSkip},
		{"cordon no effect", harness.StatusFail}, {"unexpected orphan", harness.StatusFail}, {"missing schema", harness.StatusFail},
		{"empty watch", harness.StatusFail}, {"reconnection only", harness.StatusFail}, {"foreign operation", harness.StatusFail},
		{"sensitive failure", harness.StatusFail}, {"oversize output", harness.StatusFail}, {"invalid archive", harness.StatusFail},
		{"lost registration response", harness.StatusFail}, {"cleanup failure", harness.StatusFail}, {"cancellation", harness.StatusFail},
		{"existing service key ID", harness.StatusFail}, {"existing token ID", harness.StatusFail},
		{"missing pool maximum", harness.StatusFail}, {"missing pool paused", harness.StatusFail}, {"missing host cordon", harness.StatusFail},
		{"lost token response", harness.StatusFail}, {"lost key response", harness.StatusFail},
		{"key revoke no effect", harness.StatusFail}, {"stale pool observation", harness.StatusPass}, {"blocked command", harness.StatusFail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c, svc, f, opts := extendedContext(t, tc.name)
				switch tc.name {
				case "missing fixtures":
					c.Env.CLI = nil
				case "destructive disabled":
					c.Env.Safety.AllowDestructive = false
				case "campaign pool alias":
					c.Env.Pools["linux"] = f.pool
				case "enabled pool":
					f.poolMax = 1
				case "unexpected orphan":
					f.orphan = true
				}
				err := scenarios.RunCLIExtended(c, svc, opts)
				require.Equal(t, tc.want, harness.Classify(err), "%v", err)
				require.True(t, f.poolPaused, "pool cordon must be restored even on failure")
				require.Equal(t, true, f.hosts["FIXTURE123"]["cordoned"], "host cordon must be restored")
				require.Equal(t, false, f.keys["campaign-key"]["revoked"], "campaign credential must survive")
				require.Equal(t, false, f.tokens["campaign-token"]["revoked"], "existing enrollment token must survive")
				require.NotContains(t, f.mutated, "pool:campaign-pool")
				for _, dir := range f.privateDirs {
					_, err := os.Stat(dir)
					require.ErrorIs(t, err, os.ErrNotExist, "private artifacts/configs must be removed")
				}
				for id, key := range f.keys {
					if id != "campaign-key" {
						if tc.name == "key revoke no effect" {
							require.False(t, key["revoked"].(bool))
						} else {
							require.Equal(t, true, key["revoked"], "new profile key must be revoked")
						}
					}
				}
				for id, token := range f.tokens {
					if id != "campaign-token" {
						require.Equal(t, true, token["revoked"], "new enrollment token must be revoked")
					}
				}
				if tc.name != "cleanup failure" {
					require.Len(t, f.hosts, 1, "synthetic host must be removed")
				}
				if tc.name == "unexpected orphan" {
					require.NotContains(t, f.mutated, "gc:"+f.pool, "nonempty dry-run must never lead to deletion")
				}
				if tc.name == "campaign pool alias" || tc.name == "enabled pool" || tc.name == "missing pool maximum" || tc.name == "missing pool paused" {
					require.NotContains(t, f.mutated, "pool:"+f.pool)
				}
				if tc.name == "host matches campaign pool" {
					require.NotContains(t, f.mutated, "host:FIXTURE123")
				}
				b, err := json.Marshal(c.Result)
				require.NoError(t, err)
				for _, secret := range []string{"private-key-marker", "private-token-marker", "private-output-marker", "synthetic host diagnostics"} {
					require.NotContains(t, string(b), secret, "runtime reports cannot disclose command data")
				}
				passed, failed, skipped := 0, 0, 0
				for _, check := range c.Result.Checks {
					if check.Skipped != "" {
						skipped++
						require.False(t, check.Pass)
					} else if check.Pass {
						passed++
					} else {
						failed++
					}
				}
				switch tc.want {
				case harness.StatusPass:
					require.Zero(t, failed)
					require.Zero(t, skipped)
					require.GreaterOrEqual(t, passed, 26)
				case harness.StatusSkip:
					require.Zero(t, failed)
					require.Positive(t, skipped)
				case harness.StatusFail:
					require.Positive(t, failed)
				}
			})
		})
	}
}
