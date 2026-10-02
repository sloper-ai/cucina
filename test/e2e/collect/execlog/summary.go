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

// Routing is the NFR-X1 evidence for one configuration: how many compile/link
// and test spawns ran on the runner whose REAPI platform properties equal the
// expected set exactly. Buildbarn routes an action to the queue whose property
// set equals the action's (the whole set), so a spawn's platform properties
// are the exec properties of the queue and worker that executed it; a cache
// hit's properties are part of its action digest, so it was executed there
// earlier. Spawns that ran on the client are misrouted ("local:<runner>").
type Routing struct {
	CompileLinkTotal  int `json:"compileLinkTotal"`
	CompileLinkOnPool int `json:"compileLinkOnPool"`
	// CompileLinkCached and TestCached count the cache hits among the totals.
	CompileLinkCached  int            `json:"compileLinkCached"`
	TestTotal          int            `json:"testTotal"`
	TestOnRunner       int            `json:"testOnRunner"`
	TestCached         int            `json:"testCached"`
	Misrouted          map[string]int `json:"misrouted,omitempty"` // class + property key (or local:<runner>) -> count
	CompileLinkPercent float64        `json:"compileLinkPercent"`
	TestPercent        float64        `json:"testPercent"`
}

// RouteCheck evaluates where the compile/link and test spawns ran against the
// expected compile-pool and test-runner property sets.
func (l *Log) RouteCheck(compile, test map[string]string) Routing {
	r := Routing{}
	for _, sp := range l.Spawns {
		class := Classify(sp.Mnemonic)
		if class == ClassOther {
			continue
		}
		want := compile
		if class == ClassTest {
			want = test
		}
		where, ok := PropsKey(sp.Platform), false
		switch {
		case sp.CacheHit, sp.Runner == "remote":
			ok = equalProps(sp.Platform, want)
		default:
			where = "local:" + sp.Runner
		}
		if class == ClassTest {
			r.TestTotal++
			r.TestOnRunner += b2i(ok)
			r.TestCached += b2i(sp.CacheHit)
		} else {
			r.CompileLinkTotal++
			r.CompileLinkOnPool += b2i(ok)
			r.CompileLinkCached += b2i(sp.CacheHit)
		}
		if !ok {
			if r.Misrouted == nil {
				r.Misrouted = map[string]int{}
			}
			r.Misrouted[string(class)+" on "+where]++
		}
	}
	r.percentages()
	return r
}

// Merge adds another invocation's counts (a configuration that builds and
// then tests in two invocations).
func (r *Routing) Merge(o Routing) {
	r.CompileLinkTotal += o.CompileLinkTotal
	r.CompileLinkOnPool += o.CompileLinkOnPool
	r.CompileLinkCached += o.CompileLinkCached
	r.TestTotal += o.TestTotal
	r.TestOnRunner += o.TestOnRunner
	r.TestCached += o.TestCached
	for k, v := range o.Misrouted {
		if r.Misrouted == nil {
			r.Misrouted = map[string]int{}
		}
		r.Misrouted[k] += v
	}
	r.percentages()
}

func (r *Routing) percentages() {
	r.CompileLinkPercent = pct(r.CompileLinkOnPool, r.CompileLinkTotal)
	r.TestPercent = pct(r.TestOnRunner, r.TestTotal)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
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
