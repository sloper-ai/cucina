// SPDX-License-Identifier: FSL-1.1-ALv2

// Package bep summarises a Bazel Build Event Protocol stream written with
// --build_event_json_file (newline-delimited protojson) or
// --build_event_binary_file (length-delimited protobuf) into the client
// metrics of §10.4: wall time, critical path (TimingMetrics and the "critical
// path" tool log), remote executions vs cache hits (ActionSummary.runner_count),
// network bytes (NetworkMetrics), Bazel heap (MemoryMetrics, populated with
// --memory_profile), and test outcomes including tests that needed retries.
package bep

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protodelim"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/sloper-ai/cucina/test/e2e/third_party/bazel/bespb"
)

// Well-known runner names in ActionSummary.runner_count (Bazel 9.2).
const (
	RunnerTotal          = "total"
	RunnerInternal       = "internal"
	RunnerRemote         = "remote"
	RunnerRemoteCacheHit = "remote cache hit"
	RunnerDiskCacheHit   = "disk cache hit"
)

// RunnerCount mirrors ActionSummary.RunnerCount.
type RunnerCount struct {
	Name     string `json:"name"`
	Count    int    `json:"count"`
	ExecKind string `json:"execKind,omitempty"`
}

// Test is the outcome of one test target (TestSummary plus its attempts).
type Test struct {
	Label    string        `json:"label"`
	Status   string        `json:"status"`
	Attempts int           `json:"attempts"`
	Runs     int           `json:"runs"`
	Cached   int           `json:"cached"`
	Duration time.Duration `json:"duration"`
	// Strategy/Remote describe the last attempt (TestResult.execution_info).
	Strategy string `json:"strategy,omitempty"`
	Remote   bool   `json:"cachedRemotely,omitempty"`
}

// Retried reports whether the test needed Bazel's retries (Abseil `flaky`).
func (t Test) Retried() bool { return t.Status == "FLAKY" || t.Attempts > 1 }

// Summary is the per-invocation result.
type Summary struct {
	Command          string        `json:"command"`
	UUID             string        `json:"uuid"`
	BuildToolVersion string        `json:"buildToolVersion"`
	Started          time.Time     `json:"started"`
	Finished         time.Time     `json:"finished"`
	ExitCode         int32         `json:"exitCode"`
	ExitName         string        `json:"exitName"`
	WallTime         time.Duration `json:"wallTime"`
	CPUTime          time.Duration `json:"cpuTime"`
	AnalysisTime     time.Duration `json:"analysisTime"`
	ExecutionTime    time.Duration `json:"executionTime"`
	CriticalPath     time.Duration `json:"criticalPath"`
	// CriticalPathLog is the first line of the "critical path" tool log, which
	// carries Bazel's remote breakdown (queue, setup, upload, fetch, …).
	CriticalPathLog string        `json:"criticalPathLog,omitempty"`
	ProcessStats    string        `json:"processStats,omitempty"`
	ActionsCreated  int64         `json:"actionsCreated"`
	ActionsExecuted int64         `json:"actionsExecuted"`
	Runners         []RunnerCount `json:"runners"`
	// Heap statistics (MemoryMetrics; need --memory_profile).
	PeakPostGCHeapBytes  int64    `json:"peakPostGcHeapBytes"`
	UsedHeapPostBuild    int64    `json:"usedHeapPostBuildBytes"`
	NetworkBytesSent     uint64   `json:"networkBytesSent"`
	NetworkBytesReceived uint64   `json:"networkBytesReceived"`
	Tests                []Test   `json:"tests,omitempty"`
	Aborted              []string `json:"aborted,omitempty"`
	// Events is the number of build events read; LastMessage reports whether
	// the stream was complete.
	Events      int  `json:"events"`
	LastMessage bool `json:"lastMessage"`
}

// Success reports a zero exit code from a complete stream.
func (s *Summary) Success() bool { return s.LastMessage && s.ExitCode == 0 && s.ExitName == "SUCCESS" }

// Runner returns the count for a runner name (0 if absent).
func (s *Summary) Runner(name string) int {
	for _, r := range s.Runners {
		if r.Name == name {
			return r.Count
		}
	}
	return 0
}

// RemoteExecutions is the number of actions a remote worker executed.
func (s *Summary) RemoteExecutions() int { return s.Runner(RunnerRemote) }

