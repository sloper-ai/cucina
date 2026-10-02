// SPDX-License-Identifier: FSL-1.1-ALv2

package reconcile

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/metrics"
	"github.com/sloper-ai/cucina/internal/pools"
	"github.com/sloper-ai/cucina/internal/scaling"
)

// Condition reasons beyond the API constants.
const (
	reasonInvalidSpec         = "InvalidSpec"
	reasonPlatformUnknown     = "PlatformUnknown"
	reasonProviderUnavailable = "ProviderUnavailable"
	reasonStarting            = "Starting"
	reasonDeleting            = "Deleting"
	reasonMaxZero             = "MaxZero"
	reasonThrottled           = "Throttled"
	reasonAPIErrors           = "APIErrors"
	reasonSchedulerUnknown    = "SchedulerUnreachable"
)

// PoolStatus computes the WorkerPool status from the resolution outcome and
// the loop's latest snapshot. It only depends on its inputs (now feeds the
// condition transition times), so status writes are idempotent.
func PoolStatus(wp *v1alpha1.WorkerPool, rt *PoolRuntime, resolveErr error, snap Snapshot, haveSnap bool, now time.Time) v1alpha1.WorkerPoolStatus {
	st := *wp.Status.DeepCopy()
	st.ObservedGeneration = wp.Generation
	t := metav1.NewTime(now)
	set := func(typ string, ok bool, reason, msg string) {
		s := metav1.ConditionFalse
		if ok {
			s = metav1.ConditionTrue
		}
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: typ, Status: s, Reason: reason, Message: msg, ObservedGeneration: wp.Generation, LastTransitionTime: t})
	}
	unknown := func(typ, reason, msg string) {
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: typ, Status: metav1.ConditionUnknown, Reason: reason, Message: msg, ObservedGeneration: wp.Generation, LastTransitionTime: t})
	}

	if resolveErr != nil {
		reason := reasonInvalidSpec
		switch {
		case errors.Is(resolveErr, pools.ErrUnknownPlatform):
			reason = reasonPlatformUnknown
		case errors.Is(resolveErr, ErrProviderUnavailable):
			reason = reasonProviderUnavailable
		}
		set(v1alpha1.ConditionReady, false, reason, resolveErr.Error())
		if !haveSnap {
			return st
		}
	}
	if rt != nil {
		st.ImageGeneration = rt.Spec.Generation
		st.ResolvedImage = rt.ImageRef
	}
	if !haveSnap || snap.At.IsZero() {
		if resolveErr == nil {
			unknown(v1alpha1.ConditionReady, reasonStarting, "waiting for the first autoscaler decision")
		}
		return st
	}

	c := snap.Counts
	st.Desired = int32(snap.Desired)
	st.Launching = int32(c[metrics.StateLaunching])
	st.Registered = int32(c[metrics.StateRegistered])
	st.Busy = int32(c[metrics.StateBusy])
	st.Idle = int32(c[metrics.StateIdle])
	st.Draining = int32(c[metrics.StateDraining])
	st.Stopped = int32(c[metrics.StateStopped])
	st.OldGenerationVMs = int32(snap.Status.OldGenerationVMs)
	st.QueueDeclared = snap.Status.QueueDeclared
	st.InstanceSecondsToday = int64(snap.InstanceSecondsToday)
	if !snap.LastScale.IsZero() {
		ls := metav1.NewTime(snap.LastScale)
		st.LastScaleTime = &ls
	}
	s := snap.Status
	if s.CapacityFailure != "" {
		since := s.CapacityFailingSince
		if since.IsZero() {
			since = now
		}
		st.LastCapacityFailure = &v1alpha1.CapacityFailure{Reason: string(s.CapacityFailure), Message: capacityMessage(rt, s), Since: metav1.NewTime(since)}
	}

	queueMsg := "the scheduler has every queue of the pool"
	if !s.QueueDeclared {
		queueMsg = queueNotDeclaredMessage(wp, rt)
	}
	if snap.QueuesKnown {
		set(v1alpha1.ConditionQueueDeclared, s.QueueDeclared, condReason(s.QueueDeclared, "Declared", v1alpha1.ReasonQueueNotDeclared), queueMsg)
	} else {
		unknown(v1alpha1.ConditionQueueDeclared, reasonSchedulerUnknown, "the scheduler's BuildQueueState API did not answer; queues are unknown")
	}

	imgMsg := "image " + st.ResolvedImage
	if rt != nil && rt.ImageErr != nil {
		imgMsg = rt.ImageErr.Error()
	}
	set(v1alpha1.ConditionImageResolved, s.ImageResolved, condReason(s.ImageResolved, "Resolved", v1alpha1.ReasonImageMissing), imgMsg)

	capReason := "Available"
	switch {
	case !s.CapacityAvailable:
		capReason = capacityReason(s.CapacityFailure)
	case wp.Spec.Paused:
		capReason = v1alpha1.ReasonPaused
	}
	set(v1alpha1.ConditionCapacity, s.CapacityAvailable && !wp.Spec.Paused, capReason, capacityMessage(rt, s))

	degraded, degReason, degMsg := false, "Healthy", "no persistent failures"
	switch {
	case s.CapacityFailure != "" && s.CapacityFailure != scaling.ReasonThrottled:
		degraded, degReason, degMsg = true, capacityReason(s.CapacityFailure), capacityMessage(rt, s)
	case c[metrics.StateFailed] > 0:
		degraded, degReason, degMsg = true, v1alpha1.ReasonStartupFailures, fmt.Sprintf("%d VM(s) did not register within the startup timeout", c[metrics.StateFailed])
	case !snap.QueuesKnown:
		degraded, degReason, degMsg = true, reasonSchedulerUnknown, "the scheduler's BuildQueueState API did not answer"
	case snap.LastError != "":
		degraded, degReason, degMsg = true, reasonAPIErrors, snap.LastError
	}
	set(v1alpha1.ConditionDegraded, degraded, degReason, degMsg)

	if resolveErr != nil {
		return st
	}
	ready, reason, msg := true, "Ready", fmt.Sprintf("desired %d, registered %d, launching %d", snap.Desired, c[metrics.StateRegistered], c[metrics.StateLaunching])
	switch {
	case snap.Deleting:
		ready, reason, msg = false, reasonDeleting, "draining and stopping every VM before the pool is removed (R-OPS-3)"
	case wp.Spec.Paused:
		ready, reason, msg = false, v1alpha1.ReasonPaused, "the pool is paused: no new launches"
	case wp.Spec.Capacity.Max == 0:
		ready, reason, msg = false, reasonMaxZero, "capacity.max is 0: queued work is failed fast"
	case !snap.QueuesKnown:
		ready, reason, msg = false, reasonSchedulerUnknown, "the scheduler's BuildQueueState API did not answer"
	case !s.QueueDeclared:
		ready, reason, msg = false, v1alpha1.ReasonQueueNotDeclared, queueMsg
	case !s.ImageResolved:
		ready, reason, msg = false, v1alpha1.ReasonImageMissing, imgMsg
	case !s.CapacityAvailable:
		ready, reason, msg = false, capacityReason(s.CapacityFailure), capacityMessage(rt, s)
	}
	set(v1alpha1.ConditionReady, ready, reason, msg)
	return st
}

