// SPDX-License-Identifier: FSL-1.1-ALv2

// Command e2e is the campaign's companion CLI:
//
//	e2e list                                   scenarios, requirements, cost classes
//	e2e env --name aws-e2e --base B --env E    write ~/.config/cucina/e2e/<name>.json from the OpenTofu outputs
//	e2e report --env aws-e2e [--out FILE]      render docs/reports/e2e-<date>.md (redacted) from the results
//	e2e redact-check FILE...                   fail if a report still contains environment identifiers
//
// Run from the repository root: go run ./test/e2e/cmd/e2e <command> …
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/sloper-ai/cucina/test/e2e/harness"
	"github.com/sloper-ai/cucina/test/e2e/infra"
	"github.com/sloper-ai/cucina/test/e2e/report"
	"github.com/sloper-ai/cucina/test/e2e/scenarios"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "list":
		err = list()
	case "env":
		err = envCmd(os.Args[2:])
	case "check":
		err = checkCmd(os.Args[2:])
	case "report":
		err = reportCmd(os.Args[2:])
	case "redact-check":
		err = redactCheck(os.Args[2:])
	default:
		usage()
	}
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: e2e list | env [--help] | check --env N --id ID[,ID...] | report --env N [--out FILE] | redact-check FILE...")
	os.Exit(2)
}

func list() error {
	reg := harness.NewRegistry()
	scenarios.Register(reg)
	for _, id := range reg.IDs() {
		s, _ := reg.Get(id)
		req := make([]string, len(s.Requires))
		for i, r := range s.Requires {
			req[i] = string(r)
		}
		fmt.Printf("%-16s %-6s $%-5.1f %-8s %-50s %s\n", s.ID, s.Cost, s.EstimateUSD, s.Timeout, strings.Join(req, ","), s.Title)
	}
	return nil
}

// envCmd only writes a private descriptor. The lead creates infrastructure,
// Helm values and credentials separately; capabilities are never evidence.
func envCmd(args []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	cfg := filepath.Join(home, ".config", "cucina", "aws-e2e")
	fs := flag.NewFlagSet("env", flag.ContinueOnError)
	name := fs.String("name", "aws-e2e", "descriptor name")
	base := fs.String("base", filepath.Join(cfg, "base-outputs.json"), "base outputs JSON")
	envOut := fs.String("env", filepath.Join(cfg, "env-outputs.json"), "env outputs JSON")
	repo := fs.String("repo", ".", "Cucina checkout on the dev Mac")
	state := fs.String("state-dir", cfg, "private directory holding kubeconfig, CA, keys and Helm values")
	runID := fs.String("run-id", os.Getenv("CUCINA_RUN_ID"), "campaign tag (defaults to outputs when unset)")
	expires := fs.String("expires", os.Getenv("CUCINA_EXPIRES"), "expiry tag (must match outputs)")
	storage := os.Getenv("CUCINA_DEV_STORAGE")
	artifactsDefault := ""
	if storage != "" {
		artifactsDefault = filepath.Join(storage, "e2e/runs")
	}
	artifacts := fs.String("artifacts", artifactsDefault, "raw artifacts directory (required without CUCINA_DEV_STORAGE)")
	settings := fs.String("settings", "", "private partial descriptor JSON; recursive object merge, arrays replace")
	mac := fs.String("cucinactl-darwin", "", "local path of darwin-arm64 cucinactl")
	linux := fs.String("cucinactl-linux", "", "local path of linux-amd64 cucinactl for upload")
	windows := fs.String("cucinactl-windows", "", "local path of windows-amd64 cucinactl.exe for upload")
	idp := fs.Bool("with-idp", false, "configure mock HTTPS issuer/loopback forward (does not deploy it)")
	destructive := fs.Bool("allow-destructive", false, "allow fault/teardown scenarios on this tagged test environment")
	force := fs.Bool("force", false, "replace an existing descriptor (settings should preserve customizations)")
	var values []string
	fs.Func("values", "Helm values file; repeat in merge order", func(p string) error { values = append(values, p); return nil })
	if err := fs.Parse(args); err != nil {
		return err
	}
	b, err := os.ReadFile(*base)
	if err != nil {
		return err
	}
	e, err := os.ReadFile(*envOut)
	if err != nil {
		return err
	}
	var patch []byte
	if *settings != "" {
		patch, err = os.ReadFile(*settings)
		if err != nil {
			return err
		}
	}
	repoAbs, err := filepath.Abs(*repo)
	if err != nil {
		return err
	}
	bins := map[string]string{}
	for osName, p := range map[string]string{"darwin": *mac, "linux": *linux, "windows": *windows} {
		if p != "" {
			abs, err := filepath.Abs(p)
			if err != nil {
				return err
			}
			bins[osName] = abs
		}
	}
	d, err := harness.NewAWSEnv(b, e, patch, harness.AWSSetup{Name: *name, RunID: *runID, Expires: *expires, RepoDir: repoAbs, StateDir: *state, ArtifactsDir: *artifacts, Bazel: bazeliskPath(repoAbs), Cucinactl: bins, ValuesFiles: values, WithIDP: *idp, AllowDestructive: *destructive})
	if err != nil {
		return err
	}
	out, err := harness.DescriptorPath(*name)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(out); err == nil && !*force {
		return fmt.Errorf("descriptor exists; pass --force to replace it (preserve edits with --settings)")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return err
	}
	bs, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(out), ".descriptor-*.json")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err = f.Write(append(bs, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), out); err != nil {
		return err
	}
	fmt.Println("wrote", out, "(not a readiness assertion; run e2e check --env", *name, "--id T0)")
	return nil
}

