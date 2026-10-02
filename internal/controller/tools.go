// SPDX-License-Identifier: FSL-1.1-ALv2

package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/config"
	"github.com/sloper-ai/cucina/internal/ports"
)

// ------------------------------------------------------------------ wait-for

// WaitTarget is one condition `wait-for` waits on.
type WaitTarget struct {
	Kind  string // "file" or "tcp"
	Value string // path or host:port
}

func (t WaitTarget) String() string { return t.Kind + ":" + t.Value }

// WaitFor blocks until every target is satisfied (a file exists, a TCP address
// accepts connections) or timeout elapses. It is the init-container helper of
// the chart (e.g. wait for the CA Secret's files or the scheduler port).
func WaitFor(ctx context.Context, clock ports.Clock, targets []WaitTarget, timeout, interval time.Duration, log *slog.Logger) error {
	if len(targets) == 0 {
		return errors.New("wait-for: nothing to wait for (use --file or --tcp)")
	}
	if interval <= 0 {
		interval = time.Second
	}
	deadline := clock.Now().Add(timeout)
	pending := append([]WaitTarget(nil), targets...)
	for {
		var still []WaitTarget
		for _, t := range pending {
			if err := check(ctx, t, interval); err != nil {
				still = append(still, t)
			} else {
				log.Info("wait-for: ready", "target", t.String())
			}
		}
		pending = still
		if len(pending) == 0 {
			return nil
		}
		if !clock.Now().Before(deadline) {
			names := make([]string, len(pending))
			for i, t := range pending {
				names[i] = t.String()
			}
			return fmt.Errorf("wait-for: timed out after %s waiting for %s", timeout, strings.Join(names, ", "))
		}
		if err := clock.Sleep(ctx, interval); err != nil {
			return err
		}
	}
}

func check(ctx context.Context, t WaitTarget, dialTimeout time.Duration) error {
	switch t.Kind {
	case "file":
		_, err := os.Stat(t.Value)
		return err
	case "tcp":
		d := net.Dialer{Timeout: dialTimeout}
		c, err := d.DialContext(ctx, "tcp", t.Value)
		if err != nil {
			return err
		}
		return c.Close()
	}
	return fmt.Errorf("unknown wait-for target kind %q", t.Kind)
}

// ----------------------------------------------------------------- bootstrap

// Bootstrap runs every compiled-in bootstrap step (Helm pre-install and
// pre-upgrade hook): create-if-absent CA, server certificates, signing keys,
// JWKS/deny-list ConfigMaps and the break-glass key. Steps are idempotent and
// never overwrite existing Secrets; nothing secret is printed.
func Bootstrap(ctx context.Context, d *BootstrapDeps) error {
	steps := bootstrapSteps()
	if len(steps) == 0 {
		return errors.New("bootstrap: no bootstrap steps are compiled into this binary")
	}
	for _, s := range steps {
		start := time.Now()
		if err := s.Run(ctx, d); err != nil {
			return fmt.Errorf("bootstrap step %s: %w", s.Name, err)
		}
		d.Log.Info("bootstrap step done", "step", s.Name, "took", time.Since(start).Round(time.Millisecond).String())
	}
	return nil
}

// ------------------------------------------------------------ uninstall-prep

// UninstallPrep is the Helm pre-delete hook (R-OPS-3): it deletes every
// WorkerPool and MacHost of the namespace and waits until their finalizers
// have drained and stopped every VM. With compute it then verifies that no
// controller-created instance of the cluster is left. It fails (non-zero exit)
// when the timeout expires or instances remain.
func UninstallPrep(ctx context.Context, c client.Client, clock ports.Clock, namespace string, timeout time.Duration, compute ports.Compute, cfg *config.Controller, log *slog.Logger) error {
	var pools v1alpha1.WorkerPoolList
	var hosts v1alpha1.MacHostList
	if err := c.List(ctx, &pools, client.InNamespace(namespace)); err != nil {
		return err
	}
	if err := c.List(ctx, &hosts, client.InNamespace(namespace)); err != nil {
		return err
	}
	for i := range pools.Items {
		if err := c.Delete(ctx, &pools.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	for i := range hosts.Items {
		if err := c.Delete(ctx, &hosts.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	log.Info("uninstall-prep: deleted pools and hosts; waiting for their finalizers", "pools", len(pools.Items), "hosts", len(hosts.Items), "timeout", timeout.String())

	deadline := clock.Now().Add(timeout)
	for {
		left, err := remaining(ctx, c, namespace)
		if err != nil {
			return err
		}
		if len(left) == 0 {
			break
		}
		if !clock.Now().Before(deadline) {
			return fmt.Errorf("uninstall-prep: timed out after %s; still finalizing: %s (check the controller's logs and `kubectl -n %s describe workerpools,machosts`)",
				timeout, strings.Join(left, ", "), namespace)
		}
		if err := clock.Sleep(ctx, 2*time.Second); err != nil {
			return err
		}
	}
	if compute != nil && cfg != nil {
		ins, err := compute.Describe(ctx, ports.InstanceFilter{Cluster: cfg.ClusterID,
			States: []ports.InstanceState{ports.InstancePending, ports.InstanceRunning, ports.InstanceStopping, ports.InstanceStopped}})
		if err != nil {
			return fmt.Errorf("uninstall-prep: verifying that no worker instance is left: %w", err)
		}
		if len(ins) > 0 {
			ids := make([]string, len(ins))
			for i, in := range ins {
				ids[i] = in.ID + " (" + string(in.Pool) + ")"
			}
			return fmt.Errorf("uninstall-prep: %d controller-created instance(s) remain: %s", len(ins), strings.Join(ids, ", "))
		}
	}
	log.Info("uninstall-prep: every pool and host is gone")
	return nil
}

func remaining(ctx context.Context, c client.Client, namespace string) ([]string, error) {
	var pools v1alpha1.WorkerPoolList
	var hosts v1alpha1.MacHostList
	if err := c.List(ctx, &pools, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	if err := c.List(ctx, &hosts, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	var out []string
	for _, p := range pools.Items {
		out = append(out, "workerpool/"+p.Name)
	}
	for _, h := range hosts.Items {
		out = append(out, "machost/"+h.Name)
	}
	sort.Strings(out)
	return out, nil
}

// ToolDeps builds the dependencies of a one-shot subcommand that needs the
// Compute adapter (uninstall-prep with an aws configuration).
func ToolDeps(ctx context.Context, cfg *config.Controller, log *slog.Logger) (*Deps, error) {
	d := &Deps{Mode: ModeTool, Config: cfg, Log: log, Clock: SystemClock{}, Rand: NewSystemRand()}
	if cfg == nil || cfg.AWS == nil {
		return d, nil
	}
	registry.Lock()
	f := registry.compute
	registry.Unlock()
	if f == nil {
		return d, nil
	}
	c, err := f(ctx, d)
	if err != nil {
		return nil, fmt.Errorf("EC2 adapter: %w", err)
	}
	d.Compute = c
	return d, nil
}