// RemoteCacheHits is the number of actions served from the remote AC.
func (s *Summary) RemoteCacheHits() int { return s.Runner(RunnerRemoteCacheHit) }

// Spawns is the number of non-internal actions (total minus internal).
func (s *Summary) Spawns() int { return s.Runner(RunnerTotal) - s.Runner(RunnerInternal) }

// LocalExecutions counts actions that ran on the client (exec kind "Local"),
// excluding cache hits.
func (s *Summary) LocalExecutions() int {
	n := 0
	for _, r := range s.Runners {
		if r.ExecKind == "Local" && !strings.Contains(r.Name, "cache hit") {
			n += r.Count
		}
	}
	return n
}

// RemoteCacheHitRatio is remote cache hits over all non-internal spawns
// (NFR-P3, T2); NaN-free: 0 when there were no spawns.
func (s *Summary) RemoteCacheHitRatio() float64 {
	if n := s.Spawns(); n > 0 {
		return float64(s.RemoteCacheHits()) / float64(n)
	}
	return 0
}

// RemoteRatio is the share of non-internal spawns executed remotely or served
// from the remote cache (T22: ≥ 95 % of actions run remotely).
func (s *Summary) RemoteRatio() float64 {
	if n := s.Spawns(); n > 0 {
		return float64(s.RemoteExecutions()+s.RemoteCacheHits()) / float64(n)
	}
	return 0
}

// RetriedTests lists tests that needed retries.
func (s *Summary) RetriedTests() []string {
	var out []string
	for _, t := range s.Tests {
		if t.Retried() {
			out = append(out, t.Label)
		}
	}
	return out
}

// FailedTests lists tests whose overall status is not PASSED/FLAKY.
func (s *Summary) FailedTests() []string {
	var out []string
	for _, t := range s.Tests {
		if t.Status != "PASSED" && t.Status != "FLAKY" {
			out = append(out, t.Label)
		}
	}
	return out
}

// ReadFile parses a BEP file, detecting JSON vs binary from its first bytes:
// a JSON stream starts with `{"`, while a binary stream starts with a varint
// length followed by the tag of BuildEvent.id (0x0a) — a 123-byte first
// event makes the first byte '{' too, so one byte is not enough.
func ReadFile(path string) (*Summary, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	br := bufio.NewReaderSize(f, 1<<20)
	head, err := br.Peek(2)
	if err != nil {
		return nil, fmt.Errorf("bep: %s: %w", path, err)
	}
	if head[0] == '{' && head[1] == '"' {
		return ReadJSON(br)
	}
	return ReadBinary(br)
}

// ReadJSON parses a --build_event_json_file stream.
func ReadJSON(r io.Reader) (*Summary, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 256<<20)
	opts := protojson.UnmarshalOptions{DiscardUnknown: true}
	acc := newAccumulator()
	line := 0
	for sc.Scan() {
		line++
		b := bytes.TrimSpace(sc.Bytes())
		if len(b) == 0 {
			continue
		}
		ev := &bespb.BuildEvent{}
		if err := opts.Unmarshal(b, ev); err != nil {
			return nil, fmt.Errorf("bep: line %d: %w", line, err)
		}
		acc.add(ev)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("bep: %w", err)
	}
	return acc.summary(), nil
}

// ReadBinary parses a --build_event_binary_file stream.
func ReadBinary(r io.Reader) (*Summary, error) {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReaderSize(r, 1<<20)
	}
	opts := protodelim.UnmarshalOptions{MaxSize: 256 << 20}
	acc := newAccumulator()
	for {
		ev := &bespb.BuildEvent{}
		if err := opts.UnmarshalFrom(br, ev); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("bep: event %d: %w", acc.s.Events, err)
		}
		acc.add(ev)
	}
	return acc.summary(), nil
}

type accumulator struct {
	s        Summary
	tests    map[string]*Test
	testKeys []string
}

func newAccumulator() *accumulator { return &accumulator{tests: map[string]*Test{}} }

func (a *accumulator) test(label string) *Test {
	t, ok := a.tests[label]
	if !ok {
		t = &Test{Label: label}
		a.tests[label] = t
		a.testKeys = append(a.testKeys, label)
	}
	return t
}

