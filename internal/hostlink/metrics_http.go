// SPDX-License-Identifier: FSL-1.1-ALv2

package hostlink

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/hostlink/metrictext"
	"github.com/sloper-ai/cucina/internal/pki"
)

const maxMetricsCacheBytes = 16 << 20

// Routes are internal controller metrics-listener endpoints, not public APIs.
const (
	HostMetricsDiscoveryPath = "/sd/hosts"
	HostMetricsPath          = "/metrics/hosts/"
)

type metricTarget struct {
	source cucinav1.MetricsSnapshot_Source
	vm     string
}

type metricSnapshot struct {
	data []byte // immutable validated text; global cache accounts for these bytes
	at   time.Time
}

func sourceName(source cucinav1.MetricsSnapshot_Source) string {
	switch source {
	case cucinav1.MetricsSnapshot_SOURCE_HOSTD:
		return "hostd"
	case cucinav1.MetricsSnapshot_SOURCE_HOST_L2:
		return "host-l2"
	case cucinav1.MetricsSnapshot_SOURCE_WORKER:
		return "worker"
	}
	return ""
}

func targetPath(serial string, key metricTarget) string {
	p := HostMetricsPath + serial + "/" + sourceName(key.source)
	if key.vm != "" {
		p += "/" + key.vm
	}
	return p
}

func targetOwned(h *host, key metricTarget) bool {
	if sourceName(key.source) == "" {
		return false
	}
	if key.source != cucinav1.MetricsSnapshot_SOURCE_WORKER {
		return key.vm == ""
	}
	vm := h.vms[key.vm]
	if vm == nil || vm.GetState() != "running" || vm.GetPool() == "" || h.expectVMs[key.vm] != vm.GetPool() {
		return false
	}
	_, err := pki.VMIdentity(vm.GetPool(), h.serial, key.vm)
	return err == nil
}

func (s *Server) dropMetricSnapshotsLocked(h *host) {
	for key, sample := range h.samples {
		s.metricBytes -= len(sample.data)
		delete(h.samples, key)
	}
}

func (s *Server) pruneMetricSnapshotsLocked() {
	now := s.d.Clock.Now()
	for _, h := range s.hosts {
		for key, sample := range h.samples {
			if h.session == nil || now.Sub(sample.at) >= min(45*time.Second, s.d.StaleAfter) || !targetOwned(h, key) {
				s.metricBytes -= len(sample.data)
				delete(h.samples, key)
			}
		}
	}
}

func (s *Server) receiveMetrics(serial string, sess *session, snapshot *cucinav1.MetricsSnapshot) {
	key := metricTarget{snapshot.GetSource(), snapshot.GetVmName()}
	data, err := metrictext.Validate(key.source, snapshot.GetPrometheusText())
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.hosts[serial]
	if h == nil || h.session != sess {
		return
	}
	s.pruneMetricSnapshotsLocked()
	old := h.samples[key]
	delete(h.samples, key)
	s.metricBytes -= len(old.data)
	if err == nil && (!sess.metricsAllowed || !targetOwned(h, key)) {
		err = errors.New("unowned metric target or unsupported peer")
	}
	if err == nil && key.source == cucinav1.MetricsSnapshot_SOURCE_WORKER {
		workers := 0
		for other := range h.samples {
			if other.source == cucinav1.MetricsSnapshot_SOURCE_WORKER {
				workers++
			}
		}
		if workers >= 2 {
			err = errors.New("more than two worker metric targets")
		}
	}
	if err == nil && (len(h.samples) >= 4 || s.metricBytes+len(data) > maxMetricsCacheBytes) {
		err = errors.New("metric cache capacity exceeded")
	}
	if err != nil {
		// Never include body bytes or metric-owned metadata in diagnostics.
		s.d.Log.Warn("host metrics snapshot rejected", "serial", serial, "reason", err)
		return
	}
	if h.samples == nil {
		h.samples = make(map[metricTarget]metricSnapshot)
	}
	h.samples[key] = metricSnapshot{data: data, at: s.d.Clock.Now()}
	s.metricBytes += len(data)
}

type metricDiscoveryGroup struct {
	Targets []string          `json:"targets"`
	Labels  map[string]string `json:"labels"`
}

// MetricsHandler serves pod-local HTTP SD and independent scrape targets. It
// must be registered only on the internal metrics listener. The chart routes
// both HostService and discovery through the leader Service; deployments that
// distribute sessions must discover every owning pod separately. The SD targets
// always point back to the specific accepting pod, not a load-balanced Service.
func (s *Server) MetricsHandler(namespace string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		s.mu.Lock()
		s.pruneMetricSnapshotsLocked()
		if r.URL.Path == HostMetricsDiscoveryPath {
			address, err := metricsAddress(r)
			if err != nil {
				s.mu.Unlock()
				http.Error(w, "cannot identify this controller metrics listener", http.StatusBadRequest)
				return
			}
			groups := make([]metricDiscoveryGroup, 0)
			for serial, h := range s.hosts {
				for key := range h.samples {
					pool, node := "", serial
					if key.source == cucinav1.MetricsSnapshot_SOURCE_WORKER {
						pool, node = h.vms[key.vm].GetPool(), serial+"/"+key.vm
					}
					groups = append(groups, metricDiscoveryGroup{Targets: []string{address}, Labels: map[string]string{
						"namespace": namespace, "serial": serial, "pool": pool, "node": node,
						"cucina_component": sourceName(key.source), "__metrics_path__": targetPath(serial, key),
					}})
				}
			}
			s.mu.Unlock()
			sort.Slice(groups, func(i, j int) bool {
				return groups[i].Labels["__metrics_path__"] < groups[j].Labels["__metrics_path__"]
			})
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(groups)
			return
		}
		var data []byte
		for serial, h := range s.hosts {
			for key, sample := range h.samples {
				if r.URL.Path == targetPath(serial, key) {
					data = sample.data
					break
				}
			}
		}
		s.mu.Unlock()
		if data == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write(data)
	})
}

func metricsAddress(r *http.Request) (string, error) {
	address := r.Host
	if local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		address = local.String()
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" || strings.ContainsAny(host, "/\\@ \t\r\n") {
		return "", errors.New("invalid metrics address")
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		return "", errors.New("unspecified metrics address")
	}
	p, err := strconv.Atoi(port)
	if err != nil || p <= 0 || p > 65535 {
		return "", errors.New("invalid metrics port")
	}
	return net.JoinHostPort(host, port), nil
}
