// SPDX-License-Identifier: FSL-1.1-ALv2

package canary

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/pools"
	"github.com/sloper-ai/cucina/internal/ports"
)

// ExecutionStateAnnotation persists the execution schedule across leader changes.
const ExecutionStateAnnotation = "cucina.sloper.ai/execution-canary"

// ExecutionEvery and ExecutionTimeout bound the frequency and duration of paid probes.
const (
	ExecutionEvery   = 24 * time.Hour
	ExecutionTimeout = 15 * time.Minute
)

// The pinned scheduler retains an operation without a waiting client for one
// minute; an action picked up during that minute has a one-minute timeout.
// Reserve both after cancellation/crash, not just the client deadline. This
// assumes responsive pinned components, not proof of remote quiescence under
// arbitrary stalls; ADR 1007 cites the pin and qualification limitation.
const executionSettle = 2 * time.Minute

// ExecutionTarget is an unambiguous pool runner and instance, resolved from the
// same platform catalog as the workers. It never changes pool lifecycle policy.
type ExecutionTarget struct {
	Pool     string
	Instance string
	Platform map[string]string
}

// ExecutionScheduler schedules the existing Probe on the controller leader.
// Reader must be uncached; Client writes optimistic schedule reservations before
// any work is submitted. Probe must respect its context, as Probe.Run does.
// Start and Tick must not be called concurrently on the same scheduler.
type ExecutionScheduler struct {
	Client        client.Client
	Reader        client.Reader
	Namespace     string
	InstanceNames []string
	Catalog       *pools.Catalog
	Clock         ports.Clock
	Deployment    string
	Timeout       time.Duration
	Probe         func(context.Context, ExecutionTarget) Result
	Metrics       *Metrics
	Log           *slog.Logger
}

// executionState deliberately records an attempt before submitting it. A crash
// between that write and Execute is an incomplete failure, not permission for
// another cold start. LastSuccess survives both failed attempts and restarts.
type executionState struct {
	Deployment  string    `json:"deployment"`
	Started     time.Time `json:"started"`
	HoldUntil   time.Time `json:"holdUntil,omitempty"`
	Result      *Result   `json:"result,omitempty"`
	LastSuccess time.Time `json:"lastSuccess,omitempty"`
}

// NeedLeaderElection keeps execution on the same elected leader as scaling.
func (*ExecutionScheduler) NeedLeaderElection() bool { return true }

