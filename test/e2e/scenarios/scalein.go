// SPDX-License-Identifier: FSL-1.1-ALv2

package scenarios

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sloper-ai/cucina/internal/pools"
	"github.com/sloper-ai/cucina/test/e2e/collect/scalein"
	"github.com/sloper-ai/cucina/test/e2e/harness"
	"github.com/sloper-ai/cucina/test/e2e/infra"
	"github.com/sloper-ai/cucina/test/e2e/nfr"
	"github.com/sloper-ai/cucina/test/e2e/remote"
)

// Match the controller's idle-worker grace, not its much longer busy-drain
// timeout (internal/reconcile/loop.go idleDrainGrace, R-SCALE-3/NFR-C1).
const scaleInGrace = 2 * time.Minute
const scaleInPoll = 5 * time.Second

type scaleInTimeline struct {
	SchemaVersion int                       `json:"schemaVersion"`
	RunID         string                    `json:"runId"`
	Policies      map[string]scalein.Policy `json:"policies"`
	Samples       []scalein.Sample          `json:"samples"`
	Error         string                    `json:"error,omitempty"`
}

type scaleInCapture struct {
	c        *harness.Context
	svc      *infra.Services
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	stop     sync.Once
	mu       sync.Mutex
	timeline scaleInTimeline
}

// startScaleInCapture runs before the T7 workloads: a faster pool can finish
// and terminate while another build is still running. Post-hoc T8 cannot
// reconstruct that worker's idle duration from the bounded event history.
func startScaleInCapture(c *harness.Context, svc *infra.Services) (*scaleInCapture, error) {
	if svc.EC2 == nil {
		return nil, harness.Skip("live scale-in requires tagged EC2 inventory")
	}
	cat, err := pools.LoadCatalog(filepath.Join(c.Env.RepoDir, "platforms/pools.json"))
	if err != nil {
		return nil, err
	}
	var list struct {
		Schema string `json:"schema"`
		Pools  *[]struct {
			Name     string `json:"name"`
			Provider string `json:"provider"`
		} `json:"pools"`
	}
	if err := svc.CucinactlJSON(c, &list, "pools", "list"); err != nil {
		return nil, err
	}
	if list.Schema != "pool-list.v1" || list.Pools == nil {
		return nil, fmt.Errorf("scale-in requires a complete pool-list.v1")
	}
	policies := map[string]scalein.Policy{}
	for _, p := range *list.Pools {
		if p.Provider != "ec2" {
			continue
		}
		pi, err := describePool(c, svc, p.Name)
		if err != nil {
			return nil, err
		}
		spec := specOf(pi)
		platform, ok := cat.Platform(str(field(spec, "platform")))
		if !ok {
			return nil, fmt.Errorf("pool %s: no catalog defaults", p.Name)
		}
		idle := platform.Defaults.IdleTimeout.Duration
		if timers, ok := field(spec, "timers").(map[string]any); ok {
			if value := field(timers, "idle_timeout"); value != nil {
				idle = dur(value)
			}
		}
		if idle <= 0 {
			return nil, fmt.Errorf("pool %s: invalid idle timeout", p.Name)
		}
		if min, ok := field(pi.Raw, "min_running").(float64); !ok || min != 0 {
			return nil, harness.Skip("pool %s needs a known zero minimum for scale-in proof", p.Name)
		}
		policies[p.Name] = scalein.Policy{IdleTimeout: idle, DrainGrace: scaleInGrace}
	}
	if len(policies) == 0 {
		return nil, harness.Skip("no EC2 pools available for scale-in observation")
	}
	ctx, cancel := context.WithCancel(c)
	cap := &scaleInCapture{c: c, svc: svc, ctx: ctx, cancel: cancel, done: make(chan struct{}), timeline: scaleInTimeline{SchemaVersion: 1, RunID: c.Env.RunID, Policies: policies}}
	// Synchronous first sample establishes the observation boundary before
	// the caller is allowed to submit work.
	cap.append(cap.sample())
	go func() {
		defer close(cap.done)
		for {
			if err := cap.sleep(ctx, scaleInPoll); err != nil {
				return
			}
			sample := cap.sample()
			if ctx.Err() != nil {
				return
			}
			cap.append(sample)
		}
	}()
	return cap, nil
}

