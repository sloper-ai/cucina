// SPDX-License-Identifier: FSL-1.1-ALv2

// Package infra wires the concrete clients scenarios use from an environment
// descriptor: SSM hosts for the client VMs, the dev Mac as a local host, the
// tag-filtered EC2 inventory, the Prometheus snapshotter, kubectl/helm for
// the release, cucinactl, and the tag-sweep wrapper.
package infra

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"

	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/test/e2e/collect/awsinv"
	"github.com/sloper-ai/cucina/test/e2e/collect/prom"
	"github.com/sloper-ai/cucina/test/e2e/harness"
	"github.com/sloper-ai/cucina/test/e2e/remote"
)

// DevMac is the host name of the machine running the harness.
const DevMac = "dev-mac"

// Services implements harness.Services.
type Services struct {
	Env  *harness.Env
	Log  *slog.Logger
	Kube *Kube
	// Exec replaces only local CLI process execution in deterministic tests.
	// Production leaves it nil and uses os/exec.
	Exec ports.Exec
	// Clock is optional deterministic time for integration-boundary fakes.
	Clock ports.Clock
	// EC2 is nil without the aws capability.
	EC2 *awsinv.Inventory

	mu    sync.Mutex
	hosts map[string]remote.Host
	ssm   remote.SSMAPI
}

// New builds the services for an environment (harness.Runner.NewServices).
func New(ctx context.Context, env *harness.Env) (harness.Services, error) {
	s := &Services{Env: env, Log: slog.Default(), hosts: map[string]remote.Host{}}
	work := filepath.Join(env.ArtifactsDir, env.RunID, "dev-mac-work")
	if env.DevMac != nil && env.DevMac.WorkDir != "" {
		work = env.DevMac.WorkDir
	}
	s.hosts[DevMac] = remote.NewLocal(DevMac, work)
	if env.Kubernetes != nil {
		s.Kube = &Kube{K: *env.Kubernetes}
	}
	if env.AWS != nil && env.Has(harness.RequiresAWS) {
		cfg, err := config.LoadDefaultConfig(ctx, config.WithSharedConfigProfile(env.AWS.Profile), config.WithRegion(env.AWS.Region))
		if err != nil {
			return nil, fmt.Errorf("aws config: %w", err)
		}
		tags := maps.Clone(env.AWS.Tags)
		delete(tags, "cucina:expires")
		tags["cucina:role"] = "worker"
		s.EC2 = &awsinv.Inventory{API: ec2.NewFromConfig(cfg), Tags: tags}
	}
	return s, nil
}

// Close implements harness.Services.
func (s *Services) Close() error { return nil }

// Of returns the Services behind a scenario context.
func Of(c *harness.Context) (*Services, error) {
	s, ok := c.Services.(*Services)
	if !ok || s == nil {
		return nil, errors.New("scenario needs infra.Services (run through test/e2e)")
	}
	return s, nil
}

// Host returns a host by name: "dev-mac" or a client from the descriptor.
func (s *Services) Host(ctx context.Context, name string) (remote.Host, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h, ok := s.hosts[name]; ok {
		return h, nil
	}
	c, ok := s.Env.Clients[name]
	if !ok {
		return nil, fmt.Errorf("no host %q in environment %s", name, s.Env.Name)
	}
	if s.ssm == nil {
		api, err := remote.NewSSMClient(ctx, s.Env.AWS.Profile, s.Env.AWS.Region)
		if err != nil {
			return nil, err
		}
		s.ssm = api
	}
	h, err := remote.NewSSMHost(remote.SSMHostConfig{Name: name, OS: c.OS, InstanceID: c.InstanceID, WorkDir: c.WorkDir,
		Comment: "cucina-e2e " + s.Env.RunID, Profile: s.Env.AWS.Profile, Region: s.Env.AWS.Region}, s.ssm)
	if err != nil {
		return nil, err
	}
	s.hosts[name] = h
	return h, nil
}

// ClientUser is the non-root user Bazel runs as on a Linux client.
func (s *Services) ClientUser(name string) string {
	if c, ok := s.Env.Clients[name]; ok && c.OS == remote.Linux {
		if c.User != "" {
			return c.User
		}
		return "ubuntu"
	}
	return ""
}

// Prom returns a Prometheus client that records every query on c.
func (s *Services) Prom(c *harness.Context) (*prom.Client, error) {
	if s.Env.Endpoints.Prometheus == "" {
		return nil, harness.Skip("no Prometheus endpoint in the environment descriptor")
	}
	return &prom.Client{BaseURL: s.Env.Endpoints.Prometheus, Record: func(q, spec, result string, at time.Time) {
		c.Query(harness.QueryRecord{Query: q, Range: spec, Result: result, At: at})
	}}, nil
}

// ------------------------------------------------------------------ commands

// run executes a local command with a timeout and returns stdout (stderr is
// folded into the error).
func run(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		return out.Bytes(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(redactArgs(args), " "), err, tailString(errb.String(), 2000))
	}
	return out.Bytes(), nil
}

// redactArgs hides values of flags that may carry secrets in error messages.
func redactArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if k, _, ok := strings.Cut(a, "="); ok && (strings.Contains(k, "key") || strings.Contains(k, "token") || strings.Contains(k, "secret")) {
			a = k + "=<redacted>"
		}
		out[i] = a
	}
	return out
}

