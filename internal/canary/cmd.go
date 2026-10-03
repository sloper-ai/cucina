// SPDX-License-Identifier: FSL-1.1-ALv2

package canary

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Options are the canary CLI flags.
type Options struct {
	STSURL     string
	KeyFile    string
	Endpoint   string
	Instance   string
	CAFile     string
	ServerName string
	Pool       string
	Platform   map[string]string
	Timeout    time.Duration
	ReportTo   string
	Output     string
}

// Probe builds the probe for a kind. Without --endpoint the client endpoint
// comes from the STS discovery document.
func (o Options) Probe(ctx context.Context, kind string) (*Probe, error) {
	ep := Endpoint{Target: o.Endpoint, InstanceName: o.Instance, CAFile: o.CAFile, ServerName: o.ServerName}
	var ts TokenSource
	if o.STSURL != "" {
		tlsCfg, err := ep.TLSConfig()
		if err != nil {
			return nil, err
		}
		sts := &STS{URL: o.STSURL, KeyFile: o.KeyFile, HTTP: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg}}}
		if ep.Target == "" || ep.InstanceName == "" {
			d, err := sts.Discover(ctx)
			if err != nil {
				return nil, fmt.Errorf("discovery: %w", err)
			}
			if ep.Target == "" {
				ep.Target = d.Endpoints.RemoteExecution
			}
			if ep.InstanceName == "" {
				ep.InstanceName = d.Endpoints.InstanceName
			}
		}
		ts = sts
	}
	if ep.Target == "" {
		return nil, fmt.Errorf("no endpoint: pass --endpoint or --sts-url")
	}
	if kind == KindExec && len(o.Platform) == 0 {
		return nil, fmt.Errorf("exec canary needs --platform (the pool's runner properties)")
	}
	return &Probe{Kind: kind, Endpoint: ep, Tokens: ts, Pool: o.Pool, Platform: o.Platform, Timeout: o.Timeout}, nil
}

// Main runs one probe, prints the result and pushes it if asked. It returns
// the process exit code: 0 success, 1 probe failure, 2 usage/config error.
func Main(ctx context.Context, kind string, o Options, stdout, stderr io.Writer) int {
	p, err := o.Probe(ctx, kind)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "canary:", err)
		return 2
	}
	r := p.Run(ctx)
	if o.Output == "json" {
		b, _ := json.Marshal(r)
		_, _ = fmt.Fprintln(stdout, string(b))
	} else {
		status := "OK"
		if !r.Success {
			status = "FAILED: " + r.Error
		}
		_, _ = fmt.Fprintf(stdout, "%s canary %s (%s)\n", r.Kind, status, r.Duration.Round(time.Millisecond))
		for _, s := range r.Steps {
			_, _ = fmt.Fprintf(stdout, "  %-18s %8s %s\n", s.Name, s.Duration.Round(time.Millisecond), s.Error)
		}
	}
	if o.ReportTo != "" {
		if err := Push(ctx, o.ReportTo, r, nil); err != nil {
			_, _ = fmt.Fprintln(stderr, "canary:", err)
		}
	}
	if !r.Success {
		return 1
	}
	return 0
}

// Command returns `canary cache|exec` for cucina-controller. Flags default
// from CUCINA_CANARY_* environment variables for one-shot tools and helm tests;
// scheduled probes reuse this package in-process, not a separate CronJob.
func Command() *cobra.Command {
	var o Options
	var platform []string
	root := &cobra.Command{
		Use:   "canary",
		Short: "Run a synthetic canary (cache: AC/CAS round trip + token mint/verify; exec: tiny action per pool)",
	}
	pf := root.PersistentFlags()
	pf.StringVar(&o.STSURL, "sts-url", os.Getenv("CUCINA_CANARY_STS_URL"), "STS base URL (discovery, token exchange, JWKS)")
	pf.StringVar(&o.KeyFile, "key-file", envOr("CUCINA_CANARY_KEY_FILE", "/var/run/secrets/cucina/canary/key"), "service-account key file")
	pf.StringVar(&o.Endpoint, "endpoint", os.Getenv("CUCINA_CANARY_ENDPOINT"), "client endpoint (grpcs://host:port); default from discovery")
	pf.StringVar(&o.Instance, "instance", os.Getenv("CUCINA_CANARY_INSTANCE"), "REAPI instance name; default from discovery")
	pf.StringVar(&o.CAFile, "ca-file", os.Getenv("CUCINA_CANARY_CA_FILE"), "PEM CA bundle for the endpoint and the STS")
	pf.StringVar(&o.ServerName, "server-name", os.Getenv("CUCINA_CANARY_SERVER_NAME"), "TLS server name override")
	pf.DurationVar(&o.Timeout, "timeout", 0, "probe timeout (default 2m cache, 15m exec)")
	pf.StringVar(&o.ReportTo, "report-to", os.Getenv("CUCINA_CANARY_REPORT_TO"), "controller URL receiving the result (e.g. http://cucina-controller:9090/canary/results)")
	pf.StringVar(&o.Output, "output", "text", "text or json")
	run := func(kind string) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, _ []string) error {
			if len(platform) > 0 {
				o.Platform = map[string]string{}
				for _, kv := range platform {
					k, v, ok := strings.Cut(kv, "=")
					if !ok {
						return fmt.Errorf("--platform %q: want key=value", kv)
					}
					o.Platform[k] = v
				}
			}
			if code := Main(cmd.Context(), kind, o, cmd.OutOrStdout(), cmd.ErrOrStderr()); code != 0 {
				return &ExitError{Code: code}
			}
			return nil
		}
	}
	cache := &cobra.Command{Use: "cache", Short: "AC/CAS round trip through the client endpoint plus token mint/verify (starts no workers)", Args: cobra.NoArgs, RunE: run(KindCache)}
	exec := &cobra.Command{Use: "exec", Short: "Run a tiny uncached action on one pool (may scale it from zero)", Args: cobra.NoArgs, RunE: run(KindExec)}
	exec.Flags().StringVar(&o.Pool, "pool", os.Getenv("CUCINA_CANARY_POOL"), "pool name (verified against the executed worker identity)")
	exec.Flags().StringSliceVar(&platform, "platform", nil, "runner property key=value (repeat; exact set of the pool's runner)")
	root.AddCommand(cache, exec)
	return root
}

// ExitError carries the canary's exit code through cobra.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("canary failed (exit %d)", e.Code) }

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
