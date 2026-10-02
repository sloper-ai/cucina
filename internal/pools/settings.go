// SPDX-License-Identifier: FSL-1.1-ALv2

package pools

import (
	"fmt"
	"slices"
	"sort"
	"sync"

	"google.golang.org/protobuf/types/known/durationpb"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/config"
	"github.com/sloper-ai/cucina/internal/domain"
)

// WorkerSettings renders the pool-level worker settings handed to a worker at
// enrollment (EC2: EnrollWorker; Tart: IssueVMIdentity through hostd). The
// agent/hostd adds the machine-dependent parts.
func (r *Resolved) WorkerSettings(cfg *config.Controller, node string) *cucinav1.WorkerSettings {
	ws := &cucinav1.WorkerSettings{
		Pool:                    string(r.Spec.Name),
		Node:                    node,
		SchedulerEndpoint:       cfg.Endpoints.WorkerScheduler,
		StorageEndpoint:         cfg.Endpoints.WorkerStorage,
		ServerName:              cfg.Endpoints.ServerName,
		BuildDirectory:          r.BuildDirectory,
		L1Placement:             r.L1Placement,
		L1SizeBytes:             r.L1SizeBytes,
		MaximumMessageSizeBytes: cfg.Worker.MaximumMessageSizeBytes,
		WanCompression:          r.Spec.Provider == domain.ProviderTart && cfg.Worker.WANCompressionForHosts,
		SizeClass:               r.Spec.SizeClass,
		InstanceNamePrefixes:    slices.Clone(r.Spec.InstanceNames),
		Deadman: &cucinav1.DeadmanSettings{
			IdleLimit:        durationpb.New(cfg.Autoscaler.DeadmanIdleLimit.Duration),
			UnreachableLimit: durationpb.New(cfg.Autoscaler.DeadmanUnreachableLimit.Duration),
			MaxUptime:        durationpb.New(cfg.Autoscaler.DeadmanMaxUptime.Duration),
		},
		MetricsPort:            cfg.Worker.MetricsPort,
		PushgatewayUrl:         cfg.Worker.PushgatewayURL,
		HandleSpotInterruption: r.Spot,
	}
	for i, ru := range r.Spec.Runners {
		rs := &cucinav1.RunnerSettings{Name: ru.Name, Concurrency: uint32(ru.Concurrency), Emulator: r.Runners[i].Emulator}
		if r.DeriveConcurrency[i] {
			rs.Concurrency = 0 // the worker derives it from its actual vCPUs
		}
		keys := make([]string, 0, len(ru.Properties))
		for k := range ru.Properties {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			rs.Platform = append(rs.Platform, &cucinav1.PlatformProperty{Name: k, Value: ru.Properties[k]})
		}
		ws.Runners = append(ws.Runners, rs)
	}
	return ws
}

// Set holds the currently resolved pools. The WorkerPool reconciler writes it;
// enrollment (SettingsFor), HTTP-SD and the management API read it.
type Set struct {
	cfg *config.Controller
	mu  sync.RWMutex
	m   map[domain.PoolName]*Resolved
}

// NewSet returns an empty set.
func NewSet(cfg *config.Controller) *Set {
	return &Set{cfg: cfg, m: map[domain.PoolName]*Resolved{}}
}

// Put stores (replaces) a resolved pool.
func (s *Set) Put(r *Resolved) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[r.Spec.Name] = r
}

// Delete forgets a pool.
func (s *Set) Delete(name domain.PoolName) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, name)
}

// Get returns a resolved pool.
func (s *Set) Get(name domain.PoolName) (*Resolved, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.m[name]
	return r, ok
}

// List returns all resolved pools sorted by name.
func (s *Set) List() []*Resolved {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Resolved, 0, len(s.m))
	for _, r := range s.m {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Spec.Name < out[j].Spec.Name })
	return out
}

// SettingsFor implements the enrollment side's pool lookup: the WorkerSettings
// and current generation for a worker of pool on node.
func (s *Set) SettingsFor(pool, node string) (*cucinav1.WorkerSettings, string, error) {
	r, ok := s.Get(domain.PoolName(pool))
	if !ok {
		return nil, "", fmt.Errorf("pool %q is unknown or not resolved", pool)
	}
	return r.WorkerSettings(s.cfg, node), r.Spec.Generation, nil
}
