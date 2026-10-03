// SPDX-License-Identifier: FSL-1.1-ALv2

package workeragent_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/fakes"
	"github.com/sloper-ai/cucina/internal/workeragent"
	"github.com/sloper-ai/cucina/internal/workeragent/agenttest"
	"github.com/sloper-ai/cucina/internal/workeragent/imds"
)

type activityFunc func() (workeragent.Activity, error)

func (f activityFunc) Observe(context.Context) (workeragent.Activity, error) { return f() }

type probeFunc func() error

func (f probeFunc) Probe(context.Context) error { return f() }

type spotFunc func() *imds.InstanceAction

func (f spotFunc) SpotInstanceAction(context.Context) (*imds.InstanceAction, error) { return f(), nil }

type fakeWorker struct {
	mu     sync.Mutex
	drains int
}

func (w *fakeWorker) Drain(context.Context) error { w.mu.Lock(); w.drains++; w.mu.Unlock(); return nil }

type supervised struct {
	s      *workeragent.Supervisor
	clock  *fakes.Clock
	fs     *fakes.FS
	power  *agenttest.Power
	worker *fakeWorker
	paths  workeragent.Paths
}

func newSupervised(t *testing.T, ws *cucinav1.WorkerSettings, certNotAfter time.Time, lastActivity *time.Time,
	activity workeragent.ActivityProbe, scheduler workeragent.SchedulerProbe, spot workeragent.SpotSource) *supervised {
	t.Helper()
	clock := fakes.NewClock(agenttest.Epoch)
	fs := fakes.NewFS(clock, fakes.NewRand(7), 0)
	paths := workeragent.DefaultPaths("linux")
	require.NoError(t, fs.MkdirAll("/var/lib/cucina", 0o700))
	require.NoError(t, fs.MkdirAll(paths.RunDir, 0o755))
	raw, err := protojson.Marshal(ws)
	require.NoError(t, err)
	require.NoError(t, workeragent.SaveState(fs, paths.StateFile, &workeragent.State{
		Pool: ws.GetPool(), Node: "i-0123456789abcdef0", CertNotAfter: certNotAfter, Settings: raw,
	}))
	if lastActivity != nil {
		require.NoError(t, fs.WriteFileAtomic(paths.LastActivityFile(), []byte(strconv.FormatInt(lastActivity.Unix(), 10)+"\n"), 0o644))
	}
	sv := &supervised{clock: clock, fs: fs, power: &agenttest.Power{}, worker: &fakeWorker{}, paths: paths}
	sv.s = &workeragent.Supervisor{
		Paths: paths, FS: fs, Clock: clock, Host: &agenttest.Host{Clock: clock, Boot: agenttest.Epoch.Add(-time.Minute)},
		Log: slog.New(slog.NewJSONHandler(io.Discard, nil)), Power: sv.power, Worker: sv.worker, Spot: spot,
		NewActivity:  func(*cucinav1.WorkerSettings) workeragent.ActivityProbe { return activity },
		NewScheduler: func(*cucinav1.WorkerSettings) (workeragent.SchedulerProbe, error) { return scheduler, nil },
		Metrics:      workeragent.NewAgentMetrics(),
		Intervals:    workeragent.Intervals{Tick: time.Minute, Spot: time.Minute, Activity: time.Minute, Contact: time.Minute},
	}
	require.NoError(t, sv.s.Start(context.Background()))
	return sv
}

// runUntilPowerOff ticks once per minute of fake time and returns when the
// switch fired (elapsed, reason) or the horizon passed (-1).
func (sv *supervised) runUntilPowerOff(t *testing.T, horizon time.Duration) (time.Duration, string) {
	t.Helper()
	for elapsed := time.Duration(0); elapsed <= horizon; elapsed += time.Minute {
		v, err := sv.s.Tick(context.Background())
		require.NoError(t, err)
		if v.PowerOff {
			require.Equal(t, []string{"deadman: " + v.Reason}, sv.power.Calls())
			return elapsed, v.Reason
		}
		sv.clock.Advance(time.Minute)
	}
	require.Empty(t, sv.power.Calls())
	return -1, ""
}

