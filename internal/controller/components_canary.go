// SPDX-License-Identifier: FSL-1.1-ALv2

package controller

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/sloper-ai/cucina/internal/canary"
	"github.com/sloper-ai/cucina/internal/keys"
)

// Synthetic cache canary (R-TEST-7, agent e2e's internal/canary): the leader
// runs it every 5 minutes in-process (STS token exchange + JWKS check + AC/CAS
// round trip through the client endpoint; it starts no workers) and exports
// cucina_canary_*. One-shot runs (`helm test`, the CronJob, the e2e harness)
// POST their results to /canary/results on the metrics listener.
func init() {
	RegisterComponent(Factory{Name: "canary", Modes: []Mode{ModeController}, Order: 20, New: newCanary})
}

// CanaryEvery is the cache canary period (R-TEST-7).
const CanaryEvery = 5 * time.Minute

// canaryResultsPath is where one-shot canaries report (in-cluster metrics listener).
const canaryResultsPath = "/canary/results"

func newCanary(_ context.Context, d *Deps) (any, error) {
	m := canary.NewMetrics(d.Registry)
	if err := d.Manager.AddMetricsServerExtraHandler(canaryResultsPath, canary.Handler(m)); err != nil {
		return nil, err
	}
	if os.Getenv("CUCINA_CANARY_DISABLE") == "true" {
		return struct{}{}, nil
	}
	c := &canaryLoop{d: d, m: m}
	d.Share(sharedCanary, c)
	return c, nil
}

// sharedCanary is the key of the in-process canary loop (its last result feeds the alerts).
const sharedCanary = "canary.loop"

// canaryLoop is the leader's 5-minute cache canary.
type canaryLoop struct {
	d *Deps
	m *canary.Metrics

	mu       sync.Mutex
	result   *canary.Result
	failures int // consecutive failed runs
}

// last returns the latest result and how many runs in a row have failed.
func (c *canaryLoop) last() (*canary.Result, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.result, c.failures
}

// NeedLeaderElection implements manager.LeaderElectionRunnable: one canary per cluster.
func (c *canaryLoop) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable.
func (c *canaryLoop) Start(ctx context.Context) error {
	loop := &canary.Loop{
		Probe:   c.probe,
		Every:   CanaryEvery,
		Jitter:  0.1,
		Metrics: c.m,
		Log:     c.d.Log.With("component", "canary"),
		After:   c.d.Clock.After,
		OnResult: func(r canary.Result) {
			c.mu.Lock()
			c.result = &r
			if r.Success {
				c.failures = 0
			} else {
				c.failures++
			}
			c.mu.Unlock()
		},
	}
	if err := loop.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// canaryEnv returns the CUCINA_CANARY_* setting (shared with `canary cache`) or def.
func canaryEnv(name, def string) string {
	if v := os.Getenv("CUCINA_CANARY_" + name); v != "" {
		return v
	}
	return def
}

// probe builds and runs one cache probe. Endpoints default to the configured
// public ones; the chart may point CUCINA_CANARY_ENDPOINT/STS_URL at the
// in-cluster Services. The key is the mounted canary key or, by default, the
// break-glass key (the same default as `helm test`).
func (c *canaryLoop) probe(ctx context.Context) canary.Result {
	cfg := c.d.Config
	start := c.d.Clock.Now()
	fail := func(err error) canary.Result {
		return canary.Result{Kind: canary.KindCache, Started: start, Success: false, Error: err.Error()}
	}
	ep := canary.Endpoint{
		Target:       canaryEnv("ENDPOINT", cfg.Endpoints.ClientEndpoint),
		InstanceName: canaryEnv("INSTANCE", cfg.InstanceNames[0]),
		CAFile:       canaryEnv("CA_FILE", cfg.TLS.CAFile),
		ServerName:   canaryEnv("SERVER_NAME", ""),
	}
	stsURL := strings.TrimSuffix(canaryEnv("STS_URL", cfg.Endpoints.STSURL), "/")
	if ep.Target == "" || stsURL == "" {
		return fail(errors.New("no client endpoint or STS URL configured (endpoints.clientEndpoint, endpoints.stsUrl)"))
	}
	key, err := c.key(ctx)
	if err != nil {
		return fail(err)
	}
	tlsCfg, err := ep.TLSConfig()
	if err != nil {
		return fail(err)
	}
	p := &canary.Probe{
		Kind:     canary.KindCache,
		Endpoint: ep,
		Tokens:   &canary.STS{URL: stsURL, Key: key, HTTP: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg}}},
		Now:      c.d.Clock.Now,
	}
	return p.Run(ctx)
}

func (c *canaryLoop) key(ctx context.Context) (string, error) {
	if b, err := os.ReadFile(canaryEnv("KEY_FILE", "/var/run/secrets/cucina/canary/key")); err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	name := c.d.Config.Auth.BreakGlassKeySecret
	if name == "" {
		return "", errors.New("no canary key mounted and the break-glass key is disabled")
	}
	var sec corev1.Secret
	if err := c.d.APIReader.Get(ctx, client.ObjectKey{Namespace: c.d.Config.Namespace, Name: name}, &sec); err != nil {
		return "", err
	}
	k := strings.TrimSpace(string(sec.Data[keys.BreakGlassKeyEntry]))
	if k == "" {
		return "", errors.New("break-glass Secret has no key entry")
	}
	return k, nil
}
