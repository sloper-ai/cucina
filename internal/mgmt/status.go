// SPDX-License-Identifier: FSL-1.1-ALv2

package mgmt

import (
	"context"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/durationpb"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/domain"
	cproto "github.com/sloper-ai/cucina/internal/proto"
)

const (
	// overviewMaxAge is how old a shared overview snapshot may be when a subscriber
	// sends it; it bounds snapshot builds to one per second whatever the number of
	// subscribers.
	overviewMaxAge = time.Second
	// buildTimeout bounds one shared snapshot build.
	buildTimeout = 10 * time.Second

	defaultOverviewInterval = 2 * time.Second
	minOverviewInterval     = time.Second
	maxOverviewInterval     = time.Minute
	recentStarts            = 20
)

// sourceAlert reports a failing data source inside the status instead of failing it.
func sourceAlert(source string, err error, now time.Time) *cucinav1.Alert {
	return alertProto(Alert{
		Name:     "ManagementSourceUnavailable",
		Severity: "warning",
		Summary:  source + ": " + RedactText(err.Error()),
		Since:    now,
		Labels:   map[string]string{"source": source},
	})
}

// GetStatus implements `cucinactl status`.
func (s *Server) GetStatus(ctx context.Context, _ *cucinav1.GetStatusRequest) (*cucinav1.GetStatusResponse, error) {
	now := time.Now()
	out := &cucinav1.GetStatusResponse{
		ClusterId: s.opts.ClusterID,
		Version:   s.opts.Version,
		Protocol:  &cucinav1.ProtocolVersion{Major: cproto.Major, Minor: cproto.Minor},
	}
	pools, err := s.deps.Pools.Pools(ctx)
	if err != nil {
		return nil, fail("pools", err)
	}
	for _, p := range pools {
		out.Pools = append(out.Pools, poolSummary(p, now))
	}
	workers, err := s.deps.Workers.Workers(ctx)
	if err != nil {
		return nil, fail("workers", err)
	}
	for _, w := range workers {
		if countsAsInstance(w.VM) {
			out.WorkerInstances++
		}
	}
	if s.deps.Components != nil {
		comps, err := s.deps.Components.Components(ctx)
		if err != nil {
			out.Alerts = append(out.Alerts, sourceAlert("components", err, now))
		}
		for _, c := range comps {
			out.Components = append(out.Components, &cucinav1.ComponentStatus{
				Name: c.Name, State: c.State, Message: c.Message,
				ReadyReplicas: u32(c.Ready), DesiredReplicas: u32(c.Desired),
			})
		}
	}
	if s.deps.Hosts != nil {
		hosts, err := s.deps.Hosts.Hosts(ctx)
		if err != nil {
			out.Alerts = append(out.Alerts, sourceAlert("hosts", err, now))
		}
		for _, h := range hosts {
			out.HostsTotal++
			if h.Status.Phase == v1alpha1.MacHostOnline {
				out.HostsOnline++
			}
		}
	}
	out.Alerts = append(out.Alerts, s.alerts(ctx, now)...)
	return out, nil
}

func (s *Server) alerts(ctx context.Context, now time.Time) []*cucinav1.Alert {
	if s.deps.Alerts == nil {
		return nil
	}
	as, err := s.deps.Alerts.Alerts(ctx)
	if err != nil {
		return []*cucinav1.Alert{sourceAlert("alerts", err, now)}
	}
	out := make([]*cucinav1.Alert, 0, len(as))
	for _, a := range as {
		out = append(out, alertProto(a))
	}
	return out
}