// Guards: R-POOL-7 / NFR-R4 / §12 "workers self-terminate when orphaned" —
// supervise powers the worker off on its own (no controller involved): idle
// 30 min, scheduler unreachable 10 min, 12 h uptime; busy workers live until
// the maximum uptime; an agent restart does not reset the idle timer.
func TestSupervisorDeadman(t *testing.T) {
	idle := activityFunc(func() (workeragent.Activity, error) { return workeragent.Activity{}, nil })
	busy := activityFunc(func() (workeragent.Activity, error) { return workeragent.Activity{Busy: true}, nil })
	var completed uint64
	completing := activityFunc(func() (workeragent.Activity, error) {
		completed++
		return workeragent.Activity{Completed: completed}, nil
	})
	noMetrics := activityFunc(func() (workeragent.Activity, error) { return workeragent.Activity{}, errors.New("connection refused") })
	ok := probeFunc(func() error { return nil })
	gone := probeFunc(func() error { return errors.New("connection refused") })
	tenMinutes := durationpb.New(5 * time.Minute)
	restartedAt := agenttest.Epoch.Add(-25 * time.Minute)

	cases := []struct {
		name         string
		activity     workeragent.ActivityProbe
		scheduler    workeragent.SchedulerProbe
		idleLimit    *durationpb.Duration
		lastActivity *time.Time
		wantAt       time.Duration
		wantReason   string
	}{
		{name: "controller and scheduler gone", activity: idle, scheduler: gone, wantAt: 10 * time.Minute, wantReason: workeragent.ReasonUnreachable},
		{name: "idle with the scheduler reachable", activity: idle, scheduler: ok, wantAt: 30 * time.Minute, wantReason: workeragent.ReasonIdle},
		{name: "busy until the maximum uptime", activity: busy, scheduler: ok, wantAt: 12*time.Hour - time.Minute, wantReason: workeragent.ReasonMaxUptime},
		{name: "completed actions count as activity", activity: completing, scheduler: ok, wantAt: 12*time.Hour - time.Minute, wantReason: workeragent.ReasonMaxUptime},
		{name: "unobservable bb_worker counts as idle", activity: noMetrics, scheduler: ok, wantAt: 30 * time.Minute, wantReason: workeragent.ReasonIdle},
		{name: "agent restart keeps the idle timer", activity: idle, scheduler: ok, lastActivity: &restartedAt, wantAt: 5 * time.Minute, wantReason: workeragent.ReasonIdle},
		{name: "idle limit from the pool settings", activity: idle, scheduler: ok, idleLimit: tenMinutes, wantAt: 5 * time.Minute, wantReason: workeragent.ReasonIdle},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := agenttest.Settings()
			if tc.idleLimit != nil {
				ws.Deadman.IdleLimit = tc.idleLimit
			}
			sv := newSupervised(t, ws, agenttest.Epoch.Add(24*time.Hour), tc.lastActivity, tc.activity, tc.scheduler, nil)
			at, reason := sv.runUntilPowerOff(t, 13*time.Hour)
			require.Equal(t, tc.wantReason, reason)
			require.Equal(t, tc.wantAt, at)
		})
	}

	// The timestamp files the images' shell/PowerShell timers read: Unix seconds.
	sv := newSupervised(t, agenttest.Settings(), agenttest.Epoch.Add(24*time.Hour), nil, busy, ok, nil)
	sv.clock.Advance(3 * time.Minute)
	_, err := sv.s.Tick(context.Background())
	require.NoError(t, err)
	for _, p := range []string{sv.paths.LastActivityFile(), sv.paths.LastContactFile()} {
		b, err := sv.fs.ReadFile(p)
		require.NoError(t, err)
		require.Equal(t, strconv.FormatInt(agenttest.Epoch.Add(3*time.Minute).Unix(), 10)+"\n", string(b), p)
	}
}

