// SPDX-License-Identifier: FSL-1.1-ALv2

package execlog

import (
	"sort"
	"strings"
	"time"
)

// Distribution summarises a set of durations (nearest-rank percentiles).
type Distribution struct {
	N   int           `json:"n"`
	P50 time.Duration `json:"p50"`
	P95 time.Duration `json:"p95"`
	Max time.Duration `json:"max"`
}

// Distribute computes a Distribution.
func Distribute(ds []time.Duration) Distribution {
	d := Distribution{N: len(ds), P50: Percentile(ds, 50), P95: Percentile(ds, 95)}
	for _, x := range ds {
		if x > d.Max {
			d.Max = x
		}
	}
	return d
}

// Summary is what scenarios record per invocation.
type Summary struct {
	Spawns           int            `json:"spawns"`
	ByRunner         map[string]int `json:"byRunner"`
	RemoteExecutions int            `json:"remoteExecutions"`
	RemoteCacheHits  int            `json:"remoteCacheHits"`
	OtherCacheHits   int            `json:"otherCacheHits"`
	LocalExecutions  int            `json:"localExecutions"`
	// Timings of remote executions (NFR-P4): queue, worker input-root setup,
	// execution, worker output upload, and their sum setup+processOutputs.
	Queue          Distribution `json:"queue"`
	Setup          Distribution `json:"setup"`
	Execution      Distribution `json:"execution"`
	ProcessOutputs Distribution `json:"processOutputs"`
	WorkerOverhead Distribution `json:"workerOverhead"`
	// Bytes: inputs of remote executions, outputs of every executed spawn
	// (NFR-T1's "total action output bytes" on a cold build).
	RemoteInputBytes  int64                     `json:"remoteInputBytes"`
	OutputBytes       int64                     `json:"outputBytes"`
	FirstRemoteStart  time.Time                 `json:"firstRemoteStart"`
	FailedSpawns      int                       `json:"failedSpawns"`
	MnemonicsByRunner map[string]map[string]int `json:"mnemonicsByRunner"`
}

// Summarize computes a Summary.
func (l *Log) Summarize() Summary {
	s := Summary{ByRunner: l.RunnerCounts(), MnemonicsByRunner: map[string]map[string]int{}}
	var queue, setup, exec, outs, overhead []time.Duration
	for _, sp := range l.Spawns {
		s.Spawns++
		m := s.MnemonicsByRunner[sp.Runner]
		if m == nil {
			m = map[string]int{}
			s.MnemonicsByRunner[sp.Runner] = m
		}
		m[sp.Mnemonic]++
		if sp.ExitCode != 0 || sp.Status != "" {
			s.FailedSpawns++
		}
		switch {
		case sp.RemoteCacheHit():
			s.RemoteCacheHits++
		case sp.CacheHit:
			s.OtherCacheHits++
		case sp.Remote():
			s.RemoteExecutions++
			s.RemoteInputBytes += sp.InputBytes
			s.OutputBytes += sp.OutputBytes
			queue = append(queue, sp.Timings.Queue)
			setup = append(setup, sp.Timings.Setup)
			exec = append(exec, sp.Timings.Execution)
			outs = append(outs, sp.Timings.ProcessOutputs)
			overhead = append(overhead, sp.WorkerOverhead())
		default:
			s.LocalExecutions++
			s.OutputBytes += sp.OutputBytes
		}
	}
	s.Queue, s.Setup, s.Execution = Distribute(queue), Distribute(setup), Distribute(exec)
	s.ProcessOutputs, s.WorkerOverhead = Distribute(outs), Distribute(overhead)
	s.FirstRemoteStart = l.FirstRemoteStart()
	return s
}

// RemoteCacheHitRatio is remote cache hits over all spawns (NFR-P3, T2).
func (s Summary) RemoteCacheHitRatio() float64 {
	if s.Spawns == 0 {
		return 0
	}
	return float64(s.RemoteCacheHits) / float64(s.Spawns)
}

// RemoteRatio is the share of spawns executed remotely or served by the
// remote cache (T22).
func (s Summary) RemoteRatio() float64 {
	if s.Spawns == 0 {
		return 0
	}
	return float64(s.RemoteExecutions+s.RemoteCacheHits) / float64(s.Spawns)
}

// ActionClass groups mnemonics for routing checks (NFR-X1).
type ActionClass string

const (
	ClassCompileLink ActionClass = "compile/link"
	ClassTest        ActionClass = "test"
	ClassOther       ActionClass = "other"
)

// Classify maps a mnemonic to its routing class. Compile/link covers the C++,
// Objective-C, Go and Rust compile, link and archive actions the cross matrix
// produces; TestRunner is Bazel's test action.
func Classify(mnemonic string) ActionClass {
	switch {
	case mnemonic == "TestRunner":
		return ClassTest
	case strings.HasPrefix(mnemonic, "CppCompile"), strings.HasPrefix(mnemonic, "CppLink"),
		mnemonic == "CppArchive", strings.HasPrefix(mnemonic, "ObjcCompile"), strings.HasPrefix(mnemonic, "ObjcLink"),
		strings.HasPrefix(mnemonic, "GoCompile"), mnemonic == "GoLink", mnemonic == "Rustc":
		return ClassCompileLink
	}
	return ClassOther
}

// Routing is the NFR-X1 evidence for one configuration: how many remotely
// executed actions of each class ran on a runner whose exec properties match
// the expected set exactly (Buildbarn matches the whole property set).
type Routing struct {
	CompileLinkTotal   int            `json:"compileLinkTotal"`
	CompileLinkOnPool  int            `json:"compileLinkOnPool"`
	TestTotal          int            `json:"testTotal"`
	TestOnRunner       int            `json:"testOnRunner"`
	Misrouted          map[string]int `json:"misrouted,omitempty"` // platform key -> count
	CompileLinkPercent float64        `json:"compileLinkPercent"`
	TestPercent        float64        `json:"testPercent"`
}

// RouteCheck evaluates routing of remote executions against the expected
// compile-pool and test-runner property sets.
func (l *Log) RouteCheck(compile, test map[string]string) Routing {
	r := Routing{Misrouted: map[string]int{}}
	for _, sp := range l.Spawns {
		if !sp.Remote() {
			continue
		}
		switch Classify(sp.Mnemonic) {
		case ClassCompileLink:
			r.CompileLinkTotal++
			if equalProps(sp.Platform, compile) {
				r.CompileLinkOnPool++
			} else {
				r.Misrouted[PropsKey(sp.Platform)]++
			}
		case ClassTest:
			r.TestTotal++
			if equalProps(sp.Platform, test) {
				r.TestOnRunner++
			} else {
				r.Misrouted[PropsKey(sp.Platform)]++
			}
		}
	}
	r.CompileLinkPercent = pct(r.CompileLinkOnPool, r.CompileLinkTotal)
	r.TestPercent = pct(r.TestOnRunner, r.TestTotal)
	if len(r.Misrouted) == 0 {
		r.Misrouted = nil
	}
	return r
}

func pct(n, d int) float64 {
	if d == 0 {
		return 100
	}
	return 100 * float64(n) / float64(d)
}

func equalProps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// PropsKey renders a property set canonically ("k=v;k=v", sorted), the same
// form as internal/domain.PropertiesKey.
func PropsKey(p map[string]string) string {
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(';')
		}
		b.WriteString(k + "=" + p[k])
	}
	return b.String()
}