// queueSummaries lists the scheduler's queues with the pools serving them.
func (s *Server) queueSummaries(ctx context.Context, pools []Pool) ([]*cucinav1.QueueSummary, error) {
	obs, err := s.deps.Scheduler.ListPlatformQueues(ctx)
	if err != nil {
		return nil, err
	}
	serving := map[domain.QueueKey][]string{}
	for _, p := range pools {
		for _, k := range poolQueueKeys(p.Spec, s.opts.InstanceNames) {
			serving[k] = append(serving[k], p.Name())
		}
	}
	var stats map[domain.QueueKey]QueueStat
	if s.deps.QueueStats != nil {
		stats, _ = s.deps.QueueStats.QueueStats(ctx) // timing is best effort
	}
	obs = slices.Clone(obs)
	slices.SortFunc(obs, func(a, b domain.QueueObservation) int { return compareQueueKeys(a.Key, b.Key) })
	out := make([]*cucinav1.QueueSummary, 0, len(obs))
	for _, o := range obs {
		names := slices.Clone(serving[o.Key])
		slices.Sort(names)
		q := &cucinav1.QueueSummary{
			Queue:        queueRef(o.Key),
			Pool:         strings.Join(slices.Compact(names), ","),
			Queued:       u32(o.Queued),
			Executing:    u32(o.Executing),
			IdleWorkers:  u32(o.Idle),
			TotalWorkers: u32(o.Workers),
			Drains:       u32(o.Drains),
		}
		if st, ok := stats[o.Key]; ok {
			q.OldestQueuedAge = dur(st.OldestQueuedAge)
			q.QueueTimeP95 = dur(st.QueueTimeP95)
		}
		out = append(out, q)
	}
	return out, nil
}

// buildOverview assembles one overview snapshot. A failing section becomes an
// alert; the snapshot itself never fails, so the TUI keeps updating.
func (s *Server) buildOverview() (*cucinav1.Overview, error) {
	ctx, cancel := context.WithTimeout(s.base, buildTimeout)
	defer cancel()
	now := time.Now()
	ov := &cucinav1.Overview{Time: ts(now), WorkersByState: map[string]uint32{}}
	var problems []*cucinav1.Alert

	pools, err := s.deps.Pools.Pools(ctx)
	if err != nil {
		problems = append(problems, sourceAlert("pools", err, now))
	}
	for _, p := range pools {
		ov.Pools = append(ov.Pools, poolSummary(p, now))
	}
	if ov.Queues, err = s.queueSummaries(ctx, pools); err != nil {
		problems = append(problems, sourceAlert("scheduler", err, now))
	}
	if workers, err := s.deps.Workers.Workers(ctx); err != nil {
		problems = append(problems, sourceAlert("workers", err, now))
	} else {
		for _, w := range workers {
			if w.State != domain.VMTerminated {
				ov.WorkersByState[workerState(w.VM)]++
			}
		}
	}
	if s.deps.Hosts != nil {
		hosts, err := s.deps.Hosts.Hosts(ctx)
		if err != nil {
			problems = append(problems, sourceAlert("hosts", err, now))
		}
		for _, h := range hosts {
			ov.Hosts = append(ov.Hosts, hostSummary(h))
		}
	}
	if s.deps.Cost != nil {
		if r, err := s.deps.Cost.Cost(ctx, CostQuery{}); err != nil {
			problems = append(problems, sourceAlert("cost", err, now))
		} else {
			ov.Cost = costSummary(r)
		}
	}
	if _, starts, err := s.deps.Pools.History(ctx, "", recentStarts); err != nil {
		problems = append(problems, sourceAlert("history", err, now))
	} else {
		for _, st := range starts {
			ov.RecentStarts = append(ov.RecentStarts, startProto(st))
		}
	}
	ov.Alerts = append(s.alerts(ctx, now), problems...)
	return ov, nil
}

func overviewInterval(d *durationpb.Duration) time.Duration {
	if d == nil || d.AsDuration() <= 0 {
		return defaultOverviewInterval
	}
	return min(max(d.AsDuration(), minOverviewInterval), maxOverviewInterval)
}

// WatchOverview streams an overview every interval (default 2 s, at least 1 s). All
// subscribers share one snapshot per second (overviewMaxAge); a slow client only
// delays its own stream (each stream sends at most its latest snapshot).
func (s *Server) WatchOverview(req *cucinav1.WatchOverviewRequest, stream grpc.ServerStreamingServer[cucinav1.Overview]) error {
	release, err := s.overviewWatchers.acquire("overview watchers")
	if err != nil {
		return err
	}
	defer release()
	ctx := stream.Context()
	if err := stream.SendHeader(metadata.MD{}); err != nil {
		return err
	}
	ticker := time.NewTicker(overviewInterval(req.GetInterval()))
	defer ticker.Stop()
	for {
		ov, _ := s.overview.get(overviewMaxAge, s.buildOverview)
		if err := stream.Send(ov); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return streamEnd(ctx)
		case <-s.done:
			return errStopping
		case <-ticker.C:
		}
	}
}
