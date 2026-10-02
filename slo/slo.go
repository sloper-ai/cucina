// SPDX-License-Identifier: FSL-1.1-ALv2

// Package slo is the single definition of Cucina's SLIs, recording rules and
// SLOs (R-TEST-7, R-TEST-8f "telemetry as oracle"). Scenarios (test/e2e),
// canaries (internal/canary) and alerts (the chart's PrometheusRules, which
// copy rules.json and sloth.json from this directory) all query the same
// recording rules, so tests, canaries and alerts cannot drift apart.
//
// Metric names come from docs/contracts.md §6 (Cucina) and from the pinned
// Buildbarn releases (bb-storage blobstore and gRPC metrics, bb-remote-
// execution scheduler and executor histograms). Regenerate the JSON files
// with `go run ./slo/cmd/slogen` after changing this file; a static test
// fails if they are stale.
package slo

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Rule is one Prometheus recording rule.
type Rule struct {
	Record string `json:"record"`
	Expr   string `json:"expr"`
	// Help documents the rule (not emitted into the rule file).
	Help string `json:"-"`
}

// Recording rule names (the API that scenarios, canaries and alerts share).
const (
	QueueTimeP95            = "cucina:queue_time_seconds:p95_5m"
	QueueTimeP50            = "cucina:queue_time_seconds:p50_5m"
	QueueDepth              = "cucina:queue_depth"
	FetchInputsP50          = "cucina:worker_fetch_inputs_seconds:p50_5m"
	FetchInputsP95          = "cucina:worker_fetch_inputs_seconds:p95_5m"
	UploadOutputsP50        = "cucina:worker_upload_outputs_seconds:p50_5m"
	UploadOutputsP95        = "cucina:worker_upload_outputs_seconds:p95_5m"
	ColdStartP50            = "cucina:cold_start_seconds:p50_1h"
	ColdStartMax            = "cucina:cold_start_seconds:p99_1h"
	WorkersByState          = "cucina:workers:by_state"
	IdleInstancesEmptyQueue = "cucina:idle_instances_with_empty_queue"
	ACHitRatio              = "cucina:ac_hit_ratio:rate5m"
	BlobBytesRate           = "cucina:blob_bytes:rate5m"
	CASRetention            = "cucina:cas_retention_seconds"
	ControlPlaneRSS         = "cucina:control_plane_rss_bytes"
	PodRSS                  = "cucina:pod_rss_bytes"
	OOMKills                = "cucina:oom_kills:increase1h"
	CanarySuccessRatio      = "cucina:canary_success:ratio_1h"
	InvariantViolations     = "cucina:invariant_violations:increase1h"
	WANBytesRate            = "cucina:hostd_wan_bytes:rate5m"
)

