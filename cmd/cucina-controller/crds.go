// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/sloper-ai/cucina/api/crds"
	"github.com/sloper-ai/cucina/internal/controller"
)

// crdsCmd is `crds apply`, the chart's pre-install/pre-upgrade hook: Helm
// installs the chart's crds/ only on first install, so upgrades apply the CRDs
// compiled into this binary (api/crds) with server-side apply (ADR 0406).
func crdsCmd() *cobra.Command {
	c := &cobra.Command{Use: "crds", Short: "Cucina's CustomResourceDefinitions (WorkerPool, MacHost, TrustPolicy)"}
	timeout := 2 * time.Minute
	apply := &cobra.Command{
		Use:   "apply",
		Short: "Server-side apply the CRDs this binary was built against and wait until they are Established (Helm pre-install/pre-upgrade hook)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			log := controller.SetupLogging("info", os.Stderr)
			objs, err := controller.ParseCRDs(crds.FS)
			if err != nil {
				return err
			}
			rc, err := ctrl.GetConfig()
			if err != nil {
				return fmt.Errorf("kubernetes client configuration: %w", err)
			}
			cl, err := client.New(rc, client.Options{})
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(ctrl.SetupSignalHandler(), timeout)
			defer cancel()
			return controller.ApplyCRDs(ctx, cl, objs, log)
		},
	}
	apply.Flags().DurationVar(&timeout, "timeout", timeout, "give up after this long (exit status 1)")
	c.AddCommand(apply)
	return c
}