func tailString(s string, n int) string {
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

// CucinactlPath returns the dev Mac's cucinactl binary.
func (s *Services) CucinactlPath() (string, error) {
	if p := s.Env.Cucinactl[runtime.GOOS]; p != "" {
		return p, nil
	}
	if p, err := exec.LookPath("cucinactl"); err == nil {
		return p, nil
	}
	return "", harness.Skip("no cucinactl binary for %s in the environment descriptor", runtime.GOOS)
}

// Cucinactl runs cucinactl on the dev Mac.
func (s *Services) Cucinactl(ctx context.Context, args ...string) ([]byte, error) {
	bin, err := s.CucinactlPath()
	if err != nil {
		return nil, err
	}
	if s.Exec != nil {
		r, err := s.Exec.Run(ctx, ports.Command{Path: bin, Args: args})
		if err != nil {
			return nil, err
		}
		if r.ExitCode != 0 {
			return r.Stdout, fmt.Errorf("cucinactl exited %d: %s", r.ExitCode, tailString(string(r.Stderr), 2000))
		}
		return r.Stdout, nil
	}
	return run(ctx, nil, bin, args...)
}

// BootstrapLocalProfile invokes an explicitly configured lead-owned setup
// command. Its stdout/stderr are discarded: key-creation tools may print a
// secret once. The resulting profile is checked against this descriptor.
func (s *Services) BootstrapLocalProfile(ctx context.Context) error {
	if s.Env.Kubernetes != nil && len(s.Env.Kubernetes.BootstrapCLI) > 0 {
		args := s.Env.Kubernetes.BootstrapCLI
		if s.Exec != nil {
			r, err := s.Exec.Run(ctx, ports.Command{Path: args[0], Args: args[1:]})
			if err != nil {
				return err
			}
			if r.ExitCode != 0 {
				return fmt.Errorf("local CLI bootstrap exited %d", r.ExitCode)
			}
		} else {
			if err := exec.CommandContext(ctx, args[0], args[1:]...).Run(); err != nil {
				return fmt.Errorf("local CLI bootstrap: %w", err)
			}
		}
	}
	return s.VerifyLocalProfile(ctx)
}

// VerifyLocalProfile requires the operator's already-bootstrapped CLI profile
// to name this descriptor's cluster. It never logs in, reads keys or silently
// queries an ambient profile for another deployment. Bootstrap is lead-owned.
func (s *Services) VerifyLocalProfile(ctx context.Context) error {
	var view struct {
		Profiles []struct {
			Name       string `json:"name"`
			Current    bool   `json:"current"`
			URL        string `json:"url"`
			RE         string `json:"remote_executor"`
			Management string `json:"management"`
			CA         string `json:"ca_file"`
		} `json:"profiles"`
	}
	if err := s.CucinactlJSON(ctx, &view, "config", "view"); err != nil {
		return fmt.Errorf("local CLI profile prerequisite: %w", err)
	}
	selected := os.Getenv("CUCINA_PROFILE")
	for _, p := range view.Profiles {
		if (selected != "" && p.Name == selected) || (selected == "" && p.Current) {
			if strings.TrimRight(p.URL, "/") != strings.TrimRight(s.Env.Endpoints.STS, "/") || p.RE != s.Env.Endpoints.RemoteExecution || p.Management != s.Env.Endpoints.Management || filepath.Clean(p.CA) != filepath.Clean(s.Env.Endpoints.CAFile) {
				return harness.Skip("local CLI profile does not match descriptor STS/REAPI/management/CA; bootstrap and select this campaign's profile before running")
			}
			return nil
		}
	}
	return harness.Skip("no selected local CLI profile; the lead must bootstrap the chart CA/admin credentials and log in before this scenario")
}

// CucinactlJSON runs cucinactl with --output json and decodes the result.
func (s *Services) CucinactlJSON(ctx context.Context, v any, args ...string) error {
	out, err := s.Cucinactl(ctx, append(args, "--output", "json")...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(out, v); err != nil {
		return fmt.Errorf("cucinactl %s: invalid JSON: %w", strings.Join(args, " "), err)
	}
	return nil
}

// ------------------------------------------------------------------ sweep

// SweepResult is the output of deploy/aws-e2e/scripts/sweep.sh --report.
type SweepResult struct {
	Output string `json:"output"`
	Clean  bool   `json:"clean"`
}

// Sweep runs the tag sweep (read-only report mode). It exits non-zero when
// anything tagged with the run is left (aws agent's contract).
func (s *Services) Sweep(ctx context.Context) (SweepResult, error) {
	script := "deploy/aws-e2e/scripts/sweep.sh"
	if s.Env.AWS != nil && s.Env.AWS.SweepScript != "" {
		script = s.Env.AWS.SweepScript
	}
	if !filepath.IsAbs(script) && s.Env.RepoDir != "" {
		script = filepath.Join(s.Env.RepoDir, script)
	}
	if _, err := os.Stat(script); err != nil {
		return SweepResult{}, harness.Skip("tag sweep script not found: %s", script)
	}
	env := []string{"AWS_PROFILE=" + s.Env.AWS.Profile, "AWS_REGION=" + s.Env.AWS.Region, "CUCINA_RUN_ID=" + s.Env.RunID, "AWS_PAGER="}
	cmd := exec.CommandContext(ctx, script, "--report")
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	// sweep.sh: exit 0 clean, 1 leftovers, 2 AWS API error.
	var ee *exec.ExitError
	switch {
	case err == nil:
		return SweepResult{Output: string(out), Clean: true}, nil
	case errors.As(err, &ee) && ee.ExitCode() == 1:
		return SweepResult{Output: string(out), Clean: false}, nil
	default:
		return SweepResult{Output: string(out)}, fmt.Errorf("sweep.sh: %w", err)
	}
}