// Rules are the recording rules, in file order.
var Rules = []Rule{
	{Record: QueueTimeP95, Help: "Scheduler queue time p95 per platform queue (NFR-P4).",
		Expr: `histogram_quantile(0.95, sum by (le, instance_name_prefix, platform, size_class) (rate(buildbarn_builder_in_memory_build_queue_tasks_queued_duration_seconds_bucket[5m])))`},
	{Record: QueueTimeP50, Help: "Scheduler queue time p50 per platform queue.",
		Expr: `histogram_quantile(0.50, sum by (le, instance_name_prefix, platform, size_class) (rate(buildbarn_builder_in_memory_build_queue_tasks_queued_duration_seconds_bucket[5m])))`},
	{Record: QueueDepth, Help: "Queued operations per platform queue, as the controller observes them (contracts §6).",
		Expr: `sum by (platform, size_class, instance) (cucina_queue_queued)`},
	{Record: FetchInputsP50, Help: "Worker input-root construction p50 (NFR-P4 per-action overhead, input side).",
		Expr: `histogram_quantile(0.50, sum by (le) (rate(buildbarn_builder_build_executor_duration_seconds_bucket{stage="FetchingInputs"}[5m])))`},
	{Record: FetchInputsP95, Help: "Worker input-root construction p95.",
		Expr: `histogram_quantile(0.95, sum by (le) (rate(buildbarn_builder_build_executor_duration_seconds_bucket{stage="FetchingInputs"}[5m])))`},
	{Record: UploadOutputsP50, Help: "Worker output upload p50 (NFR-P4 per-action overhead, output side).",
		Expr: `histogram_quantile(0.50, sum by (le) (rate(buildbarn_builder_build_executor_duration_seconds_bucket{stage="UploadingOutputs"}[5m])))`},
	{Record: UploadOutputsP95, Help: "Worker output upload p95.",
		Expr: `histogram_quantile(0.95, sum by (le) (rate(buildbarn_builder_build_executor_duration_seconds_bucket{stage="UploadingOutputs"}[5m])))`},
	{Record: ColdStartP50, Help: "VM start to first action p50 per pool over 1 h (NFR-P1).",
		Expr: `histogram_quantile(0.50, sum by (le, pool) (rate(cucina_vm_start_seconds_bucket{phase="to_first_action"}[1h])))`},
	{Record: ColdStartMax, Help: "VM start to first action p99 per pool over 1 h (NFR-P1 max proxy).",
		Expr: `histogram_quantile(0.99, sum by (le, pool) (rate(cucina_vm_start_seconds_bucket{phase="to_first_action"}[1h])))`},
	{Record: WorkersByState, Help: "Worker VMs per pool and state (contracts §6).",
		Expr: `sum by (pool, state) (cucina_pool_vms)`},
	{Record: IdleInstancesEmptyQueue, Help: "Instances running with an empty queue beyond idleTimeout + drain grace (cost leak, NFR-C1).",
		Expr: `sum by (pool) (cucina_idle_instances_with_empty_queue)`},
	{Record: ACHitRatio, Help: "Action cache hit ratio at the frontend (GetActionResult OK over OK+NotFound).",
		Expr: `sum(rate(grpc_server_handled_total{grpc_service="build.bazel.remote.execution.v2.ActionCache",grpc_method="GetActionResult",grpc_code="OK"}[5m])) / sum(rate(grpc_server_handled_total{grpc_service="build.bazel.remote.execution.v2.ActionCache",grpc_method="GetActionResult",grpc_code=~"OK|NotFound"}[5m]))`},
	{Record: BlobBytesRate, Help: "Blob bytes per second per storage type, backend and operation (bytes per tier, R-DATA-7).",
		Expr: `sum by (job, storage_type, backend_type, operation) (rate(buildbarn_blobstore_blob_access_operations_blob_size_bytes_sum[5m]))`},
	{Record: CASRetention, Help: "Conservative persistent L3 CAS retention per storage pod; worker L1 and host L2 are excluded. Before eviction this is map-construction/restart age, not infinite retention.",
		Expr: `time() - max by (namespace, pod) (buildbarn_blobstore_old_current_new_location_blob_map_last_removed_old_block_insertion_time_seconds{cucina_component="storage",storage_type="cas"})`},
	{Record: PodRSS, Help: "RSS per Cucina pod, page cache excluded (NFR-M1/M2).",
		Expr: `sum by (namespace, pod) (container_memory_rss{container!="",container!="POD"})`},
	{Record: ControlPlaneRSS, Help: "Total RSS across the Cucina namespace's pods (NFR-M1).",
		Expr: `sum by (namespace) (container_memory_rss{container!="",container!="POD"})`},
	{Record: OOMKills, Help: "OOM kills per namespace over 1 h (NFR-M1: none during the campaign).",
		Expr: `sum by (namespace) (increase(container_oom_events_total[1h]))`},
	{Record: CanarySuccessRatio, Help: "Canary success ratio per kind and pool over 1 h (R-TEST-7).",
		Expr: `sum by (kind, pool) (increase(cucina_canary_runs_total{result="success"}[1h])) / sum by (kind, pool) (increase(cucina_canary_runs_total[1h]))`},
	{Record: InvariantViolations, Help: "Invariant violations over 1 h (R-TEST-7; must stay 0).",
		Expr: `sum by (invariant) (increase(cucina_invariant_violations_total[1h]))`},
	{Record: WANBytesRate, Help: "Mac host WAN bytes per second by direction (NFR-T6, R-DATA-7).",
		Expr: `sum by (direction) (rate(cucina_hostd_wan_bytes_total[5m]))`},
}

// RuleByName returns the recording rule with the given name.
func RuleByName(record string) (Rule, bool) {
	for _, r := range Rules {
		if r.Record == record {
			return r, true
		}
	}
	return Rule{}, false
}

// Op is a threshold comparison.
type Op string

const (
	LE Op = "<="
	GE Op = ">="
	EQ Op = "=="
)

// Threshold is a scenario post-condition / alert condition over a query.
type Threshold struct {
	Query string  `json:"query"`
	Op    Op      `json:"op"`
	Value float64 `json:"value"`
	Unit  string  `json:"unit,omitempty"`
}

// Holds reports whether v satisfies the threshold.
func (t Threshold) Holds(v float64) bool {
	switch t.Op {
	case LE:
		return v <= t.Value
	case GE:
		return v >= t.Value
	case EQ:
		return v == t.Value
	}
	return false
}

func (t Threshold) String() string {
	return fmt.Sprintf("%s %s %g%s", t.Query, t.Op, t.Value, unitSuffix(t.Unit))
}

func unitSuffix(u string) string {
	if u == "" {
		return ""
	}
	return " " + u
}