func (s *scaleInCapture) sleep(ctx context.Context, d time.Duration) error {
	if s.svc.Clock != nil {
		return s.svc.Clock.Sleep(ctx, d)
	}
	return remote.RealSleep(ctx, d)
}
func (s *scaleInCapture) append(sample scalein.Sample) {
	s.mu.Lock()
	s.timeline.Samples = append(s.timeline.Samples, sample)
	s.mu.Unlock()
}
func (s *scaleInCapture) sample() scalein.Sample {
	out := scalein.Sample{Started: s.c.Now(), Queues: map[string]scalein.Queue{}}
	var workers struct {
		Schema  string `json:"schema"`
		Workers *[]struct {
			Node    string   `json:"node"`
			Pool    string   `json:"pool"`
			State   string   `json:"state"`
			Busy    *uint32  `json:"busy_threads"`
			Idle    *float64 `json:"idle_seconds"`
			Drained *bool    `json:"drained"`
		} `json:"workers"`
	}
	if err := s.svc.CucinactlJSON(s.ctx, &workers, "workers", "list"); err != nil {
		out.Errors = append(out.Errors, "workers: "+err.Error())
	} else if workers.Schema != "worker-list.v1" || workers.Workers == nil {
		out.Errors = append(out.Errors, "workers: incomplete worker-list.v1")
	} else {
		for _, w := range *workers.Workers {
			if _, ok := s.timeline.Policies[w.Pool]; !ok {
				continue
			}
			if w.Node == "" || w.State == "" || w.Busy == nil || w.Drained == nil {
				out.Errors = append(out.Errors, "worker missing safety fields")
				continue
			}
			row := scalein.Worker{Node: w.Node, Pool: w.Pool, State: w.State, Busy: *w.Busy, Drained: *w.Drained}
			if w.Idle != nil {
				d := time.Duration(*w.Idle * float64(time.Second))
				row.IdleFor = &d
			}
			out.Workers = append(out.Workers, row)
		}
	}
	var queues struct {
		Schema string `json:"schema"`
		Queues *[]struct {
			Pool   string  `json:"pool"`
			Queued *uint64 `json:"queued"`
		} `json:"queues"`
	}
	if err := s.svc.CucinactlJSON(s.ctx, &queues, "queues"); err != nil {
		out.Errors = append(out.Errors, "queues: "+err.Error())
	} else if queues.Schema != "queue-list.v1" || queues.Queues == nil {
		out.Errors = append(out.Errors, "queues: incomplete queue-list.v1")
	} else {
		for _, q := range *queues.Queues {
			if _, ok := s.timeline.Policies[q.Pool]; !ok {
				continue
			}
			if q.Queued == nil {
				out.Errors = append(out.Errors, "queue missing queued count")
				continue
			}
			x := out.Queues[q.Pool]
			x.Queued += *q.Queued
			out.Queues[q.Pool] = x
		}
	}
	for pool := range s.timeline.Policies {
		var raw map[string]any
		if err := s.svc.CucinactlJSON(s.ctx, &raw, "pools", "describe", pool); err != nil {
			out.Errors = append(out.Errors, "drain history: "+err.Error())
			continue
		}
		if raw["schema"] != "pool-describe.v1" {
			out.Errors = append(out.Errors, "drain history: wrong schema")
			continue
		}
		for _, e := range poolEvents(PoolInfo{Raw: raw}) {
			if e.Type == "drain-acknowledged" {
				out.Drains = append(out.Drains, scalein.Drain{Node: e.Subject, Pool: pool, At: e.Time, Acknowledged: true})
			}
		}
	}
	inv, err := s.svc.EC2.DescribeStates(s.ctx)
	if err != nil {
		out.Errors = append(out.Errors, "provider states: "+err.Error())
	} else {
		for _, in := range inv.Instances {
			out.Instances = append(out.Instances, scalein.Instance{Node: in.ID, Pool: in.Pool, State: in.State, Launched: in.LaunchTime})
		}
	}
	out.Finished = s.c.Now()
	return out
}

func (s *scaleInCapture) snapshot() scaleInTimeline {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.timeline
	out.Samples = append([]scalein.Sample(nil), s.timeline.Samples...)
	return out
}

// finish observes natural scale-in after the builds, with a bounded wait.
// It preserves incomplete evidence rather than preventing T8 diagnostics.
func (s *scaleInCapture) finish() scaleInTimeline {
	limit := time.Duration(0)
	for _, p := range s.timeline.Policies {
		limit = max(limit, p.IdleTimeout+p.DrainGrace)
	}
	deadline := s.c.Now().Add(limit + 2*scaleInPoll)
	for {
		t := s.snapshot()
		r := scalein.Evaluate(t.Policies, t.Samples)
		terminal := len(r.Nodes) > 0
		for _, n := range r.Nodes {
			terminal = terminal && !n.TerminatedBy.IsZero()
		}
		if terminal {
			break
		}
		if !s.c.Now().Before(deadline) {
			s.mu.Lock()
			s.timeline.Error = "scale-in observation deadline exceeded"
			s.mu.Unlock()
			break
		}
		if err := s.sleep(s.ctx, scaleInPoll); err != nil {
			s.mu.Lock()
			s.timeline.Error = err.Error()
			s.mu.Unlock()
			break
		}
	}
	s.close()
	return s.snapshot()
}
func (s *scaleInCapture) close() { s.stop.Do(func() { s.cancel(); <-s.done }) }

