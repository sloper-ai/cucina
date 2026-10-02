// SPDX-License-Identifier: FSL-1.1-ALv2

package hostlink

import "github.com/prometheus/client_golang/prometheus"

var hostMetricDescs = struct {
	requests, wan, size, free *prometheus.Desc
}{
	requests: prometheus.NewDesc("cucina_hostd_l2_requests_total", "Host L2 CAS reads by result (hit, miss).", []string{"serial", "result"}, nil),
	wan:      prometheus.NewDesc("cucina_hostd_wan_bytes_total", "Bytes between the host L2 and the central endpoint.", []string{"serial", "direction"}, nil),
	size:     prometheus.NewDesc("cucina_hostd_l2_size_bytes", "Configured host L2 cache size.", []string{"serial"}, nil),
	free:     prometheus.NewDesc("cucina_hostd_disk_free_bytes", "Free bytes on the volume holding VMs and the L2.", []string{"serial"}, nil),
}

var _ prometheus.Collector = (*Server)(nil)

// Describe implements prometheus.Collector for the controller registry. These
// are the same L2/disk families hostd exposes locally, labeled with the serial
// authenticated by the host stream (never a label supplied in metrics data).
func (s *Server) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{hostMetricDescs.requests, hostMetricDescs.wan, hostMetricDescs.size, hostMetricDescs.free} {
		ch <- d
	}
}

// Collect exports absolute counters, not increments per scrape. An absent,
// stale or disconnected host produces no samples; zero is a real value, not a
// substitute for unavailable telemetry. Legacy protocol 1.0 hosts contribute
// reliable L2/disk fields, but no WAN samples: their WAN fields counted logical
// blob sizes. No network I/O occurs while scraping.
func (s *Server) Collect(ch chan<- prometheus.Metric) {
	for _, h := range s.metricSamples() {
		ch <- prometheus.MustNewConstMetric(hostMetricDescs.requests, prometheus.CounterValue, float64(h.hits), h.serial, "hit")
		ch <- prometheus.MustNewConstMetric(hostMetricDescs.requests, prometheus.CounterValue, float64(h.misses), h.serial, "miss")
		if h.physicalWAN {
			ch <- prometheus.MustNewConstMetric(hostMetricDescs.wan, prometheus.CounterValue, float64(h.received), h.serial, "received")
			ch <- prometheus.MustNewConstMetric(hostMetricDescs.wan, prometheus.CounterValue, float64(h.sent), h.serial, "sent")
		}
		ch <- prometheus.MustNewConstMetric(hostMetricDescs.size, prometheus.GaugeValue, float64(h.size), h.serial)
		ch <- prometheus.MustNewConstMetric(hostMetricDescs.free, prometheus.GaugeValue, float64(h.free), h.serial)
	}
}

type metricSample struct {
	serial                                   string
	physicalWAN                              bool // protocol 1.0 fields are logical bytes, not transport counters
	hits, misses, received, sent, size, free uint64
}

// Copy scalar values under the session lock, then release it before sending to
// a potentially slow collector channel. Freshness is measured at receipt of a
// metrics heartbeat, not the last command/log or a host-controlled timestamp.
func (s *Server) metricSamples() []metricSample {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.d.Clock.Now()
	out := make([]metricSample, 0, len(s.hosts))
	for _, h := range s.hosts {
		if h.session == nil || h.metrics == nil || now.Sub(h.metricsAt) >= s.d.StaleAfter || now.Sub(h.lastSeen) >= s.d.StaleAfter {
			continue
		}
		m := h.metrics
		out = append(out, metricSample{serial: h.serial, physicalWAN: h.session.metricsAllowed, hits: m.GetL2Hits(), misses: m.GetL2Misses(),
			received: m.GetWanBytesReceived(), sent: m.GetWanBytesSent(), size: m.GetL2SizeBytes(), free: m.GetDiskFreeBytes()})
	}
	return out
}
