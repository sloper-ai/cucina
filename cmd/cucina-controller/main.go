// SPDX-License-Identifier: FSL-1.1-ALv2

// Command cucina-controller is Cucina's control-plane binary: the fleet
// controller (reconcilers, autoscaler, enrollment, host stream, management API,
// HTTP service discovery), the stateless STS, and the helpers the Helm chart
// runs as init containers and hooks. See docs/dev/controller.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/sloper-ai/cucina/internal/config"
	"github.com/sloper-ai/cucina/internal/controller"
	"github.com/sloper-ai/cucina/internal/controller/canary"
	cucinaproto "github.com/sloper-ai/cucina/internal/proto"
)

const defaultConfig = "/etc/cucina/controller.json"

func main() {
	if err := newRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "cucina-controller:", err)
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "cucina-controller",
		Short:         "Cucina control plane: fleet controller, STS and chart helpers",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(runCmd(controller.ModeController, "controller", "Run the manager, reconcilers, autoscaler and every server (leader election)"))
	root.AddCommand(runCmd(controller.ModeSTS, "sts", "Run only the STS HTTPS server (stateless, leaderless Deployment)"))
	root.AddCommand(waitForCmd(), versionCmd(), bootstrapCmd(), uninstallPrepCmd(), canaryCmd(), keysCmd())
	return root
}

func runCmd(mode controller.Mode, use, short string) *cobra.Command {
	cfgPath, certs := defaultConfig, ""
	c := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return controller.Run(ctrl.SetupSignalHandler(), controller.Options{ConfigPath: cfgPath, Mode: mode, CertsFile: certs})
		},
	}
	c.Flags().StringVar(&cfgPath, "config", cfgPath, "controller configuration (JSON, parsed strictly)")
	if mode == controller.ModeController {
		c.Flags().StringVar(&certs, "certs", "", "in-cluster certificate list (JSON array of pki.CertSpec) the leader renews before expiry")
	}
	return c
}

func waitForCmd() *cobra.Command {
	var files, tcps []string
	timeout, interval := 5*time.Minute, time.Second
	c := &cobra.Command{
		Use:   "wait-for",
		Short: "Wait until files exist and TCP addresses accept connections (init-container helper)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var ts []controller.WaitTarget
			for _, f := range files {
				ts = append(ts, controller.WaitTarget{Kind: "file", Value: f})
			}
			for _, a := range tcps {
				ts = append(ts, controller.WaitTarget{Kind: "tcp", Value: a})
			}
			log := controller.SetupLogging("info", os.Stderr)
			return controller.WaitFor(ctrl.SetupSignalHandler(), controller.SystemClock{}, ts, timeout, interval, log)
		},
	}
	c.Flags().StringArrayVar(&files, "file", nil, "path that must exist (repeatable)")
	c.Flags().StringArrayVar(&tcps, "tcp", nil, "host:port that must accept TCP connections (repeatable)")
	c.Flags().DurationVar(&timeout, "timeout", timeout, "give up after this long (exit status 1)")
	c.Flags().DurationVar(&interval, "interval", interval, "polling interval")
	return c
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version, protocol version and compiled-in components as JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(map[string]any{
				"version":    controller.Version,
				"commit":     controller.Commit,
				"go":         runtime.Version(),
				"protocol":   cucinaproto.Current.String(),
				"compiledIn": controller.CompiledIn(),
			})
		},
	}
}

func kubeClients() (client.Client, kubernetes.Interface, error) {
	rc, err := ctrl.GetConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("kubernetes client configuration: %w", err)
	}
	scheme, err := controller.NewScheme()
	if err != nil {
		return nil, nil, err
	}
	c, err := client.New(rc, client.Options{Scheme: scheme})
	if err != nil {
		return nil, nil, err
	}
	k, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, nil, err
	}
	return c, k, nil
}

// optionalConfig loads the controller configuration when a path is given.
func optionalConfig(path string) (*config.Controller, error) {
	if path == "" {
		return nil, nil
	}
	return controller.LoadConfig(path, controller.ModeTool)
}

func bootstrapCmd() *cobra.Command {
	var ns, release, cfgPath, certs string
	c := &cobra.Command{
		Use:   "bootstrap",
		Short: "Create the CA, server certificates, signing keys and break-glass key if absent (Helm pre-install/pre-upgrade hook)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			log := controller.SetupLogging("info", os.Stderr)
			cfg, err := optionalConfig(cfgPath)
			if err != nil {
				return err
			}
			if cfg != nil {
				if ns == "" {
					ns = cfg.Namespace
				}
				if release == "" {
					release = cfg.ReleaseName
				}
			}
			if ns == "" || release == "" {
				return errors.New("bootstrap needs --namespace and --release (or --config)")
			}
			cl, kube, err := kubeClients()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(ctrl.SetupSignalHandler(), 5*time.Minute)
			defer cancel()
			return controller.Bootstrap(ctx, &controller.BootstrapDeps{Client: cl, Kube: kube, Namespace: ns, Release: release, Config: cfg, CertsFile: certs, Log: log})
		},
	}
	c.Flags().StringVar(&ns, "namespace", "", "namespace of the release")
	c.Flags().StringVar(&release, "release", "", "Helm release name")
	c.Flags().StringVar(&cfgPath, "config", "", "controller configuration (supplies the CA and auth Secret names)")
	c.Flags().StringVar(&certs, "certs", "", "in-cluster certificates to create if absent (JSON array of pki.CertSpec)")
	return c
}