// checkCmd checks local inputs, capability declarations and prior scenario
// dependencies without calling AWS, Kubernetes or a credential-bearing CLI.
// READY means offline inputs only, never an acceptance verdict.
func checkCmd(args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	name := fs.String("env", "aws-e2e", "descriptor name or path")
	ids := fs.String("id", "T0", "comma-separated scenarios, or all")
	if err := fs.Parse(args); err != nil {
		return err
	}
	e, err := harness.LoadEnv(*name)
	if err != nil {
		return err
	}
	reg := harness.NewRegistry()
	scenarios.Register(reg)
	prior := map[string]*harness.Result{}
	rs, err := harness.LoadResults(filepath.Join(e.ArtifactsDir, e.RunID, "results"))
	if err != nil {
		return err
	}
	for _, r := range rs {
		if r.RunID == e.RunID {
			prior[r.ID] = r
		}
	}
	runner := harness.Runner{Env: e}
	selected := strings.Split(*ids, ",")
	if *ids == "all" {
		selected = reg.IDs()
	}
	blocked := 0
	for _, id := range selected {
		s, ok := reg.Get(strings.TrimSpace(id))
		if !ok {
			return fmt.Errorf("unknown scenario %q", id)
		}
		if reason := runner.Preflight(s, prior); reason != "" {
			fmt.Printf("%s BLOCKED: %s\n", id, reason)
			blocked++
			continue
		}
		var missing []string
		file := func(label, path string) {
			if path == "" {
				missing = append(missing, label+" not configured")
				return
			}
			if _, err := os.Stat(path); err != nil {
				missing = append(missing, label+": "+err.Error())
			}
		}
		file("repoDir", e.RepoDir)
		for _, req := range s.Requires {
			switch req {
			case harness.RequiresKubernetes:
				file("kubeconfig", e.Kubernetes.Kubeconfig)
				file("chart", e.Kubernetes.Chart)
				for _, v := range e.Kubernetes.ValuesFiles {
					file("Helm values", v)
				}
			case harness.RequiresCucinactl:
				file("local cucinactl", e.Cucinactl[runtime.GOOS])
			case harness.RequiresLinuxClient:
				file("linux cucinactl", e.Cucinactl["linux"])
			case harness.RequiresWindowsClient:
				file("windows cucinactl", e.Cucinactl["windows"])
			case harness.RequiresCrossMatrix:
				file("targets.json", filepath.Join(e.RepoDir, "platforms/targets.json"))
				file("module segment", filepath.Join(e.RepoDir, "tools/xplat/cucina_platforms.MODULE.bazel"))
				if e.DevMac != nil {
					file("Bazelisk", e.DevMac.Bazel)
				}
			case harness.RequiresMacHost:
				file("hostd", e.DevMac.HostdBinary)
				file("hostd config", e.DevMac.HostdConfig)
			case harness.RequiresHostdPkg:
				file("signed pkg", e.DevMac.PkgPath)
				file("upgrade pkg", e.DevMac.UpgradePkgPath)
				file("signer cert", e.DevMac.SignerCert)
			case harness.RequiresIdP:
				file("mock issuer CA", e.IdP.CAFile)
			}
		}
		if s.ID != "T0" && s.ID != "T15" && !strings.HasPrefix(s.ID, "baseline-") && !s.ReadOnly {
			file("Cucina CA", e.Endpoints.CAFile)
			file("writer key", e.Secrets.ServiceKeyFile)
		}
		switch s.ID {
		case "T0":
			if e.Kubernetes != nil && len(e.Kubernetes.BootstrapCLI) > 0 {
				file("post-install CLI bootstrap", e.Kubernetes.BootstrapCLI[0])
			} else if len(missing) == 0 {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				err := (&infra.Services{Env: e}).VerifyLocalProfile(ctx)
				cancel()
				if err != nil {
					missing = append(missing, err.Error())
				}
			}
		case "T1", "T4":
			lane := "linux"
			if s.ID == "T4" {
				lane = "windows"
			}
			p := prior["baseline-"+lane]
			if p == nil || p.Status != harness.StatusPass || p.Metrics["baseline."+lane+"-baseline.build_seconds"].Value <= 0 {
				missing = append(missing, "successful worker-type "+lane+"-baseline measurement required for NFR-P2")
			}
		case "T11":
			file("upgrade values", e.Kubernetes.UpgradeValuesFile)
		case "T12":
			for _, role := range []string{"linux", "windows"} {
				p := e.Pools[role]
				v := e.Images[p]
				if v.Current == "" || v.Next == "" || v.Current == v.Next {
					missing = append(missing, "distinct current/next images required for "+role)
				}
			}
		case "T10e":
			if len(e.Kubernetes.RotateSigningKey) == 0 {
				missing = append(missing, "kubernetes.rotateSigningKey not configured")
			}
		case "T10f":
			file("read-only key", e.Secrets.ReadOnlyKeyFile)
		case "T10h":
			file("host cert", e.Secrets.HostCertFile)
			file("host key", e.Secrets.HostKeyFile)
			file("worker cert", e.Secrets.WorkerCertFile)
			file("worker key", e.Secrets.WorkerKeyFile)
		case "T22":
			if e.Cucina == nil || e.Cucina.Commit == "" || e.Cucina.URL == "" {
				missing = append(missing, "cucina URL and reachable commit required")
			}
		}
		if len(missing) > 0 {
			blocked++
			fmt.Printf("%s BLOCKED: %s\n", id, strings.Join(missing, "; "))
		} else {
			fmt.Printf("%s READY: offline prerequisites only; no live readiness check performed\n", id)
		}
	}
	if blocked > 0 {
		return fmt.Errorf("%d scenarios have unmet prerequisites", blocked)
	}
	return nil
}

