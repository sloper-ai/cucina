// SPDX-License-Identifier: FSL-1.1-ALv2

package canary

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metric names (append to docs/contracts.md §6; slo/ builds
// cucina:canary_success:ratio_1h on cucina_canary_runs_total).
const (
	MetricRuns        = "cucina_canary_runs_total"
	MetricDuration    = "cucina_canary_duration_seconds"
	MetricStep        = "cucina_canary_step_duration_seconds"
	MetricLastRun     = "cucina_canary_last_run_timestamp_seconds"
	MetricLastSuccess = "cucina_canary_last_success_timestamp_seconds"
	MetricUp          = "cucina_canary_up"
	MetricQueue       = "cucina_canary_exec_queue_seconds"
)

// Metrics are the canary's Prometheus collectors.
type Metrics struct {
	mu     sync.Mutex
	execUp map[string]prometheus.Gauge // known execution health, including pools since deleted

	runs        *prometheus.CounterVec
	duration    *prometheus.HistogramVec
	step        *prometheus.HistogramVec
	lastRun     *prometheus.GaugeVec
	lastSuccess *prometheus.GaugeVec
	up          *prometheus.GaugeVec
	queue       *prometheus.GaugeVec
}

// NewMetrics registers the canary collectors on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		execUp: make(map[string]prometheus.Gauge),
		runs: prometheus.NewCounterVec(prometheus.CounterOpts{Name: MetricRuns,
			Help: "Canary runs by kind (cache, exec), pool and result (success, failure)."}, []string{"kind", "pool", "result"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: MetricDuration,
			Help:    "Canary run duration in seconds.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600}}, []string{"kind", "pool"}),
		step: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: MetricStep,
			Help:    "Canary step duration in seconds.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 10, 60, 300}}, []string{"kind", "step"}),
		lastRun: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: MetricLastRun,
			Help: "Unix time of the last canary run."}, []string{"kind", "pool"}),
		lastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: MetricLastSuccess,
			Help: "Unix time of the last successful canary run."}, []string{"kind", "pool"}),
		up: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: MetricUp,
			Help: "1 if the last canary run succeeded, else 0; scheduled execution also requires current pool availability and deployment evidence."}, []string{"kind", "pool"}),
		queue: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: MetricQueue,
			Help: "Queue time of the last execution canary (a cold-start sample when the pool was at zero), in seconds."}, []string{"pool"}),
	}
	reg.MustRegister(m.runs, m.duration, m.step, m.lastRun, m.lastSuccess, m.up, m.queue)
	return m
}

// Observe records one result.
func (m *Metrics) Observe(r Result) {
	result := "failure"
	if r.Success {
		result = "success"
	}
	m.runs.WithLabelValues(r.Kind, r.Pool, result).Inc()
	m.duration.WithLabelValues(r.Kind, r.Pool).Observe(r.Duration.Seconds())
	for _, s := range r.Steps {
		m.step.WithLabelValues(r.Kind, s.Name).Observe(s.Duration.Seconds())
	}
	m.restore(r)
}

// restore rehydrates durable results after a leadership change without counting
// the same run again in counters and histograms.
func (m *Metrics) restore(r Result) {
	up := 0.0
	if r.Success {
		up = 1
	}
	end := r.Started.Add(r.Duration)
	m.lastRun.WithLabelValues(r.Kind, r.Pool).Set(float64(end.Unix()))
	m.setUp(r.Kind, r.Pool, up)
	if r.Success {
		m.lastSuccess.WithLabelValues(r.Kind, r.Pool).Set(float64(end.Unix()))
		if r.Kind == KindExec {
			m.queue.WithLabelValues(r.Pool).Set(r.QueueTime.Seconds())
		}
	}
}

func (m *Metrics) setUp(kind, pool string, up float64) {
	gauge := m.up.WithLabelValues(kind, pool)
	if kind == KindExec {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.execUp[pool] = gauge
	}
	gauge.Set(up)
}

// maskExecution withdraws current execution health before refreshing inventory
// or on a failed refresh. Historical successes and cache-canary health survive;
// a deleted pool stays non-passing even though its history is still exported.
func (m *Metrics) maskExecution() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, gauge := range m.execUp {
		gauge.Set(0)
	}
}

// Loop runs a probe periodically in-process (the controller's 5-minute cache
// canary) and observes every result.
type Loop struct {
	Probe   func(ctx context.Context) Result
	Every   time.Duration
	Jitter  float64 // fraction of Every added at random (0.1 = up to +10 %)
	Metrics *Metrics
	Log     *slog.Logger
	// After waits (time.After by default; tests use a fake clock).
	After func(time.Duration) <-chan time.Time
	// OnResult, if set, receives each result (e.g. to emit a Kubernetes Event).
	OnResult func(Result)
}

// Run blocks until ctx ends. The first probe runs immediately.
func (l *Loop) Run(ctx context.Context) error {
	after := l.After
	if after == nil {
		after = time.After
	}
	for {
		r := l.Probe(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if l.Metrics != nil {
			l.Metrics.Observe(r)
		}
		if l.Log != nil {
			l.Log.Info("canary", "kind", r.Kind, "pool", r.Pool, "success", r.Success, "duration", r.Duration.String(), "error", r.Error)
		}
		if l.OnResult != nil {
			l.OnResult(r)
		}
		wait := l.Every
		if l.Jitter > 0 {
			wait += time.Duration(rand.Float64() * l.Jitter * float64(l.Every))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-after(wait):
		}
	}
}

// Handler accepts results POSTed by one-shot canary runs (CronJob, helm test,
// post-upgrade hook) and observes them. Mount it on the controller's
// in-cluster metrics listener (e.g. /canary/results); it is not exposed
// outside the cluster.
func Handler(m *Metrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var res Result
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&res); err != nil {
			http.Error(w, "bad result: "+err.Error(), http.StatusBadRequest)
			return
		}
		if res.Kind != KindCache && res.Kind != KindExec {
			http.Error(w, "unknown kind", http.StatusBadRequest)
			return
		}
		m.Observe(res)
		w.WriteHeader(http.StatusNoContent)
	})
}

// Push sends a result to a controller Handler.
func Push(ctx context.Context, url string, r Result, hc *http.Client) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return &pushError{status: resp.Status}
	}
	return nil
}

type pushError struct{ status string }

func (e *pushError) Error() string { return "canary push: " + e.status }
