// SPDX-License-Identifier: FSL-1.1-ALv2

package workeragent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/pki"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/internal/workeragent/imds"
)

// ErrCertExpiring is returned by supervise when the worker certificate is
// within an hour of expiry. Certificates outlive the maximum uptime, so this
// only happens when the controller issues too-short certificates; supervise
// exits non-zero loudly (the images' shell dead-man timer still applies).
var ErrCertExpiring = errors.New("worker certificate expires within 1h")

// errPoweringOff ends Start/Run after a power-off was requested.
var errPoweringOff = errors.New("powering off")

// SpotSource reports a pending Spot interruption notice (nil when none).
type SpotSource interface {
	SpotInstanceAction(ctx context.Context) (*imds.InstanceAction, error)
}

// Intervals are the supervise cadences.
type Intervals struct {
	Tick     time.Duration // dead-man evaluation (default 5s)
	Spot     time.Duration // Spot notice poll (default 5s, as AWS recommends)
	Activity time.Duration // bb_worker metrics scrape (default 15s)
	Contact  time.Duration // scheduler probe (default 30s)
}

func (i Intervals) withDefaults() Intervals {
	def := func(v, d time.Duration) time.Duration {
		if v <= 0 {
			return d
		}
		return v
	}
	return Intervals{Tick: def(i.Tick, 5*time.Second), Spot: def(i.Spot, 5*time.Second),
		Activity: def(i.Activity, 15*time.Second), Contact: def(i.Contact, 30*time.Second)}
}

// Supervisor is `cucina-worker-agent supervise`: the dead-man switch
// (R-POOL-7), Spot interruption handling (R-POOL-2), certificate hygiene and
// the agent's own metrics. It needs nothing from the controller.
type Supervisor struct {
	Paths  Paths
	FS     ports.FS
	Clock  ports.Clock
	Host   Host
	Log    *slog.Logger
	Power  Poweroff
	Worker WorkerControl
	// Spot is nil when Spot handling is unavailable (not on EC2).
	Spot SpotSource
	// NewActivity and NewScheduler build the probes from the bootstrap state.
	NewActivity  func(ws *cucinav1.WorkerSettings) ActivityProbe
	NewScheduler func(ws *cucinav1.WorkerSettings) (SchedulerProbe, error)
	Intervals    Intervals
	// Metrics, when set, receives the agent gauges.
	Metrics *AgentMetrics

	log          *slog.Logger
	iv           Intervals
	limits       DeadmanLimits
	settings     *cucinav1.WorkerSettings
	certNotAfter time.Time
	activity     ActivityProbe
	scheduler    SchedulerProbe

	lastActivity, lastContact           time.Time
	nextSpot, nextActivity, nextContact time.Time
	spotHandled                         bool
	prevCompleted                       uint64
	havePrev                            bool
	reachable                           bool
	activityErrLogged                   bool
	unreachableLogged                   bool
}

// Start waits for the bootstrap state, then initialises limits, timestamps and
// probes. While waiting it still enforces the maximum uptime and powers off a
// machine that has not finished bootstrapping within the idle limit.
func (s *Supervisor) Start(ctx context.Context) error {
	s.log = s.Log.With("cmd", "supervise")
	s.iv = s.Intervals.withDefaults()
	var st *State
	logged := false
	for {
		var err error
		st, err = LoadState(s.FS, s.Paths.StateFile)
		if err == nil {
			break
		}
		if !logged {
			s.log.Warn("waiting for bootstrap state", "event", "supervise.wait", "stateFile", s.Paths.StateFile, "error", err.Error())
			logged = true
		}
		up, uerr := s.Host.Uptime()
		if uerr == nil && (up >= DefaultMaxUptime || up >= DefaultIdleLimit) {
			reason := "not-bootstrapped"
			if up >= DefaultMaxUptime {
				reason = ReasonMaxUptime
			}
			s.log.Error("dead-man: no bootstrap state; powering off", "event", "deadman.poweroff", "reason", reason, "uptime", up.String())
			perr := s.Power.PowerOff(ctx, "deadman: "+reason)
			if perr == nil {
				return errPoweringOff
			}
			s.log.Error("poweroff failed; will retry", "event", "poweroff.failed", "error", perr.Error())
		}
		if err := s.Clock.Sleep(ctx, s.iv.Tick); err != nil {
			return err
		}
	}
	ws, err := st.WorkerSettings()
	if err != nil {
		return err
	}
	s.settings = ws
	s.limits = LimitsFromSettings(ws.GetDeadman())
	s.certNotAfter = st.CertNotAfter
	if s.Metrics != nil {
		s.Metrics.setCertificate(s.Clock, st.CertNotAfter)
	}
	now := s.Clock.Now()
	s.lastActivity = s.loadTimestamp(s.Paths.LastActivityFile(), now)
	s.lastContact = s.loadTimestamp(s.Paths.LastContactFile(), now)
	s.activity = s.NewActivity(ws)
	if s.scheduler, err = s.NewScheduler(ws); err != nil {
		return fmt.Errorf("scheduler probe: %w", err)
	}
	s.nextSpot, s.nextActivity, s.nextContact = now, now, now
	s.log.Info("supervising", "event", "supervise.start", "pool", st.Pool, "node", st.Node,
		"idleLimit", s.limits.Idle.String(), "unreachableLimit", s.limits.Unreachable.String(),
		"maxUptime", s.limits.MaxUptime.String(), "spot", ws.GetHandleSpotInterruption() && s.Spot != nil,
		"certNotAfter", s.certNotAfter.UTC().Format(time.RFC3339))
	return nil
}