// SLO is one objective. Error/Total are Sloth event queries with the
// {{.window}} placeholder; Check is the instant post-condition scenarios use.
type SLO struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Objective   float64   `json:"objective"` // percent
	ErrorQuery  string    `json:"errorQuery"`
	TotalQuery  string    `json:"totalQuery"`
	Check       Threshold `json:"check"`
	Alert       string    `json:"alert"`
	// Requirement names what the SLO protects.
	Requirement string `json:"requirement"`
}

// SLOs are Cucina's objectives (UC16, R-TEST-7). Histogram bucket
// boundaries used in event SLIs (le="1", le="90") must exist in the
// corresponding histograms (Buildbarn's decimal-exponential buckets include
// 1 s; cucina_vm_start_seconds must include 45/60/90/120/180 s).
var SLOs = []SLO{
	{
		Name: "queue-time", Requirement: "NFR-P4", Objective: 99,
		Description: "Operations wait at most 1 s in the scheduler queue while free slots exist.",
		ErrorQuery:  `sum(rate(buildbarn_builder_in_memory_build_queue_tasks_queued_duration_seconds_count[{{.window}}])) - sum(rate(buildbarn_builder_in_memory_build_queue_tasks_queued_duration_seconds_bucket{le="1"}[{{.window}}]))`,
		TotalQuery:  `sum(rate(buildbarn_builder_in_memory_build_queue_tasks_queued_duration_seconds_count[{{.window}}]))`,
		Check:       Threshold{Query: "max(" + QueueTimeP95 + ")", Op: LE, Value: 1, Unit: "s"},
		Alert:       "CucinaQueueTimeHigh",
	},
	{
		Name: "cold-start-linux", Requirement: "NFR-P1", Objective: 95,
		Description: "Linux VMs execute their first action within 90 s of launch.",
		ErrorQuery:  `sum(rate(cucina_vm_start_seconds_count{phase="to_first_action",pool=~"linux.*"}[{{.window}}])) - sum(rate(cucina_vm_start_seconds_bucket{phase="to_first_action",pool=~"linux.*",le="90"}[{{.window}}]))`,
		TotalQuery:  `sum(rate(cucina_vm_start_seconds_count{phase="to_first_action",pool=~"linux.*"}[{{.window}}]))`,
		Check:       Threshold{Query: `max(` + ColdStartP50 + `{pool=~"linux.*"})`, Op: LE, Value: 60, Unit: "s"},
		Alert:       "CucinaColdStartSlow",
	},
	{
		Name: "cache-canary", Requirement: "R-TEST-7", Objective: 99.5,
		Description: "The cache canary (AC/CAS round trip + token mint/verify) succeeds.",
		ErrorQuery:  `sum(rate(cucina_canary_runs_total{kind="cache",result="failure"}[{{.window}}]))`,
		TotalQuery:  `sum(rate(cucina_canary_runs_total{kind="cache"}[{{.window}}]))`,
		Check:       Threshold{Query: `min(` + CanarySuccessRatio + `{kind="cache"})`, Op: GE, Value: 0.995},
		Alert:       "CucinaCacheCanaryFailing",
	},
	{
		Name: "cache-hit-ratio", Requirement: "UC16", Objective: 90,
		Description: "Action cache lookups hit (alerts on a hit-rate drop).",
		ErrorQuery:  `sum(rate(grpc_server_handled_total{grpc_service="build.bazel.remote.execution.v2.ActionCache",grpc_method="GetActionResult",grpc_code="NotFound"}[{{.window}}]))`,
		TotalQuery:  `sum(rate(grpc_server_handled_total{grpc_service="build.bazel.remote.execution.v2.ActionCache",grpc_method="GetActionResult",grpc_code=~"OK|NotFound"}[{{.window}}]))`,
		Check:       Threshold{Query: ACHitRatio, Op: GE, Value: 0.5},
		Alert:       "CucinaCacheHitRateDrop",
	},
}

// Invariant thresholds scenarios assert after every run (production guards).
var (
	NoInvariantViolations = Threshold{Query: `sum(` + InvariantViolations + `) or vector(0)`, Op: EQ, Value: 0}
	NoCostLeak            = Threshold{Query: `sum(` + IdleInstancesEmptyQueue + `) or vector(0)`, Op: EQ, Value: 0}
	NoOOMKills            = Threshold{Query: `sum(` + OOMKills + `) or vector(0)`, Op: EQ, Value: 0}
	// RetentionAboveBazelTTL: CAS retention must exceed --experimental_remote_cache_ttl (3 h) with margin.
	RetentionAboveBazelTTL = Threshold{Query: `min(` + CASRetention + `)`, Op: GE, Value: (4 * time.Hour).Seconds(), Unit: "s"}
)

// ruleFile is the Prometheus rule-file shape (JSON is valid YAML).
type ruleFile struct {
	Groups []ruleGroup `json:"groups"`
}

