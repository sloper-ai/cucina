// SPDX-License-Identifier: FSL-1.1-ALv2

// Package httpsd serves Prometheus HTTP service discovery for EC2 workers
// (R-OBS-1): GET /sd/workers returns one target group per running worker
// instance, addressed by its private IP and the bb_worker metrics port, with
// the labels pool, node, generation and instance_type.
//
// Prometheus refreshes HTTP SD every minute by default and keeps the previous
// targets when a refresh fails, so the handler serves a short-lived cache of
// tag-filtered Describe results instead of calling EC2 per request (R-SCALE-6).
// The leader's autoscaler loop publishes fresh observations into the same cache,
// so it is usually served without any extra API call.
package httpsd

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/sloper-ai/cucina/internal/ports"
)

// Path is where the handler is mounted on the metrics listener.
const Path = "/sd/workers"

// Group is one Prometheus HTTP SD target group.
type Group struct {
	Targets []string          `json:"targets"`
	Labels  map[string]string `json:"labels"`
}

// Groups converts worker instances into target groups: running instances with
// a private IP only, sorted by instance ID so the output is stable.
func Groups(instances []ports.Instance, port uint32) []Group {
	out := make([]Group, 0, len(instances))
	for _, in := range instances {
		if in.State != ports.InstanceRunning || !in.PrivateIP.IsValid() {
			continue
		}
		out = append(out, Group{
			Targets: []string{net.JoinHostPort(in.PrivateIP.String(), strconv.FormatUint(uint64(port), 10))},
			Labels: map[string]string{
				"pool":          string(in.Pool),
				"node":          in.ID,
				"generation":    in.Generation,
				"instance_type": in.Type,
			},
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Labels["node"] < out[j].Labels["node"] })
	return out
}

// FetchFunc lists the cluster's worker instances (always tag-filtered).
type FetchFunc func(ctx context.Context) ([]ports.Instance, error)

// Cache holds the last instance list for at most TTL.
type Cache struct {
	fetch FetchFunc
	ttl   time.Duration
	now   func() time.Time

	mu        sync.Mutex
	instances []ports.Instance
	at        time.Time
}

// NewCache returns a cache that refreshes through fetch when its content is
// older than ttl.
func NewCache(fetch FetchFunc, ttl time.Duration, now func() time.Time) *Cache {
	return &Cache{fetch: fetch, ttl: ttl, now: now}
}

// SetFetch installs the refresh function (when the EC2 adapter is known).
func (c *Cache) SetFetch(fetch FetchFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fetch = fetch
}

// Publish stores a fresh observation (from the autoscaler loop).
func (c *Cache) Publish(instances []ports.Instance) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.instances = append(c.instances[:0:0], instances...)
	c.at = c.now()
}

// Instances returns cached instances, refreshing them when stale.
func (c *Cache) Instances(ctx context.Context) ([]ports.Instance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && c.now().Sub(c.at) < c.ttl {
		return c.instances, nil
	}
	if c.fetch == nil {
		return c.instances, nil
	}
	in, err := c.fetch(ctx)
	if err != nil {
		return nil, err
	}
	c.instances, c.at = in, c.now()
	return in, nil
}

// Handler serves GET /sd/workers.
type Handler struct {
	Source interface {
		Instances(ctx context.Context) ([]ports.Instance, error)
	}
	Port uint32
	Log  *slog.Logger
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	instances, err := h.Source.Instances(r.Context())
	if err != nil {
		if h.Log != nil {
			h.Log.Warn("http-sd: listing worker instances failed", "err", err)
		}
		http.Error(w, "listing worker instances failed", http.StatusServiceUnavailable)
		return
	}
	b, err := json.Marshal(Groups(instances, h.Port))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(b)
}
