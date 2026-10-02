// SPDX-License-Identifier: FSL-1.1-ALv2

package pki

import (
	"math"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sloper-ai/cucina/internal/ports"
)

// ExpiryMetricName is the metric of docs/contracts.md §6.
const ExpiryMetricName = "cucina_cert_expiry_seconds"

// Expiry roles (label values of cucina_cert_expiry_seconds{role}).
const (
	ExpiryRoleCA         = "ca"
	ExpiryRoleServer     = "server"
	ExpiryRoleController = "controller"
	ExpiryRoleHost       = "host"
	ExpiryRoleWorker     = "worker"
)

// ExpiryTracker exports cucina_cert_expiry_seconds{role}: seconds until the
// earliest-expiring tracked certificate of each role expires (negative once
// expired), computed at scrape time. Register it once on the process registry.
type ExpiryTracker struct {
	clock ports.Clock
	desc  *prometheus.Desc
	mu    sync.Mutex
	certs map[string]map[string]time.Time // role -> name -> notAfter
}

// NewExpiryTracker returns an empty tracker.
func NewExpiryTracker(clock ports.Clock) *ExpiryTracker {
	return &ExpiryTracker{
		clock: clock,
		desc: prometheus.NewDesc(ExpiryMetricName,
			"Seconds until the earliest-expiring certificate of the role expires (negative when expired).",
			[]string{"role"}, nil),
		certs: map[string]map[string]time.Time{},
	}
}

// Set records (or replaces) the expiry of the named certificate of a role. A nil
// tracker is a no-op, so components can take an optional tracker.
func (t *ExpiryTracker) Set(role, name string, notAfter time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.certs[role] == nil {
		t.certs[role] = map[string]time.Time{}
	}
	t.certs[role][name] = notAfter
}

// Delete forgets a certificate.
func (t *ExpiryTracker) Delete(role, name string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.certs[role], name)
}

// Describe implements prometheus.Collector.
func (t *ExpiryTracker) Describe(ch chan<- *prometheus.Desc) { ch <- t.desc }

// Collect implements prometheus.Collector.
func (t *ExpiryTracker) Collect(ch chan<- prometheus.Metric) {
	now := t.clock.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	for role, names := range t.certs {
		if len(names) == 0 {
			continue
		}
		earliest := math.Inf(1)
		for _, na := range names {
			earliest = math.Min(earliest, na.Sub(now).Seconds())
		}
		ch <- prometheus.MustNewConstMetric(t.desc, prometheus.GaugeValue, earliest, role)
	}
}