type ruleGroup struct {
	Name     string `json:"name"`
	Interval string `json:"interval,omitempty"`
	Rules    []Rule `json:"rules"`
}

// RulesFile renders the recording rules as a Prometheus rule file.
func RulesFile() ([]byte, error) {
	b, err := json.MarshalIndent(ruleFile{Groups: []ruleGroup{{Name: "cucina-recording", Interval: "30s", Rules: Rules}}}, "", "  ")
	return append(b, '\n'), err
}

// slothSpec is the Sloth v1 (prometheus/v1) spec shape.
type slothSpec struct {
	Version string            `json:"version"`
	Service string            `json:"service"`
	Labels  map[string]string `json:"labels"`
	SLOs    []slothSLO        `json:"slos"`
}

type slothSLO struct {
	Name        string            `json:"name"`
	Objective   float64           `json:"objective"`
	Description string            `json:"description"`
	Labels      map[string]string `json:"labels,omitempty"`
	SLI         struct {
		Events struct {
			ErrorQuery string `json:"error_query"`
			TotalQuery string `json:"total_query"`
		} `json:"events"`
	} `json:"sli"`
	Alerting struct {
		Name        string            `json:"name"`
		Labels      map[string]string `json:"labels,omitempty"`
		Annotations map[string]string `json:"annotations,omitempty"`
		PageAlert   map[string]any    `json:"page_alert"`
		TicketAlert map[string]any    `json:"ticket_alert"`
	} `json:"alerting"`
}

// SlothSpec renders the SLOs as a Sloth prometheus/v1 spec (JSON).
func SlothSpec() ([]byte, error) {
	spec := slothSpec{Version: "prometheus/v1", Service: "cucina", Labels: map[string]string{"owner": "cucina"}}
	for _, s := range SLOs {
		var o slothSLO
		o.Name, o.Objective, o.Description = s.Name, s.Objective, s.Description
		o.Labels = map[string]string{"requirement": s.Requirement}
		o.SLI.Events.ErrorQuery, o.SLI.Events.TotalQuery = s.ErrorQuery, s.TotalQuery
		o.Alerting.Name = s.Alert
		o.Alerting.Annotations = map[string]string{"summary": s.Description}
		o.Alerting.PageAlert = map[string]any{"labels": map[string]string{"severity": "critical"}}
		o.Alerting.TicketAlert = map[string]any{"labels": map[string]string{"severity": "warning"}}
		spec.SLOs = append(spec.SLOs, o)
	}
	b, err := json.MarshalIndent(spec, "", "  ")
	return append(b, '\n'), err
}

// MetricNames returns the distinct metric (and recording-rule) names an
// expression references: identifiers that are not functions, keywords,
// grouping labels, label matchers or durations. The static test uses it to
// check every referenced metric against docs/contracts.md §6, the pinned
// Buildbarn/gRPC/cAdvisor metrics and the recording rules.
func MetricNames(expr string) []string {
	toks := lex(expr)
	seen := map[string]bool{}
	var out []string
	depth := 0 // inside {...} or [...]
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		switch t {
		case "{", "[":
			depth++
			continue
		case "}", "]":
			depth--
			continue
		}
		if depth > 0 || !isIdent(t) {
			continue
		}
		next := ""
		if i+1 < len(toks) {
			next = toks[i+1]
		}
		if grouping[t] && next == "(" {
			for i < len(toks) && toks[i] != ")" { // skip the label list
				i++
			}
			continue
		}
		if keywords[t] || next == "(" || (t[0] >= '0' && t[0] <= '9') {
			continue
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

func isIdent(t string) bool {
	c := t[0]
	return c == '_' || c == ':' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func identByte(c byte) bool {
	return c == '_' || c == ':' || c == '.' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// lex splits a PromQL expression into identifiers and single-byte
// punctuation, dropping whitespace and string literals.
func lex(expr string) []string {
	var toks []string
	for i := 0; i < len(expr); {
		c := expr[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n':
			i++
		case c == '"' || c == '\'' || c == '`':
			j := i + 1
			for j < len(expr) && expr[j] != c {
				if expr[j] == '\\' {
					j++
				}
				j++
			}
			i = j + 1
		case identByte(c):
			j := i
			for j < len(expr) && identByte(expr[j]) {
				j++
			}
			toks = append(toks, expr[i:j])
			i = j
		default:
			toks = append(toks, string(c))
			i++
		}
	}
	return toks
}

var grouping = map[string]bool{"by": true, "without": true, "on": true, "ignoring": true, "group_left": true, "group_right": true}

var keywords = map[string]bool{
	"sum": true, "max": true, "min": true, "avg": true, "count": true, "or": true, "and": true, "unless": true,
	"offset": true, "bool": true, "by": true, "without": true, "on": true, "ignoring": true,
}