// Guards: R-TEST-7 / R-OBS-1 — the production agent HTTP registry delivers
// current certificate lifetime, including expiration, rather than an uncollected gauge.
func TestSupervisorCertificateMetrics(t *testing.T) {
	sv := newSupervised(t, agenttest.Settings(), agenttest.Epoch.Add(24*time.Hour), nil,
		activityFunc(func() (workeragent.Activity, error) { return workeragent.Activity{}, nil }), probeFunc(func() error { return nil }), nil)
	for _, tc := range []struct {
		advance time.Duration
		value   string
	}{
		{value: "86400"}, {advance: 23 * time.Hour, value: "3600"}, {advance: 2 * time.Hour, value: "-3600"},
	} {
		sv.clock.Advance(tc.advance)
		recorder := httptest.NewRecorder()
		promhttp.HandlerFor(sv.s.Metrics.Registry, promhttp.HandlerOpts{}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		require.Equal(t, http.StatusOK, recorder.Code)
		require.Contains(t, recorder.Body.String(), `cucina_cert_expiry_seconds{role="worker"} `+tc.value+"\n")
		require.False(t, strings.Contains(recorder.Body.String(), `role="host"`), "a worker agent cannot claim a host identity")
	}
	state, err := workeragent.LoadState(sv.fs, sv.paths.StateFile)
	require.NoError(t, err)
	state.CertNotAfter = time.Time{}
	require.NoError(t, workeragent.SaveState(sv.fs, sv.paths.StateFile, state))
	require.NoError(t, sv.s.Start(t.Context()))
	recorder := httptest.NewRecorder()
	promhttp.HandlerFor(sv.s.Metrics.Registry, promhttp.HandlerOpts{}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.NotContains(t, recorder.Body.String(), "cucina_cert_expiry_seconds", "unknown replacement identity must not retain an old expiry or invent zero")
}

// Guards: R-POOL-2 (SHOULD) Spot interruption — the 2-minute notice drains
// bb_worker once; and certificate hygiene — supervise exits loudly when the
// certificate is within 1 h of expiry.
func TestSupervisorSpotAndCertificate(t *testing.T) {
	ok := probeFunc(func() error { return nil })
	busy := activityFunc(func() (workeragent.Activity, error) { return workeragent.Activity{Busy: true}, nil })
	var noticeAt time.Time
	sv := (*supervised)(nil)
	spot := spotFunc(func() *imds.InstanceAction {
		if sv == nil || sv.clock.Now().Before(noticeAt) {
			return nil
		}
		return &imds.InstanceAction{Action: "terminate", Time: noticeAt.Add(2 * time.Minute)}
	})
	for _, handle := range []bool{true, false} {
		ws := agenttest.Settings()
		ws.HandleSpotInterruption = handle
		noticeAt = agenttest.Epoch.Add(3 * time.Minute)
		sv = newSupervised(t, ws, agenttest.Epoch.Add(24*time.Hour), nil, busy, ok, spot)
		for range 10 {
			_, err := sv.s.Tick(context.Background())
			require.NoError(t, err)
			sv.clock.Advance(time.Minute)
		}
		want := 0
		if handle {
			want = 1
		}
		require.Equal(t, want, sv.worker.drains, "handle_spot_interruption=%v", handle)
		require.Empty(t, sv.power.Calls(), "the notice drains; EC2 terminates the instance")
	}

	sv = newSupervised(t, agenttest.Settings(), agenttest.Epoch.Add(90*time.Minute), nil, busy, ok, nil)
	var err error
	elapsed := time.Duration(0)
	for ; elapsed <= time.Hour; elapsed += time.Minute {
		if _, err = sv.s.Tick(context.Background()); err != nil {
			break
		}
		sv.clock.Advance(time.Minute)
	}
	require.ErrorIs(t, err, workeragent.ErrCertExpiring)
	require.Equal(t, 31*time.Minute, elapsed, "first tick with less than 1 h left")
}
