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
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sloper-ai/cucina/test/e2e/harness"
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
	case "report":
		err = reportCmd(os.Args[2:])
	case "redact-check":
		err = redactCheck(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: e2e list | env --name N --base FILE --env FILE | report --env N [--out FILE] | redact-check FILE...")
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

// tofuOutputs reads `tofu output -json` (values wrapped in {"value": …}) or a
// flat JSON object.
func tofuOutputs(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := map[string]any{}
	for k, v := range raw {
		if m, ok := v.(map[string]any); ok {
			if inner, ok := m["value"]; ok {
				v = inner
			}
		}
		out[k] = v
	}
	return out, nil
}

func s(m map[string]any, k string) string {
	v, _ := m[k].(string)
	return v
}

// envCmd writes the AWS campaign descriptor from the aws agent's outputs
// (~/.config/cucina/aws-e2e/{base,env}-outputs.json) plus flags.
func envCmd(args []string) error {
	fs := flag.NewFlagSet("env", flag.ExitOnError)
	name := fs.String("name", "aws-e2e", "descriptor name")
	base := fs.String("base", "", "base outputs JSON")
	envOut := fs.String("env", "", "env outputs JSON")
	repo := fs.String("repo", ".", "Cucina checkout on the dev Mac")
	runID := fs.String("run-id", os.Getenv("CUCINA_RUN_ID"), "cucina:run tag value")
	expires := fs.String("expires", os.Getenv("CUCINA_EXPIRES"), "cucina:expires tag value")
	artifacts := fs.String("artifacts", filepath.Join(os.Getenv("CUCINA_DEV_STORAGE"), "e2e", "runs"), "raw artifacts directory")
	abseilCommit := fs.String("abseil-commit", "2065f4ded0558c6f89fee67c8e5228feb4eb960e", "commit of the Abseil tag")
	_ = fs.Parse(args)
	b, err := tofuOutputs(*base)
	if err != nil {
		return err
	}
	e, err := tofuOutputs(*envOut)
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	cfg := filepath.Join(home, ".config", "cucina", "aws-e2e")
	pub := s(e, "k3s_public_ip")
	repoAbs, _ := filepath.Abs(*repo)
	d := harness.Env{
		Name: *name, Kind: harness.EnvAWS, RunID: *runID,
		Capabilities: []harness.Requirement{harness.RequiresAWS, harness.RequiresKubernetes, harness.RequiresPrometheus, harness.RequiresLinuxClient,
			harness.RequiresDestructive, harness.RequiresCucinactl, harness.RequiresMacHost, harness.RequiresIdP},
		Kubernetes: &harness.KubeEnv{Kubeconfig: filepath.Join(cfg, "kubeconfig"), Namespace: "cucina", Release: "cucina",
			Chart: filepath.Join(repoAbs, "charts", "cucina"), ValuesFiles: []string{filepath.Join(cfg, "values-campaign.yaml")}},
		Endpoints: harness.Endpoints{RemoteExecution: "grpcs://" + pub + ":443", InstanceName: "main", STS: "https://" + pub + ":8443",
			Management: pub + ":8444", Host: pub + ":8446", Prometheus: "http://127.0.0.1:9090", CAFile: filepath.Join(cfg, "ca.pem"), PublicHost: pub,
			WorkerListener: s(e, "k3s_private_ip") + ":8981"},
		AWS: &harness.AWSEnv{Profile: "default", Region: "us-west-1",
			Tags:       map[string]string{"cucina:env": "e2e", "cucina:run": *runID, "cucina:expires": *expires},
			ClusterTag: "cucina", K3sInstanceID: s(e, "k3s_instance_id"), WorkerSecurityGroup: s(b, "sg_workers"),
			SweepScript: filepath.Join(repoAbs, "deploy", "aws-e2e", "scripts", "sweep.sh")},
		Clients: map[string]harness.ClientEnv{},
		DevMac: &harness.DevMacEnv{WorkDir: filepath.Join(*artifacts, *runID, "dev-mac"), TartHome: os.Getenv("TART_HOME"), Bazel: bazeliskPath(repoAbs),
			BaseImage: "ghcr.io/cirruslabs/macos-golden-gate-xcode:27", MDMKit: filepath.Join(repoAbs, "macos", "pkg", "scripts")},
		IdP:       &harness.IdPEnv{MockOAuth2URL: "http://127.0.0.1:18080", ClientID: "cucina-e2e", HostedDomain: "example.com"},
		Cucinactl: map[string]string{},
		Pools:     map[string]string{"linux": "linux-x86-64", "windows": "windows-x86-64", "linux-arm64": "linux-aarch64", "macos": "macos-arm64-xcode27.0"},
		Abseil:    harness.AbseilPin{Tag: "20260817.0", Commit: *abseilCommit},
		Safety:    harness.Safety{MaxSpendUSD: harness.DefaultBudgetUSD, MaxInstances: 16, AllowDestructive: true},
		Secrets: harness.Secrets{ServiceKeyFile: filepath.Join(cfg, "e2e-writer.key"), ReadOnlyKeyFile: filepath.Join(cfg, "e2e-readonly.key"),
			AdminKeyFile: filepath.Join(cfg, "break-glass.key")},
		ArtifactsDir: *artifacts, RepoDir: repoAbs,
	}
	if id := s(e, "linux_client_instance_id"); id != "" {
		d.Clients["linux-client"] = harness.ClientEnv{OS: "linux", InstanceID: id, User: "ubuntu", WorkDir: "/home/ubuntu/e2e"}
	}
	if id := s(e, "windows_client_instance_id"); id != "" {
		d.Clients["windows-client"] = harness.ClientEnv{OS: "windows", InstanceID: id, WorkDir: `C:\e2e`}
		d.Capabilities = append(d.Capabilities, harness.RequiresWindowsClient)
	}
	out, err := harness.DescriptorPath(*name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return err
	}
	bs, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, append(bs, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Println("wrote", out, "(edit cucinactl paths, images, cucina repo pin, valuesFiles before running)")
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