// Start watches the durable schedule independently of the cache loop. Polling
// checks newly created/reconciled pools without introducing extra executions.
func (s *ExecutionScheduler) Start(ctx context.Context) error {
	for {
		if err := s.Tick(ctx); err != nil && ctx.Err() == nil && s.Log != nil {
			s.Log.Error("execution canary schedule", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-s.Clock.After(time.Minute):
		}
	}
}

// Tick checks the durable schedule and submits due probes serially. Kubernetes
// read/write failures never authorize a submission. Completed attempts, including
// preflight failures, run again only after 24h or a new deployment identity.
func (s *ExecutionScheduler) Tick(ctx context.Context) (err error) {
	if s.Metrics != nil {
		// Re-establish current health from this inventory, not the previous
		// tick. Deleted pools and failed reads must never retain up=1.
		s.Metrics.maskExecution()
		defer func() {
			if err != nil {
				s.Metrics.maskExecution()
			}
		}()
	}
	if s.Timeout <= 0 || s.Timeout > ExecutionTimeout {
		return fmt.Errorf("execution canary timeout must be positive and at most %s", ExecutionTimeout)
	}
	if s.Deployment == "" {
		return errors.New("execution canary deployment identity is empty")
	}
	var list v1alpha1.WorkerPoolList
	if err := s.Reader.List(ctx, &list, client.InNamespace(s.Namespace)); err != nil {
		return err
	}
	slices.SortFunc(list.Items, func(a, b v1alpha1.WorkerPool) int {
		return strings.Compare(a.Name, b.Name)
	})
	// Restore every pool before considering any work. An outstanding reservation
	// anywhere blocks new submissions, including after a leadership handoff.
	blocked := false
	var stateErrors []error
	for i := range list.Items {
		wp := &list.Items[i]
		state, err := executionRecord(wp)
		if err != nil {
			s.pending(wp.Name, time.Time{})
			stateErrors = append(stateErrors, err)
			continue
		}
		s.restore(wp, state)
		blocked = blocked || s.Clock.Now().Before(state.HoldUntil)
	}
	if err := errors.Join(stateErrors...); err != nil {
		return err // unknown prior work: fail closed for the entire inventory
	}
	if blocked {
		return nil
	}
	for i := range list.Items {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Refresh the resource version and operator controls immediately before
		// reserving. The optimistic patch refuses a concurrent spec/status change.
		wp := &v1alpha1.WorkerPool{}
		if err := s.Reader.Get(ctx, client.ObjectKeyFromObject(&list.Items[i]), wp); err != nil {
			return err
		}
		state, err := executionRecord(wp)
		if err != nil {
			s.pending(wp.Name, time.Time{})
			return err
		}
		s.restore(wp, state)
		now := s.Clock.Now()
		if now.Before(state.HoldUntil) {
			return nil
		}
		if !state.Started.IsZero() && state.Result == nil {
			r := Result{Kind: KindExec, Pool: wp.Name, Started: state.Started,
				Duration: state.HoldUntil.Sub(state.Started), Error: "previous execution canary did not record completion; not retried before the next scheduled attempt"}
			state.Result = &r
			if err := s.save(ctx, wp, state); err != nil {
				return err
			}
			s.observe(r)
		}
		if state.Deployment == s.Deployment && now.Before(state.Started.Add(ExecutionEvery)) {
			continue
		}
		reason, pending := unavailablePool(wp)
		if pending {
			// A brand-new pool must first be reconciled, rather than consume its
			// deployment attempt against a missing/stale startup status.
			s.pending(wp.Name, state.LastSuccess)
			continue
		}
		target, targetErr := s.target(wp, list.Items)
		if reason == "" && targetErr != nil {
			reason = targetErr.Error()
		}
		next := executionState{Deployment: s.Deployment, Started: now, LastSuccess: state.LastSuccess}
		if reason != "" {
			r := Result{Kind: KindExec, Pool: wp.Name, Started: now, Error: "execution not attempted: " + reason}
			next.Result = &r
			if err := s.save(ctx, wp, next); err != nil {
				return err
			}
			s.observe(r)
			continue
		}
		// One absolute deadline covers reservation I/O AND the probe. A
		// delayed/ambiguous patch response cannot buy extra execution time
		// beyond the hold that another leader reads from the API.
		deadline := now.Add(s.Timeout)
		next.HoldUntil = deadline.Add(executionSettle)
		pctx, cancel := context.WithDeadline(ctx, deadline)
		if err := s.save(pctx, wp, next); err != nil {
			cancel()
			return err
		}
		s.pending(wp.Name, state.LastSuccess)
		var r Result
		if pctx.Err() == nil && s.Clock.Now().Before(deadline) {
			r = s.Probe(pctx, target)
		} else {
			r.Error = "execution deadline expired while reserving; no probe submitted"
		}
		probeErr := pctx.Err()
		cancel()
		if err := ctx.Err(); err != nil {
			return err // keep the reservation; the next leader reports it incomplete
		}
		r.Kind, r.Pool, r.Instance = KindExec, target.Pool, target.Instance
		r.Started, r.Duration = now, s.Clock.Now().Sub(now)
		if probeErr != nil {
			r.Success, r.Error = false, probeErr.Error()
		}
		r.Error = truncate(r.Error, 1024)
		next.Result = &r
		if r.Success {
			next.LastSuccess = r.Started.Add(r.Duration)
			next.HoldUntil = time.Time{}
		}
		// Status writes from the reconciler are expected while the probe runs.
		// Read fresh, but only complete the reservation we actually own.
		if err := s.Reader.Get(ctx, client.ObjectKeyFromObject(wp), wp); err != nil {
			return err
		}
		current, err := executionRecord(wp)
		if err != nil {
			return err
		}
		if current.Deployment != next.Deployment || !current.Started.Equal(next.Started) || current.Result != nil {
			return errors.New("execution canary reservation changed before completion")
		}
		if err := s.save(ctx, wp, next); err != nil {
			return err
		}
		s.observe(r)
		s.restore(wp, next) // availability may have changed while the action ran
		if !r.Success {
			return nil // retain the cancellation/remote-action grace before another pool
		}
	}
	return nil
}

func executionRecord(wp *v1alpha1.WorkerPool) (executionState, error) {
	var state executionState
	if raw := wp.Annotations[ExecutionStateAnnotation]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &state); err != nil {
			return state, fmt.Errorf("pool %s has invalid execution canary state: %w", wp.Name, err)
		}
		if state.Started.IsZero() || state.Deployment == "" || (state.Result == nil && state.HoldUntil.IsZero()) {
			return state, fmt.Errorf("pool %s has incomplete execution canary state", wp.Name)
		}
	}
	return state, nil
}