func uninstallPrepCmd() *cobra.Command {
	var ns, cfgPath string
	timeout := 20 * time.Minute
	c := &cobra.Command{
		Use:   "uninstall-prep",
		Short: "Delete every WorkerPool and MacHost and wait until their VMs are stopped (Helm pre-delete hook)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			log := controller.SetupLogging("info", os.Stderr)
			cfg, err := optionalConfig(cfgPath)
			if err != nil {
				return err
			}
			if ns == "" && cfg != nil {
				ns = cfg.Namespace
			}
			if ns == "" {
				return errors.New("uninstall-prep needs --namespace (or --config)")
			}
			cl, _, err := kubeClients()
			if err != nil {
				return err
			}
			ctx := ctrl.SetupSignalHandler()
			d, err := controller.ToolDeps(ctx, cfg, log)
			if err != nil {
				return err
			}
			return controller.UninstallPrep(ctx, cl, d.Clock, ns, timeout, d.Compute, cfg, log)
		},
	}
	c.Flags().StringVar(&ns, "namespace", "", "namespace of the release")
	c.Flags().StringVar(&cfgPath, "config", "", "controller configuration (optional; enables the final EC2 instance check)")
	c.Flags().DurationVar(&timeout, "timeout", timeout, "how long to wait for the finalizers")
	return c
}

func keysCmd() *cobra.Command {
	cfgPath, kid, reason := defaultConfig, "", ""
	c := &cobra.Command{Use: "keys", Short: "Signing-key operations (R-AUTH-9); run inside the controller Pod"}
	run := func(action string) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, _ []string) error {
			log := controller.SetupLogging("info", os.Stderr)
			cfg, err := controller.LoadConfig(cfgPath, controller.ModeTool)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(ctrl.SetupSignalHandler(), time.Minute)
			defer cancel()
			newKID, err := controller.KeysCommand(ctx, cfg, action, kid, reason, log)
			if err != nil {
				return err
			}
			if newKID != "" {
				fmt.Fprintln(cmd.OutOrStdout(), newKID)
			}
			return nil
		}
	}
	rotate := &cobra.Command{Use: "rotate", Short: "Publish a successor signing key; the leader promotes it once every frontend loaded it", Args: cobra.NoArgs, RunE: run("rotate")}
	compromise := &cobra.Command{Use: "compromise", Short: "Remove a key from the JWKS now, activate a fresh one and restart the frontends", Args: cobra.NoArgs, RunE: run("compromise")}
	compromise.Flags().StringVar(&kid, "kid", "", `key ID to remove ("*" = every key)`)
	compromise.Flags().StringVar(&reason, "reason", "compromised", "reason recorded with the restart")
	c.PersistentFlags().StringVar(&cfgPath, "config", cfgPath, "controller configuration")
	c.AddCommand(rotate, compromise)
	return c
}

func canaryCmd() *cobra.Command {
	c := &cobra.Command{Use: "canary", Short: "Synthetic canaries (helm test, CronJob)"}
	var o canary.Options
	timeout := time.Minute
	cache := &cobra.Command{
		Use:   "cache",
		Short: "STS token exchange, GetCapabilities, CAS write/read and AC write/read through the client endpoint (starts no workers)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if o.STSURL == "" || o.Endpoint == "" || o.KeyFile == "" {
				return errors.New("canary cache needs --sts-url, --endpoint and --key-file")
			}
			ctx, cancel := context.WithTimeout(ctrl.SetupSignalHandler(), timeout)
			defer cancel()
			rep, err := canary.RunCache(ctx, o)
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			_ = enc.Encode(rep)
			return err
		},
	}
	f := cache.Flags()
	f.StringVar(&o.STSURL, "sts-url", "", "STS base URL (https://…)")
	f.StringVar(&o.Endpoint, "endpoint", "", "client endpoint (grpcs://host:port)")
	f.StringVar(&o.ServerName, "server-name", "", "TLS server name override")
	f.StringVar(&o.InstanceName, "instance", "main", "Buildbarn instance name")
	f.StringVar(&o.CAFile, "ca-file", "", "CA bundle for the STS and the endpoint (default: system roots)")
	f.StringVar(&o.KeyFile, "key-file", "", "file holding the service-account key (never printed)")
	f.DurationVar(&timeout, "timeout", timeout, "overall timeout")
	exec := &cobra.Command{
		Use:   "exec",
		Short: "Run a tiny uncached action (scale from zero); not implemented yet",
		RunE: func(*cobra.Command, []string) error {
			return errors.New("canary exec is not implemented yet (owned by the e2e harness)")
		},
	}
	c.AddCommand(cache, exec)
	return c
}
