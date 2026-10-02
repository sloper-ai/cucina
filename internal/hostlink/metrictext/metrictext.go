// SPDX-License-Identifier: FSL-1.1-ALv2

// Package metrictext defines the bounded, explicit metric-family selection for
// the host relay. It never transports arbitrary exposition, logs or metadata.
package metrictext

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"google.golang.org/protobuf/proto"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
)

const (
	MaxSnapshotBytes = 128 << 10
	MaxScrapeBytes   = 1 << 20
	MaxFamilies      = 64
	MaxSamples       = 4096
	MaxLabels        = 16
	MaxLabelBytes    = 256
)

// Filter selects the supported families from a newly obtained local scrape.
// Selection is explicit; if the selected data exceeds any limit the entire
// scrape fails, rather than sending partial evidence. The controller instead
// calls Validate, which rejects even a single unsupported family.
func Filter(source cucinav1.MetricsSnapshot_Source, raw []byte) ([]byte, error) {
	return normalize(source, raw, true)
}

// Validate validates an on-wire snapshot and returns canonical text. Identity
// labels are forbidden; the controller provides them through HTTP discovery.
func Validate(source cucinav1.MetricsSnapshot_Source, raw []byte) ([]byte, error) {
	return normalize(source, raw, false)
}

func allowed(source cucinav1.MetricsSnapshot_Source, name string) bool {
	switch source {
	case cucinav1.MetricsSnapshot_SOURCE_HOSTD, cucinav1.MetricsSnapshot_SOURCE_HOST_L2, cucinav1.MetricsSnapshot_SOURCE_WORKER:
	default:
		return false
	}
	switch name {
	case "process_resident_memory_bytes", "process_cpu_seconds_total", "go_goroutines", "go_memstats_alloc_bytes", "go_memstats_heap_alloc_bytes":
		return true
	}
	if source == cucinav1.MetricsSnapshot_SOURCE_HOSTD {
		// L2/WAN/disk aggregates are already on the controller's main registry.
		// Do not publish a second copy that would double-count unqualified sums.
		switch name {
		case "cucina_hostd_vms", "cucina_hostd_controller_connected", "cucina_hostd_relay_bytes_total":
			return true
		}
		return false
	}
	switch name {
	case "buildbarn_blobstore_blob_access_operations_blob_size_bytes",
		"buildbarn_blobstore_blob_access_operations_duration_seconds",
		"buildbarn_blobstore_old_current_new_location_blob_map_last_removed_old_block_insertion_time_seconds":
		return true
	case "buildbarn_builder_build_executor_duration_seconds":
		return source == cucinav1.MetricsSnapshot_SOURCE_WORKER
	}
	return false
}

func allowedLabel(name string) bool {
	// The pinned Buildbarn families use these operational labels. None can
	// assign a target identity, discovery metadata, endpoint or arbitrary text.
	switch name {
	case "storage_type", "backend_type", "operation", "grpc_code", "result", "stage", "state", "relay", "direction":
		return true
	}
	return false
}

func normalize(source cucinav1.MetricsSnapshot_Source, raw []byte, filter bool) ([]byte, error) {
	limit := MaxSnapshotBytes
	if filter {
		limit = MaxScrapeBytes
	}
	if len(raw) == 0 || len(raw) > limit {
		return nil, errors.New("metrics body empty or too large")
	}
	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(bytes.NewReader(raw))
	if err != nil {
		return nil, errors.New("malformed metrics text") // do not echo a payload into logs
	}
	var names []string
	for name := range families {
		if allowed(source, name) {
			names = append(names, name)
		} else if !filter {
			return nil, errors.New("unsupported metric family or source")
		}
	}
	if len(names) == 0 || len(names) > MaxFamilies {
		return nil, errors.New("unsupported or excessive metric families")
	}
	sort.Strings(names)
	var out bytes.Buffer
	samples := 0
	for _, name := range names {
		mf := families[name]
		// HELP is not a free-form side channel. Target identity and descriptive
		// metadata are controller-owned, not copied from a guest's exposition.
		mf.Help = proto.String("Relayed " + name + ".")
		seen := map[string]bool{}
		for _, m := range mf.GetMetric() {
			if m.TimestampMs != nil || len(m.Label) > MaxLabels {
				return nil, errors.New("timestamp or excessive labels in metrics")
			}
			sort.Slice(m.Label, func(i, j int) bool { return m.Label[i].GetName() < m.Label[j].GetName() })
			var key strings.Builder
			last := ""
			for _, l := range m.Label {
				if !allowedLabel(l.GetName()) || l.GetName() == last || len(l.GetValue()) > MaxLabelBytes || strings.ContainsAny(l.GetValue(), "\r\n\x00") {
					return nil, errors.New("forbidden, duplicate or excessive metric label")
				}
				last = l.GetName()
				key.WriteString(last + "=" + strconv.Quote(l.GetValue()) + ";")
			}
			if seen[key.String()] {
				return nil, errors.New("duplicate metric sample")
			}
			seen[key.String()] = true
			samples++
			var value float64
			switch mf.GetType() {
			case dto.MetricType_COUNTER:
				value = m.GetCounter().GetValue()
				if value < 0 {
					return nil, errors.New("negative metric counter")
				}
			case dto.MetricType_GAUGE:
				value = m.GetGauge().GetValue()
			case dto.MetricType_HISTOGRAM:
				h := m.GetHistogram()
				value = h.GetSampleSum()
				samples += len(h.GetBucket()) + 1 // buckets, count, sum
				var count uint64
				bound := math.Inf(-1)
				for _, b := range h.GetBucket() {
					if math.IsNaN(b.GetUpperBound()) || b.GetUpperBound() <= bound || b.GetCumulativeCount() < count || b.GetCumulativeCount() > h.GetSampleCount() {
						return nil, errors.New("invalid metric histogram")
					}
					bound, count = b.GetUpperBound(), b.GetCumulativeCount()
				}
			default:
				return nil, errors.New("unsupported metric type")
			}
			if math.IsNaN(value) || math.IsInf(value, 0) || samples > MaxSamples {
				return nil, errors.New("non-finite or excessive metric samples")
			}
		}
		if _, err := expfmt.MetricFamilyToText(&out, mf); err != nil {
			return nil, fmt.Errorf("encoding metrics: %w", err)
		}
		if out.Len() > MaxSnapshotBytes {
			return nil, errors.New("selected metrics exceed snapshot limit")
		}
	}
	return out.Bytes(), nil
}
