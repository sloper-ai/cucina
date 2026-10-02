// SPDX-License-Identifier: FSL-1.1-ALv2

package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Status is a scenario outcome.
type Status string

const (
	StatusPass  Status = "pass"
	StatusFail  Status = "fail"
	StatusSkip  Status = "skip"
	StatusError Status = "error" // harness or environment error (not a product verdict)
)

// Metric is one recorded number.
type Metric struct {
	Value float64 `json:"value"`
	Unit  string  `json:"unit,omitempty"`
}

// NFRResult is one NFR measurement made by a scenario. A scenario may record
// the same NFR several times (e.g. NFR-P1 per pool); the report aggregates.
type NFRResult struct {
	ID       string  `json:"id"`
	Subject  string  `json:"subject,omitempty"` // e.g. "linux", "windows", "macos", configuration name
	Measured float64 `json:"measured"`
	Unit     string  `json:"unit,omitempty"`
	Target   string  `json:"target"`
	Pass     bool    `json:"pass"`
	Detail   string  `json:"detail,omitempty"`
	// Unqualified preserves diagnostic numbers without claiming acceptance.
	Unqualified string `json:"unqualified,omitempty"`
}

// CheckResult is the outcome of one post-condition.
type CheckResult struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Query   string `json:"query,omitempty"`
	Value   string `json:"value,omitempty"`
	Pass    bool   `json:"pass"`
	Skipped string `json:"skipped,omitempty"` // reason when the check could not run
	Detail  string `json:"detail,omitempty"`
}

// StepResult times one named step.
type StepResult struct {
	Name     string        `json:"name"`
	Started  time.Time     `json:"started"`
	Duration time.Duration `json:"duration"`
	Error    string        `json:"error,omitempty"`
}

// QueryRecord is one PromQL query the scenario ran (§10.4: "record the
// PromQL used").
type QueryRecord struct {
	Query  string    `json:"query"`
	At     time.Time `json:"at"`
	Range  string    `json:"range,omitempty"`
	Result string    `json:"result"`
}

// Artifact is a raw file kept under the run's artifacts directory.
type Artifact struct {
	Name   string `json:"name"`
	Path   string `json:"path"` // relative to the artifacts directory
	SHA256 string `json:"sha256,omitempty"`
	Bytes  int64  `json:"bytes"`
}

// CostRecord is the spend attributed to the scenario.
type CostRecord struct {
	Unpriced    []string           `json:"unpriced,omitempty"` // incomplete compute prices; never a zero-cost claim
	EstimateUSD float64            `json:"estimateUSD"`
	MeasuredUSD float64            `json:"measuredUSD"`
	Items       map[string]float64 `json:"items,omitempty"` // e.g. "ec2:c8i.8xlarge", "ebs:gp3"
	// InstanceSeconds by instance type, EBS GB-hours, data transfer bytes.
	InstanceSeconds map[string]float64 `json:"instanceSeconds,omitempty"`
	EBSGBHours      float64            `json:"ebsGbHours,omitempty"`
}

// Result is the JSON report of one scenario run (R-TEST-8d).
type Result struct {
	ID               string            `json:"id"`
	Title            string            `json:"title"`
	Env              string            `json:"env"`
	EnvKind          EnvKind           `json:"envKind"`
	MeasurementScope string            `json:"measurementScope,omitempty"`
	RunID            string            `json:"runId"`
	Status           Status            `json:"status"`
	SkipReason       string            `json:"skipReason,omitempty"`
	Error            string            `json:"error,omitempty"`
	Started          time.Time         `json:"started"`
	Finished         time.Time         `json:"finished"`
	Duration         time.Duration     `json:"duration"`
	CostClass        CostClass         `json:"costClass"`
	Cost             CostRecord        `json:"cost"`
	Tags             map[string]string `json:"tags,omitempty"`
	Steps            []StepResult      `json:"steps,omitempty"`
	Metrics          map[string]Metric `json:"metrics,omitempty"`
	Values           map[string]any    `json:"values,omitempty"` // structured collector outputs
	NFRs             []NFRResult       `json:"nfrs,omitempty"`
	Checks           []CheckResult     `json:"checks,omitempty"`
	Queries          []QueryRecord     `json:"queries,omitempty"`
	Artifacts        []Artifact        `json:"artifacts,omitempty"`
	Logs             []string          `json:"logs,omitempty"`
	Notes            []string          `json:"notes,omitempty"`
	Timeline         []TimelineEvent   `json:"timeline,omitempty"`
}

// TimelineEvent is one timestamped event (instance lifecycle, scale-in, …).
type TimelineEvent struct {
	At      time.Time `json:"at"`
	Subject string    `json:"subject"`
	Event   string    `json:"event"`
}

// ResultFileName is the file a result is stored in.
func ResultFileName(id string) string { return "result-" + id + ".json" }

// Save writes the result as indented JSON into dir.
func (r *Result) Save(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, ResultFileName(r.ID))
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return "", err
	}
	return p, os.Rename(tmp, p)
}

// LoadResults reads every result-*.json in dir, sorted naturally by ID.
func LoadResults(dir string) ([]*Result, error) {
	files, err := filepath.Glob(filepath.Join(dir, "result-*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Result
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var r Result
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		out = append(out, &r)
	}
	sort.Slice(out, func(i, j int) bool { return naturalLess(out[i].ID, out[j].ID) })
	return out, nil
}