// loadTimestamp keeps the dead-man clocks across agent restarts: a crash loop
// must not reset the idle or unreachable timers.
func (s *Supervisor) loadTimestamp(path string, now time.Time) time.Time {
	t, err := readTimestamp(s.FS, path)
	if err != nil || t.After(now) {
		_ = writeTimestamp(s.FS, path, now)
		return now
	}
	return t
}

// Tick runs the checks that are due and evaluates the dead-man switch. It
// requests a power-off itself when the verdict says so.
func (s *Supervisor) Tick(ctx context.Context) (DeadmanVerdict, error) {
	now := s.Clock.Now()
	if s.Spot != nil && s.settings.GetHandleSpotInterruption() && !s.spotHandled && !now.Before(s.nextSpot) {
		s.nextSpot = now.Add(s.iv.Spot)
		s.checkSpot(ctx)
	}
	if !now.Before(s.nextActivity) {
		s.nextActivity = now.Add(s.iv.Activity)
		s.observeActivity(ctx, now)
	}
	if !now.Before(s.nextContact) {
		s.nextContact = now.Add(s.iv.Contact)
		s.probeScheduler(ctx, now)
	}
	up, err := s.Host.Uptime()
	if err != nil {
		return DeadmanVerdict{}, fmt.Errorf("uptime: %w", err)
	}
	obs := DeadmanObservation{Uptime: up, SinceActivity: nonNeg(now.Sub(s.lastActivity)), SinceContact: nonNeg(now.Sub(s.lastContact))}
	if s.Metrics != nil {
		s.Metrics.update(obs, s.reachable)
	}
	v := DecideDeadman(s.limits, obs)
	if v.PowerOff {
		s.log.Error("dead-man switch: powering off", "event", "deadman.poweroff", "reason", v.Reason,
			"uptime", obs.Uptime.String(), "sinceActivity", obs.SinceActivity.String(), "sinceContact", obs.SinceContact.String())
		if err := s.Power.PowerOff(context.WithoutCancel(ctx), "deadman: "+v.Reason); err != nil {
			s.log.Error("poweroff failed; will retry", "event", "poweroff.failed", "error", err.Error())
			v.PowerOff = false
		}
	}
	if left := s.certNotAfter.Sub(now); !v.PowerOff && left < minCertValidity {
		s.log.Error("worker certificate about to expire; exiting", "event", "cert.expiring",
			"certNotAfter", s.certNotAfter.UTC().Format(time.RFC3339), "left", left.String())
		return v, ErrCertExpiring
	}
	return v, nil
}

func nonNeg(d time.Duration) time.Duration { return max(d, 0) }

func (s *Supervisor) checkSpot(ctx context.Context) {
	a, err := s.Spot.SpotInstanceAction(ctx)
	if err != nil || a == nil {
		return
	}
	s.log.Warn("spot interruption notice: draining bb_worker", "event", "spot.notice", "action", a.Action,
		"time", a.Time.UTC().Format(time.RFC3339))
	if err := s.Worker.Drain(ctx); err != nil {
		s.log.Error("draining bb_worker failed; will retry", "event", "spot.drain_failed", "error", err.Error())
		return
	}
	s.spotHandled = true
	s.log.Info("bb_worker draining", "event", "spot.drained")
}

func (s *Supervisor) observeActivity(ctx context.Context, now time.Time) {
	a, err := s.activity.Observe(ctx)
	if err != nil {
		if !s.activityErrLogged {
			s.log.Warn("bb_worker metrics unavailable (counts as idle)", "event", "activity.unavailable", "error", err.Error())
			s.activityErrLogged = true
		}
		return
	}
	s.activityErrLogged = false
	if a.Busy || (s.havePrev && a.Completed > s.prevCompleted) {
		s.lastActivity = now
		if err := writeTimestamp(s.FS, s.Paths.LastActivityFile(), now); err != nil {
			s.log.Warn("writing last-activity failed", "event", "timestamp.write_failed", "error", err.Error())
		}
	}
	s.prevCompleted, s.havePrev = a.Completed, true
}

