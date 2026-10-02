// SPDX-License-Identifier: FSL-1.1-ALv2

package workeragent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// bb_worker metric families the activity probe reads (bb-remote-execution
// 20260930T173749Z-1a3be95):
//   - pkg/filesystem/pool/metrics_file_pool.go: every file the virtual build
//     directory allocates (including each action's stdout/stderr) comes from
//     the file pool, so created − closed > 0 while any action executes;
//   - pkg/builder/metrics_build_executor.go: the duration histogram counts
//     completed actions.
const (
	metricPoolFilesCreated = "buildbarn_filesystem_file_pool_files_created_total"
	metricPoolFilesClosed  = "buildbarn_filesystem_file_pool_files_closed_total"
	metricActionsCompleted = "buildbarn_builder_build_executor_duration_seconds"
)

// Activity is one observation of bb_worker.
type Activity struct {
	// Busy is set while at least one action executes.
	Busy bool
	// Completed is a monotonic count of completed action stages; any increase
	// is activity.
	Completed uint64
}

// ActivityProbe observes bb_worker's local activity.
type ActivityProbe interface {
	Observe(ctx context.Context) (Activity, error)
}

// MetricsActivityProbe scrapes bb_worker's Prometheus endpoint on localhost
// (WorkerSettings.metrics_port). Idleness is never taken from the controller.
type MetricsActivityProbe struct {
	URL    string // http://127.0.0.1:<metrics_port>/metrics
	Client *http.Client
}

// NewMetricsActivityProbe returns a probe for bb_worker's metrics port.
func NewMetricsActivityProbe(port uint32) *MetricsActivityProbe {
	return &MetricsActivityProbe{
		URL:    fmt.Sprintf("http://127.0.0.1:%d/metrics", port),
		Client: &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}},
	}
}

// Observe implements ActivityProbe.
func (p *MetricsActivityProbe) Observe(ctx context.Context) (Activity, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return Activity{}, err
	}
	req.Header.Set("Accept", "text/plain;version=0.0.4")
	resp, err := p.Client.Do(req)
	if err != nil {
		return Activity{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Activity{}, fmt.Errorf("bb_worker metrics: HTTP %d", resp.StatusCode)
	}
	return ParseActivity(io.LimitReader(resp.Body, 32<<20))
}

// ParseActivity derives Activity from a Prometheus text exposition.
func ParseActivity(r io.Reader) (Activity, error) {
	parser := expfmt.NewTextParser(model.UTF8Validation)
	fams, err := parser.TextToMetricFamilies(r)
	if err != nil {
		return Activity{}, fmt.Errorf("bb_worker metrics: %w", err)
	}
	created, closed := sumValues(fams[metricPoolFilesCreated]), sumValues(fams[metricPoolFilesClosed])
	return Activity{
		Busy:      created > closed,
		Completed: uint64(sumValues(fams[metricActionsCompleted])),
	}, nil
}

// sumValues adds counter/gauge/untyped values or histogram sample counts over
// every series of a family.
func sumValues(f *dto.MetricFamily) float64 {
	if f == nil {
		return 0
	}
	var s float64
	for _, m := range f.GetMetric() {
		switch {
		case m.Counter != nil:
			s += m.GetCounter().GetValue()
		case m.Gauge != nil:
			s += m.GetGauge().GetValue()
		case m.Untyped != nil:
			s += m.GetUntyped().GetValue()
		case m.Histogram != nil:
			s += float64(m.GetHistogram().GetSampleCount())
		}
	}
	return s
}