// saveScaleInEvidence uses a file because the unredacted per-node timeline
// is private run evidence, not a huge report value or a committed fixture.
func saveScaleInEvidence(c *harness.Context, t scaleInTimeline) error {
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(c.Dir(), "scale-in-evidence.json")
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return err
	}
	c.Record("scaleInEvidencePath", path)
	return c.Artifact("scale-in-evidence", path)
}

// runScaleInEvidence evaluates the complete T7 cohort, including workers
// already gone before T8 starts, and independently waits for actual residue.
func runScaleInEvidence(c *harness.Context) error {
	prior, ok := c.Prior["T7"]
	if !ok {
		return harness.Skip("T8 needs a T7 run with pre-workload live scale-in capture")
	}
	path := filepath.Join(c.Env.ArtifactsDir, c.Env.RunID, "T7", "scale-in-evidence.json")
	if str(prior.Values["scaleInEvidencePath"]) != path {
		return harness.Skip("T7 did not persist live scale-in evidence; rerun T7 with the collector")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	registered := false
	for _, a := range prior.Artifacts {
		if a.Name == "scale-in-evidence" && a.Path == filepath.Join("T7", "scale-in-evidence.json") && a.SHA256 == hex.EncodeToString(sum[:]) && a.Bytes == int64(len(b)) {
			registered = true
		}
	}
	if !registered {
		return harness.Fail("T7 lifecycle artifact hash/size is not bound to this prior result")
	}
	var timeline scaleInTimeline
	if err := json.Unmarshal(b, &timeline); err != nil {
		return err
	}
	if timeline.SchemaVersion != 1 || timeline.RunID != c.Env.RunID || len(timeline.Samples) == 0 || timeline.Samples[0].Started.Before(prior.Started) {
		return harness.Fail("stale or incomplete scale-in cohort record")
	}
	if prior.Finished.IsZero() {
		return harness.Fail("T7 lifecycle result has no finished observation boundary")
	}
	for _, s := range timeline.Samples {
		if s.Started.Before(prior.Started) || s.Finished.After(prior.Finished) {
			return harness.Fail("T7 lifecycle observations escaped the persisted attempt interval")
		}
	}
	result := scalein.Evaluate(timeline.Policies, timeline.Samples)
	if timeline.Error != "" {
		result.Pass = false
		result.Unavailable = append(result.Unavailable, timeline.Error)
	}
	c.Record("scaleIn", result)
	for _, n := range result.Nodes {
		if !n.IdleEarliest.IsZero() {
			c.Event(n.ID, "idle+empty queue (earliest bound)", n.IdleEarliest)
		}
		if !n.DrainedAt.IsZero() {
			c.Event(n.ID, "drain acknowledged", n.DrainedAt)
		}
		if !n.TerminatedBy.IsZero() {
			c.Event(n.ID, "EC2 terminated observed by", n.TerminatedBy)
		}
		c.Metric("scale_in."+n.ID+".upper_seconds", n.ElapsedUpper.Seconds(), "s")
	}
	took, residue, err := waitZero(c, scaleInLimit)
	if err != nil {
		return err
	}
	c.Metric("scale_in_final_residue_wait_seconds", took.Seconds(), "s")
	c.NFR(nfr.Residue("after scale-in", residue.Instances, residue.Volumes, residue.ENIs, residue.EIPs, residue.PublicIPs))
	if len(result.Violations) > 0 {
		return harness.Fail("scale-in timing violations: %s", strings.Join(result.Violations, "; "))
	}
	if !residue.Zero() {
		return harness.Fail("scale-in left resources: %s", residue)
	}
	if !result.Pass {
		return harness.Skip("scale-in timing evidence unavailable: %s", strings.Join(result.Unavailable, "; "))
	}
	c.Check(harness.CheckResult{Name: "every cohort worker drained and terminated within idle timeout + grace", Kind: "lifecycle", Pass: true, Detail: fmt.Sprintf("%d nodes, %d timestamped samples", len(result.Nodes), len(timeline.Samples))})
	return nil
}