func (s *Supervisor) probeScheduler(ctx context.Context, now time.Time) {
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err := s.scheduler.Probe(pctx)
	cancel()
	if err != nil {
		if !s.unreachableLogged {
			s.log.Warn("scheduler unreachable", "event", "scheduler.unreachable", "error", err.Error())
			s.unreachableLogged = true
		}
		s.reachable = false
		return
	}
	if !s.reachable {
		s.log.Info("scheduler reachable", "event", "scheduler.reachable")
	}
	s.reachable, s.unreachableLogged = true, false
	s.lastContact = now
	if err := writeTimestamp(s.FS, s.Paths.LastContactFile(), now); err != nil {
		s.log.Warn("writing last-contact failed", "event", "timestamp.write_failed", "error", err.Error())
	}
}

// Run is Start followed by Tick every Intervals.Tick until ctx ends, the
// machine powers off, or the certificate is about to expire.
func (s *Supervisor) Run(ctx context.Context) error {
	if err := s.Start(ctx); err != nil {
		if errors.Is(err, errPoweringOff) || ctx.Err() != nil {
			return nil
		}
		return err
	}
	for {
		v, err := s.Tick(ctx)
		if err != nil {
			return err
		}
		if v.PowerOff {
			return nil
		}
		if err := s.Clock.Sleep(ctx, s.iv.Tick); err != nil {
			return nil // shutting down
		}
	}
}

// WipeKeyAtShutdown removes the per-boot private key when the operating system
// is shutting down (supervise calls it when it is stopped). A plain restart of
// the agent keeps the key, because bb_worker re-reads it.
func WipeKeyAtShutdown(ctx context.Context, ex ports.Exec, fs ports.FS, p Paths, log *slog.Logger) {
	if !systemShuttingDown(ctx, ex) {
		return
	}
	if err := fs.Remove(p.KeyFile()); err != nil {
		log.Warn("wiping the worker key failed", "event", "key.wipe_failed", "error", err.Error())
		return
	}
	log.Info("worker key wiped at shutdown", "event", "key.wiped")
}

// AgentMetrics are the agent's own gauges, served on a local port.
type AgentMetrics struct {
	Registry  *prometheus.Registry
	uptime    prometheus.Gauge
	idle      prometheus.Gauge
	reachable prometheus.Gauge
	expiry    *pki.ExpiryTracker
}

// NewAgentMetrics registers cucina_agent_uptime_seconds,
// cucina_agent_idle_seconds and cucina_agent_scheduler_reachable.
func NewAgentMetrics() *AgentMetrics {
	m := &AgentMetrics{
		Registry: prometheus.NewRegistry(),
		uptime: prometheus.NewGauge(prometheus.GaugeOpts{Name: "cucina_agent_uptime_seconds",
			Help: "Time since the worker booted, in seconds."}),
		idle: prometheus.NewGauge(prometheus.GaugeOpts{Name: "cucina_agent_idle_seconds",
			Help: "Time since bb_worker last executed or completed an action, in seconds."}),
		reachable: prometheus.NewGauge(prometheus.GaugeOpts{Name: "cucina_agent_scheduler_reachable",
			Help: "1 if the last scheduler probe succeeded, else 0."}),
	}
	m.Registry.MustRegister(m.uptime, m.idle, m.reachable)
	return m
}

// setCertificate is called when bootstrap state is loaded, using the same
// clock as the supervisor. Re-loading replaces the one worker identity; an
// unknown expiry is absent, not a synthetic zero/expired certificate.
func (m *AgentMetrics) setCertificate(clock ports.Clock, notAfter time.Time) {
	if m.expiry == nil {
		m.expiry = pki.NewExpiryTracker(clock)
		m.Registry.MustRegister(m.expiry)
	}
	if notAfter.IsZero() {
		m.expiry.Delete(pki.ExpiryRoleWorker, "self")
	} else {
		m.expiry.Set(pki.ExpiryRoleWorker, "self", notAfter)
	}
}

func (m *AgentMetrics) update(o DeadmanObservation, reachable bool) {
	m.uptime.Set(o.Uptime.Seconds())
	m.idle.Set(o.SinceActivity.Seconds())
	if reachable {
		m.reachable.Set(1)
	} else {
		m.reachable.Set(0)
	}
}

// Serve serves /metrics on addr until ctx ends.
func (m *AgentMetrics) Serve(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
