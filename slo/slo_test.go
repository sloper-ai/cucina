// SPDX-License-Identifier: FSL-1.1-ALv2

package slo

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Metrics the SLO layer may reference. Cucina names are the API in
// docs/contracts.md §6 (plus the cucina_canary_* family this package's
// canary library exports); the others come from the pinned Buildbarn
// releases (bb-storage blobstore + go-grpc-prometheus, bb-remote-execution
// scheduler/executor) and cAdvisor via the kubelet.
var knownMetrics = map[string]bool{
	"cucina_queue_queued": true, "cucina_vm_start_seconds_bucket": true, "cucina_vm_start_seconds_count": true,
	"cucina_pool_vms": true, "cucina_idle_instances_with_empty_queue": true, "cucina_invariant_violations_total": true,
	"cucina_hostd_wan_bytes_total": true, "cucina_canary_runs_total": true,
	"buildbarn_builder_in_memory_build_queue_tasks_queued_duration_seconds_bucket":                        true,
	"buildbarn_builder_in_memory_build_queue_tasks_queued_duration_seconds_count":                         true,
	"buildbarn_builder_build_executor_duration_seconds_bucket":                                            true,
	"buildbarn_blobstore_blob_access_operations_blob_size_bytes_sum":                                      true,
	"buildbarn_blobstore_blob_access_operations_blob_size_bytes_count":                                    true,
	"buildbarn_blobstore_old_current_new_location_blob_map_last_removed_old_block_insertion_time_seconds": true,
	"grpc_server_handled_total": true, "grpc_server_msg_received_total": true,
	"container_memory_rss": true, "container_oom_events_total": true,
}

// Guards R-TEST-8f: every rule, SLO and threshold references only contract
// metrics or recording rules defined here, with balanced PromQL; rule names
// are unique and in the cucina: namespace.
func TestDefinitionsReferenceKnownMetrics(t *testing.T) {
	records := map[string]bool{}
	for _, r := range Rules {
		require.True(t, strings.HasPrefix(r.Record, "cucina:"), r.Record)
		require.False(t, records[r.Record], "duplicate rule %s", r.Record)
		records[r.Record] = true
	}
	var exprs []string
	for _, r := range Rules {
		exprs = append(exprs, r.Expr)
	}
	for _, s := range SLOs {
		exprs = append(exprs, s.ErrorQuery, s.TotalQuery, s.Check.Query)
	}
	for _, th := range []Threshold{NoInvariantViolations, NoCostLeak, NoOOMKills, RetentionAboveBazelTTL} {
		exprs = append(exprs, th.Query)
	}
	for _, e := range exprs {
		require.True(t, balanced(e), "unbalanced: %s", e)
		for _, m := range MetricNames(e) {
			require.True(t, knownMetrics[m] || records[m], "unknown metric %q in %s", m, e)
		}
	}
}

func balanced(e string) bool {
	var stack []byte
	pairs := map[byte]byte{')': '(', '}': '{', ']': '['}
	inStr := byte(0)
	for i := 0; i < len(e); i++ {
		c := e[i]
		switch {
		case inStr != 0:
			if c == inStr {
				inStr = 0
			}
		case c == '"':
			inStr = c
		case c == '(' || c == '{' || c == '[':
			stack = append(stack, c)
		case c == ')' || c == '}' || c == ']':
			if len(stack) == 0 || stack[len(stack)-1] != pairs[c] {
				return false
			}
			stack = stack[:len(stack)-1]
		}
	}
	return len(stack) == 0 && inStr == 0
}

func TestMetricNames(t *testing.T) {
	require.Equal(t, []string{"a_total", "cucina:x"},
		MetricNames(`sum by (le, pool) (rate(a_total{x="y",z=~"w"}[5m])) / on(pool) cucina:x or vector(0)`))
}

// Guards the chart contract: the checked-in rules.json and sloth.json (which
// the chart copies) are exactly what this package generates. Regenerate with
// `go run ./slo/cmd/slogen`.
func TestGeneratedFilesAreFresh(t *testing.T) {
	for name, gen := range map[string]func() ([]byte, error){"rules.json": RulesFile, "sloth.json": SlothSpec} {
		want, err := gen()
		require.NoError(t, err)
		got, err := os.ReadFile(name)
		require.NoError(t, err)
		require.Equal(t, string(want), string(got), "%s is stale: run go run ./slo/cmd/slogen", name)
	}
}

func TestThresholds(t *testing.T) {
	require.True(t, Threshold{Op: LE, Value: 1}.Holds(1))
	require.False(t, Threshold{Op: LE, Value: 1}.Holds(1.01))
	require.True(t, Threshold{Op: GE, Value: 0.99}.Holds(0.995))
	require.True(t, NoCostLeak.Holds(0))
	require.False(t, NoCostLeak.Holds(1))
}
