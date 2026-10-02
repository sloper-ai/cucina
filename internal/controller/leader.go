// SPDX-License-Identifier: FSL-1.1-ALv2

package controller

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// LeaderLabel marks the controller Pod that holds the lease. The autoscaler's
// state (VMs, history) lives on the leader only, so the chart's management
// Service selects `cucina.sloper.ai/leader: "true"` to route cucinactl there
// (docs/dev/controller.md).
const LeaderLabel = "cucina.sloper.ai/leader"

// leaderLabeler keeps LeaderLabel on this replica's Pod: "false" at start,
// "true" once elected, "false" again on shutdown. A replica that crashed while
// leading restarts as a follower and resets its label before serving; while it
// is down its readiness probe keeps it out of the Service anyway.
type leaderLabeler struct {
	c       client.Client
	ns, pod string
	elected <-chan struct{}
	log     *slog.Logger
}

// newLeaderLabeler returns nil when the Pod name is unknown (not in a cluster).
func newLeaderLabeler(c client.Client, ns string, elected <-chan struct{}, log *slog.Logger) *leaderLabeler {
	pod := os.Getenv("POD_NAME")
	if pod == "" {
		pod, _ = os.Hostname()
	}
	if p := os.Getenv("POD_NAMESPACE"); p != "" {
		ns = p
	}
	if pod == "" || os.Getenv("KUBERNETES_SERVICE_HOST") == "" {
		return nil
	}
	return &leaderLabeler{c: c, ns: ns, pod: pod, elected: elected, log: log}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable: every replica.
func (l *leaderLabeler) NeedLeaderElection() bool { return false }

// Start implements manager.Runnable.
func (l *leaderLabeler) Start(ctx context.Context) error {
	l.set(ctx, false)
	select {
	case <-ctx.Done():
		return nil
	case <-l.elected:
	}
	l.set(ctx, true)
	<-ctx.Done()
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	l.set(sctx, false)
	return nil
}

func (l *leaderLabeler) set(ctx context.Context, leader bool) {
	v := "false"
	if leader {
		v = "true"
	}
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"labels": map[string]string{LeaderLabel: v}}})
	pod := &corev1.Pod{}
	pod.Name, pod.Namespace = l.pod, l.ns
	for attempt := 0; attempt < 5; attempt++ {
		err := l.c.Patch(ctx, pod, client.RawPatch(types.MergePatchType, patch))
		if err == nil {
			l.log.Info("leader label set", "pod", l.pod, LeaderLabel, v)
			return
		}
		l.log.Warn("setting the leader label failed", "pod", l.pod, "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(attempt+1) * time.Second):
		}
	}
}