func (a *accumulator) add(ev *bespb.BuildEvent) {
	s := &a.s
	s.Events++
	if ev.GetLastMessage() {
		s.LastMessage = true
	}
	switch p := ev.GetPayload().(type) {
	case *bespb.BuildEvent_Started:
		st := p.Started
		s.Command, s.UUID, s.BuildToolVersion = st.GetCommand(), st.GetUuid(), st.GetBuildToolVersion()
		if ts := st.GetStartTime(); ts != nil {
			s.Started = ts.AsTime()
		}
	case *bespb.BuildEvent_UnstructuredCommandLine:
		// Deliberately ignored: it carries the whole client environment
		// (--client_env=…), which must never reach a report.
	case *bespb.BuildEvent_Aborted:
		s.Aborted = append(s.Aborted, p.Aborted.GetReason().String()+": "+p.Aborted.GetDescription())
	case *bespb.BuildEvent_Finished:
		f := p.Finished
		s.ExitCode, s.ExitName = f.GetExitCode().GetCode(), f.GetExitCode().GetName()
		if ts := f.GetFinishTime(); ts != nil {
			s.Finished = ts.AsTime()
		}
	case *bespb.BuildEvent_BuildMetrics:
		m := p.BuildMetrics
		as := m.GetActionSummary()
		s.ActionsCreated, s.ActionsExecuted = as.GetActionsCreated(), as.GetActionsExecuted()
		s.Runners = s.Runners[:0]
		for _, rc := range as.GetRunnerCount() {
			s.Runners = append(s.Runners, RunnerCount{Name: rc.GetName(), Count: int(rc.GetCount()), ExecKind: rc.GetExecKind()})
		}
		tm := m.GetTimingMetrics()
		s.WallTime = time.Duration(tm.GetWallTimeInMs()) * time.Millisecond
		s.CPUTime = time.Duration(tm.GetCpuTimeInMs()) * time.Millisecond
		s.AnalysisTime = time.Duration(tm.GetAnalysisPhaseTimeInMs()) * time.Millisecond
		s.ExecutionTime = time.Duration(tm.GetExecutionPhaseTimeInMs()) * time.Millisecond
		if cp := tm.GetCriticalPathTime(); cp != nil {
			s.CriticalPath = cp.AsDuration()
		}
		mm := m.GetMemoryMetrics()
		s.PeakPostGCHeapBytes, s.UsedHeapPostBuild = mm.GetPeakPostGcHeapSize(), mm.GetUsedHeapSizePostBuild()
		ns := m.GetNetworkMetrics().GetSystemNetworkStats()
		s.NetworkBytesSent, s.NetworkBytesReceived = ns.GetBytesSent(), ns.GetBytesRecv()
	case *bespb.BuildEvent_BuildToolLogs:
		for _, f := range p.BuildToolLogs.GetLog() {
			c := string(f.GetContents())
			switch f.GetName() {
			case "critical path":
				s.CriticalPathLog, _, _ = strings.Cut(c, "\n")
			case "process stats":
				s.ProcessStats = strings.TrimSpace(c)
			}
		}
	case *bespb.BuildEvent_TestResult:
		t := a.test(ev.GetId().GetTestResult().GetLabel())
		r := p.TestResult
		t.Strategy = r.GetExecutionInfo().GetStrategy()
		t.Remote = r.GetExecutionInfo().GetCachedRemotely()
		if d := r.GetTestAttemptDuration(); d != nil {
			t.Duration += d.AsDuration()
		}
		if n := int(ev.GetId().GetTestResult().GetAttempt()); n > t.Attempts {
			t.Attempts = n
		}
	case *bespb.BuildEvent_TestSummary:
		t := a.test(ev.GetId().GetTestSummary().GetLabel())
		ts := p.TestSummary
		t.Status = ts.GetOverallStatus().String()
		t.Runs = int(ts.GetRunCount())
		t.Cached = int(ts.GetTotalNumCached())
		if n := int(ts.GetAttemptCount()); n > t.Attempts {
			t.Attempts = n
		}
	}
}

func (a *accumulator) summary() *Summary {
	s := a.s
	if s.WallTime == 0 && !s.Started.IsZero() && !s.Finished.IsZero() {
		s.WallTime = s.Finished.Sub(s.Started)
	}
	sort.Strings(a.testKeys)
	for _, k := range a.testKeys {
		s.Tests = append(s.Tests, *a.tests[k])
	}
	return &s
}