// bazeliskPath resolves the real Bazelisk binary (mise shims only resolve
// inside directories with a mise.toml).
func bazeliskPath(repo string) string {
	cmd := exec.Command("mise", "which", "bazelisk")
	cmd.Dir = repo
	if out, err := cmd.Output(); err == nil {
		return strings.TrimSpace(string(out))
	}
	if p, err := exec.LookPath("bazelisk"); err == nil {
		return p
	}
	return ""
}

// literals collects identifiers from the descriptor for exact redaction.
func literals(env *harness.Env) map[string]string {
	lit := map[string]string{}
	add := func(v, name string) {
		if v != "" {
			lit[v] = name
		}
	}
	add(env.Endpoints.PublicHost, "public-endpoint")
	if env.AWS != nil {
		add(env.AWS.K3sInstanceID, "k3s-instance")
		add(env.AWS.WorkerSecurityGroup, "security-group")
		add(env.AWS.IsolationSecurityGroup, "security-group")
		add(env.AWS.ClusterTag, "cluster-tag")
	}
	for name, c := range env.Clients {
		add(c.InstanceID, name+"-instance")
	}
	for pool, img := range env.Images {
		add(img.Current, pool+"-image")
		add(img.Next, pool+"-image")
	}
	if h, err := os.Hostname(); err == nil {
		add(h, "dev-mac-hostname")
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(home, "home")
	}
	delete(lit, "cucina") // a cluster tag of "cucina" is the product name, not an identifier
	return lit
}

func reportCmd(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	envName := fs.String("env", "aws-e2e", "environment descriptor")
	results := fs.String("results", "", "results directory (default <artifactsDir>/<runId>/results)")
	out := fs.String("out", "", "output (default docs/reports/e2e-<date>.md)")
	issues := fs.String("issues", "docs/reports/issues.md", "issues ledger maintained by the lead")
	adr := fs.String("adr", "docs/adr", "ADR directory")
	extra := fs.String("extra-spend", "", "JSON object of spend not attributed to scenarios (item → USD)")
	_ = fs.Parse(args)
	env, err := harness.LoadEnv(*envName)
	if err != nil {
		return err
	}
	if *results == "" {
		*results = filepath.Join(env.ArtifactsDir, env.RunID, "results")
	}
	rs, err := harness.LoadResults(*results)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if *out == "" {
		*out = filepath.Join("docs", "reports", "e2e-"+now.Format("2006-01-02")+".md")
	}
	in := report.Input{Date: now, RunID: env.RunID, Results: rs, BudgetUSD: harness.DefaultBudgetUSD}
	if b, err := os.ReadFile(*issues); err == nil {
		in.Issues = string(b)
	}
	if in.ADRs, err = report.ADRIndex(*adr); err != nil {
		return err
	}
	if *extra != "" {
		b, err := os.ReadFile(*extra)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, &in.ExtraSpend); err != nil {
			return err
		}
	}
	found, err := report.Write(*out, in, report.Redactor{Literals: literals(env)})
	if err != nil {
		return err
	}
	counts := map[string]int{}
	for _, f := range found {
		counts[f.Rule]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s×%d", k, counts[k]))
	}
	fmt.Printf("wrote %s (%d scenarios; redacted: %s)\n", *out, len(rs), strings.Join(parts, ", "))
	return nil
}

func redactCheck(files []string) error {
	bad := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		for _, x := range report.Check(string(b)) {
			bad++
			fmt.Printf("%s:%d: %s: %s\n", f, x.Line, x.Rule, x.Match)
		}
	}
	if bad > 0 {
		return fmt.Errorf("%d environment identifiers found; regenerate the report with `e2e report` (redaction) or fix the file", bad)
	}
	return nil
}
