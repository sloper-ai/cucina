// SPDX-License-Identifier: FSL-1.1-ALv2

package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Services are the concrete clients a scenario uses (SSM hosts, EC2
// inventory, Prometheus, kubectl/helm, cucinactl). The harness treats them as
// opaque; test/e2e/infra provides the implementation.
type Services interface {
	Close() error
}

// SkipError makes a scenario SKIP from inside Run (e.g. a runtime-detected
// missing prerequisite). Use Skip to construct it.
type SkipError struct{ Reason string }

func (e *SkipError) Error() string { return "skip: " + e.Reason }

// Skip returns an error that marks the scenario skipped with a reason.
func Skip(format string, args ...any) error { return &SkipError{Reason: fmt.Sprintf(format, args...)} }

// Context is what a scenario's Run receives.
type Context struct {
	context.Context
	Env      *Env
	Scenario *Scenario
	Result   *Result
	Services Services
	Log      *slog.Logger
	Now      func() time.Time
	// Prior holds results of earlier scenarios in the same run (DependsOn).
	Prior map[string]*Result

	mu  sync.Mutex
	dir string
}

// Dir is the scenario's raw-artifact directory (created on first use).
func (c *Context) Dir() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dir == "" {
		c.dir = filepath.Join(c.Env.ArtifactsDir, c.Result.RunID, c.Scenario.ID)
		_ = os.MkdirAll(c.dir, 0o755)
	}
	return c.dir
}

// Step runs fn as a named, timed step and records it.
func (c *Context) Step(name string, fn func() error) error {
	start := c.Now()
	c.Log.Info("step start", "scenario", c.Scenario.ID, "step", name)
	err := fn()
	sr := StepResult{Name: name, Started: start, Duration: c.Now().Sub(start)}
	if err != nil {
		sr.Error = err.Error()
	}
	c.mu.Lock()
	c.Result.Steps = append(c.Result.Steps, sr)
	c.mu.Unlock()
	c.Log.Info("step end", "scenario", c.Scenario.ID, "step", name, "duration", sr.Duration.String(), "error", sr.Error)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// Metric records a number.
func (c *Context) Metric(name string, v float64, unit string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Result.Metrics == nil {
		c.Result.Metrics = map[string]Metric{}
	}
	c.Result.Metrics[name] = Metric{Value: v, Unit: unit}
}

// Record stores a structured collector output (BEP/exec-log summaries, …)
// in the result's values.
func (c *Context) Record(name string, v any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Result.Values == nil {
		c.Result.Values = map[string]any{}
	}
	c.Result.Values[name] = v
}

// NFR records an NFR measurement.
func (c *Context) NFR(r NFRResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Result.NFRs = append(c.Result.NFRs, r)
}

// Note records a free-text observation for the report.
func (c *Context) Note(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Result.Notes = append(c.Result.Notes, fmt.Sprintf(format, args...))
}

// Event records a timeline event.
func (c *Context) Event(subject, event string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Result.Timeline = append(c.Result.Timeline, TimelineEvent{At: at, Subject: subject, Event: event})
}

// Query records a PromQL query and its (abbreviated) result.
func (c *Context) Query(q QueryRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(q.Result) > 4000 {
		q.Result = q.Result[:4000] + "…"
	}
	c.Result.Queries = append(c.Result.Queries, q)
}

// Check records an inline check result (in addition to declared Post checks).
func (c *Context) Check(r CheckResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Result.Checks = append(c.Result.Checks, r)
}

// AddCost adds measured spend.
func (c *Context) AddCost(item string, usd float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Result.Cost.Items == nil {
		c.Result.Cost.Items = map[string]float64{}
	}
	c.Result.Cost.Items[item] += usd
	c.Result.Cost.MeasuredUSD += usd
}

// Artifact registers a file under Dir() (path may be absolute inside Dir or
// relative to it), hashing it for the report.
func (c *Context) Artifact(name, path string) error {
	if !filepath.IsAbs(path) {
		path = filepath.Join(c.Dir(), path)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(filepath.Join(c.Env.ArtifactsDir, c.Result.RunID), path)
	if err != nil {
		rel = path
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Result.Artifacts = append(c.Result.Artifacts, Artifact{Name: name, Path: rel, SHA256: hex.EncodeToString(h.Sum(nil)), Bytes: n})
	return nil
}

// Fail returns an error describing a failed pass criterion; scenarios
// usually record the NFR/check first and then return Fail to end the run.
func Fail(format string, args ...any) error { return &FailError{Msg: fmt.Sprintf(format, args...)} }

// FailError is a product verdict (StatusFail), as opposed to an environment
// or harness error (StatusError).
type FailError struct{ Msg string }

func (e *FailError) Error() string { return e.Msg }

// Classify maps an error returned by Run to a status.
func Classify(err error) Status {
	var se *SkipError
	var fe *FailError
	switch {
	case err == nil:
		return StatusPass
	case errors.As(err, &se):
		return StatusSkip
	case errors.As(err, &fe):
		return StatusFail
	case errors.Is(err, context.DeadlineExceeded):
		return StatusFail // a timeout is a failed pass criterion
	default:
		return StatusError
	}
}