func (s *ExecutionScheduler) save(ctx context.Context, wp *v1alpha1.WorkerPool, state executionState) error {
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	base := wp.DeepCopy()
	if wp.Annotations == nil {
		wp.Annotations = map[string]string{}
	}
	wp.Annotations[ExecutionStateAnnotation] = string(b)
	return s.Client.Patch(ctx, wp, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

func (s *ExecutionScheduler) pending(pool string, lastSuccess time.Time) {
	if s.Metrics != nil {
		s.Metrics.setUp(KindExec, pool, 0)
		if !lastSuccess.IsZero() {
			s.Metrics.lastSuccess.WithLabelValues(KindExec, pool).Set(float64(lastSuccess.Unix()))
		}
	}
}

func (s *ExecutionScheduler) restore(wp *v1alpha1.WorkerPool, state executionState) {
	s.pending(wp.Name, state.LastSuccess)
	if s.Metrics != nil {
		if state.Result != nil {
			s.Metrics.restore(*state.Result)
		} else if !state.Started.IsZero() {
			// A surviving reservation is immediately visible as unverified
			// evidence, even before its conservative overlap hold expires.
			s.Metrics.restore(Result{Kind: KindExec, Pool: wp.Name, Started: state.Started})
		}
	}
	if reason, _ := unavailablePool(wp); reason != "" || state.Deployment != s.Deployment {
		// Preserve the historical success timestamp, but never display a
		// paused/offline/failed pool or a pending deployment as currently up.
		s.pending(wp.Name, state.LastSuccess)
	}
}

func (s *ExecutionScheduler) observe(r Result) {
	if s.Metrics != nil {
		s.Metrics.Observe(r)
	}
	if s.Log != nil {
		s.Log.Info("execution canary", "pool", r.Pool, "success", r.Success, "duration", r.Duration.String(), "error", r.Error)
	}
}

// unavailablePool does not require any registered workers: Ready means capacity
// can be obtained. Zero is a normal probe target, not proof of a cold start.
func unavailablePool(wp *v1alpha1.WorkerPool) (reason string, pending bool) {
	switch {
	case wp.DeletionTimestamp != nil:
		return "pool is being deleted", false
	case wp.Spec.Paused:
		return "pool is paused", false
	case wp.Spec.Capacity.Max == 0:
		return "pool capacity.max is zero", false
	case wp.Status.ObservedGeneration != wp.Generation:
		return "pool generation has not been observed", true
	}
	ready := meta.FindStatusCondition(wp.Status.Conditions, v1alpha1.ConditionReady)
	if ready == nil || ready.ObservedGeneration != wp.Generation || ready.Status == metav1.ConditionUnknown {
		return "pool readiness is unknown", true
	}
	if ready.Status != metav1.ConditionTrue {
		return "pool is not ready: " + ready.Reason, false
	}
	if degraded := meta.FindStatusCondition(wp.Status.Conditions, v1alpha1.ConditionDegraded); degraded != nil && degraded.Status == metav1.ConditionTrue {
		return "pool is degraded: " + degraded.Reason, false
	}
	return "", false
}

func (s *ExecutionScheduler) instances(wp *v1alpha1.WorkerPool) []string {
	if len(wp.Spec.InstanceNames) > 0 {
		return wp.Spec.InstanceNames
	}
	return s.InstanceNames
}

// Execute cannot select a WorkerPool or size class. Use a unique instance and
// exact native/Xcode runner, and fail explicitly if this configuration cannot
// be addressed per pool. Do not cordon peers to manufacture routing or cold starts.
func (s *ExecutionScheduler) target(wp *v1alpha1.WorkerPool, all []v1alpha1.WorkerPool) (ExecutionTarget, error) {
	platform, ok := s.Catalog.Platform(wp.Spec.Platform)
	if !ok || string(platform.Provider) != wp.Spec.Provider {
		return ExecutionTarget{}, errors.New("pool platform is unknown or has a different provider")
	}
	for _, runner := range platform.Runners {
		if runner.Generic || runner.Emulator != "" || len(runner.Properties) == 0 {
			continue
		}
		for _, instance := range s.instances(wp) {
			if !slices.Contains(s.InstanceNames, instance) {
				continue
			}
			unique := true
			for _, peer := range all {
				if peer.Name == wp.Name || !slices.Contains(s.instances(&peer), instance) {
					continue
				}
				if other, ok := s.Catalog.Platform(peer.Spec.Platform); ok {
					for _, r := range other.Runners {
						if maps.Equal(r.Properties, runner.Properties) {
							unique = false
						}
					}
				}
			}
			if unique {
				return ExecutionTarget{Pool: wp.Name, Instance: instance, Platform: maps.Clone(runner.Properties)}, nil
			}
		}
	}
	return ExecutionTarget{}, errors.New("no unique native runner/instance route to this pool (Execute cannot select a pool or size class)")
}