func condReason(ok bool, yes, no string) string {
	if ok {
		return yes
	}
	return no
}

func capacityReason(r scaling.Reason) string {
	switch r {
	case scaling.ReasonImageMissing:
		return v1alpha1.ReasonImageMissing
	case scaling.ReasonThrottled:
		return reasonThrottled
	case scaling.ReasonStartupFailures:
		return v1alpha1.ReasonStartupFailures
	case scaling.ReasonLaunchError:
		return "LaunchErrors"
	case "":
		return "Available"
	default:
		return v1alpha1.ReasonNoCapacity
	}
}

func capacityMessage(rt *PoolRuntime, s scaling.Status) string {
	switch s.CapacityFailure {
	case "":
		return "capacity can be obtained"
	case scaling.ReasonImageMissing:
		if rt != nil && rt.ImageErr != nil {
			return rt.ImageErr.Error()
		}
		return "the pool's image cannot be resolved"
	case scaling.ReasonNoCapacity:
		return fmt.Sprintf("no capacity since %s; queued work is failed fast (R-RE-2)", s.CapacityFailingSince.UTC().Format(time.RFC3339))
	default:
		return fmt.Sprintf("capacity errors (%s) since %s; retrying with backoff and alternative instance types", s.CapacityFailure, s.CapacityFailingSince.UTC().Format(time.RFC3339))
	}
}

func queueNotDeclaredMessage(wp *v1alpha1.WorkerPool, rt *PoolRuntime) string {
	var props []string
	if rt != nil {
		for _, r := range rt.Spec.Runners {
			props = append(props, domain.PropertiesKey(r.Properties))
		}
	}
	return fmt.Sprintf("the scheduler has no predeclared queue for platform %q size class %q (%s); add the pool to the chart's values.pools and run helm upgrade (ADR 0002). The pool never launches and its queued work is failed fast",
		wp.Spec.Platform, wp.Spec.SizeClass, strings.Join(props, " | "))
}
