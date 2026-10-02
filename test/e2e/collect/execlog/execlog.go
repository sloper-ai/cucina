// SPDX-License-Identifier: FSL-1.1-ALv2

// Package execlog parses Bazel's compact execution log
// (--execution_log_compact_file: a zstd-compressed stream of length-delimited
// tools.protos.ExecLogEntry messages, Bazel 9.2 spawn.proto) into one record
// per spawn: runner, cache hit, platform (REAPI exec properties), action
// digest, per-phase timings, input and output bytes (§10.4 "write a small
// parser"). Summaries feed NFR-P4 (queue time, worker overhead), NFR-X1
// (routing) and NFR-T1 (total action output bytes).
package execlog

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"google.golang.org/protobuf/encoding/protodelim"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/sloper-ai/cucina/test/e2e/third_party/bazel/spawnpb"
)

// Digest is a REAPI digest (hash function from the log's Invocation entry).
type Digest struct {
	Hash      string `json:"hash"`
	SizeBytes int64  `json:"sizeBytes"`
}

// IsZero reports whether the digest is unset.
func (d Digest) IsZero() bool { return d.Hash == "" }

// String renders "hash/size", the form `cucinactl action inspect` accepts.
func (d Digest) String() string { return fmt.Sprintf("%s/%d", d.Hash, d.SizeBytes) }

// Timings are the per-phase durations Bazel records for a spawn
// (SpawnMetrics). For remote executions queue/setup/execution/processOutputs
// come from the worker's ExecutedActionMetadata.
type Timings struct {
	Total          time.Duration `json:"total"`
	Parse          time.Duration `json:"parse"`
	Network        time.Duration `json:"network"`
	Fetch          time.Duration `json:"fetch"`
	Queue          time.Duration `json:"queue"`
	Setup          time.Duration `json:"setup"`
	Upload         time.Duration `json:"upload"`
	Execution      time.Duration `json:"execution"`
	ProcessOutputs time.Duration `json:"processOutputs"`
	Retry          time.Duration `json:"retry"`
}

// Spawn is one executed (or cache-hit) spawn.
type Spawn struct {
	Mnemonic    string            `json:"mnemonic"`
	TargetLabel string            `json:"targetLabel"`
	Runner      string            `json:"runner"`
	CacheHit    bool              `json:"cacheHit"`
	Remotable   bool              `json:"remotable"`
	Cacheable   bool              `json:"cacheable"`
	ExitCode    int32             `json:"exitCode"`
	Status      string            `json:"status,omitempty"`
	Platform    map[string]string `json:"platform,omitempty"`
	// ActionDigest is the action cache key (only set with a remote/disk cache).
	ActionDigest Digest `json:"actionDigest"`
	// InputBytes/InputFiles come from SpawnMetrics when Bazel filled them, else
	// they are computed from the input set (unique files, transitively).
	InputBytes  int64     `json:"inputBytes"`
	InputFiles  int64     `json:"inputFiles"`
	OutputBytes int64     `json:"outputBytes"`
	OutputFiles int64     `json:"outputFiles"`
	Start       time.Time `json:"start"`
	Timings     Timings   `json:"timings"`
}

// Remote reports whether the spawn was executed by a remote worker (not a
// cache hit). Bazel names that runner "remote".
func (s Spawn) Remote() bool { return s.Runner == "remote" && !s.CacheHit }

// RemoteCacheHit reports a hit in the remote action cache.
func (s Spawn) RemoteCacheHit() bool { return s.CacheHit && s.Runner == "remote cache hit" }

// WorkerOverhead is the per-action worker overhead of NFR-P4: input root
// construction plus output upload on the worker.
func (s Spawn) WorkerOverhead() time.Duration { return s.Timings.Setup + s.Timings.ProcessOutputs }

// InputIdentity retains used file identities for X5's RPC/digest join, without
// expanding the same toolchain inventory into every spawn. Symlink references
// have no content digest and must not be counted as uploaded SDK bytes.
type InputIdentity struct {
	Path          string `json:"path"`
	Digest        Digest `json:"digest"`
	Tool          bool   `json:"tool"`
	SymlinkTarget string `json:"symlinkTarget,omitempty"`
}

// Log is a parsed compact execution log.
type Log struct {
	InvocationID string          `json:"invocationId"`
	HashFunction string          `json:"hashFunction"`
	Spawns       []Spawn         `json:"spawns"`
	Inputs       []InputIdentity `json:"inputs,omitempty"`
}

// ReadFile parses a compact execution log file.
func ReadFile(path string) (*Log, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return Read(f)
}

// Read parses a compact execution log stream (zstd-compressed).
func Read(r io.Reader) (*Log, error) {
	zr, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, fmt.Errorf("execlog: zstd: %w", err)
	}
	defer zr.Close()
	return readEntries(bufio.NewReaderSize(zr, 1<<16))
}

type fileInfo struct {
	size       int64
	files      int64
	identities []InputIdentity
}

type state struct {
	files    map[uint32]fileInfo // File, Directory (aggregated), UnresolvedSymlink entries
	sets     map[uint32]*spawnpb.ExecLogEntry_InputSet
	runfiles map[uint32]uint32 // runfiles tree id -> input set id
	memo     map[uint32]map[uint32]struct{}
	used     map[string]InputIdentity
}

func readEntries(br *bufio.Reader) (*Log, error) {
	st := &state{
		files:    map[uint32]fileInfo{},
		sets:     map[uint32]*spawnpb.ExecLogEntry_InputSet{},
		runfiles: map[uint32]uint32{},
		memo:     map[uint32]map[uint32]struct{}{},
		used:     map[string]InputIdentity{},
	}
	log := &Log{}
	opts := protodelim.UnmarshalOptions{MaxSize: 64 << 20}
	for {
		e := &spawnpb.ExecLogEntry{}
		if err := opts.UnmarshalFrom(br, e); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("execlog: entry %d: %w", len(log.Spawns), err)
		}
		id := e.GetId()
		switch t := e.GetType().(type) {
		case *spawnpb.ExecLogEntry_Invocation_:
			log.InvocationID = t.Invocation.GetId()
			log.HashFunction = t.Invocation.GetHashFunctionName()
		case *spawnpb.ExecLogEntry_File_:
			st.files[id] = fileInfo{size: t.File.GetDigest().GetSizeBytes(), files: 1, identities: []InputIdentity{{Path: strings.ReplaceAll(t.File.GetPath(), `\`, "/"), Digest: Digest{Hash: t.File.GetDigest().GetHash(), SizeBytes: t.File.GetDigest().GetSizeBytes()}}}}
		case *spawnpb.ExecLogEntry_Directory_:
			var fi fileInfo
			for _, f := range t.Directory.GetFiles() {
				fi.size += f.GetDigest().GetSizeBytes()
				fi.files++
				fi.identities = append(fi.identities, InputIdentity{Path: path.Join(strings.ReplaceAll(t.Directory.GetPath(), `\`, "/"), strings.ReplaceAll(f.GetPath(), `\`, "/")), Digest: Digest{Hash: f.GetDigest().GetHash(), SizeBytes: f.GetDigest().GetSizeBytes()}})
			}
			st.files[id] = fi
		case *spawnpb.ExecLogEntry_UnresolvedSymlink_:
			st.files[id] = fileInfo{files: 1, identities: []InputIdentity{{Path: strings.ReplaceAll(t.UnresolvedSymlink.GetPath(), `\`, "/"), SymlinkTarget: t.UnresolvedSymlink.GetTargetPath()}}}
		case *spawnpb.ExecLogEntry_InputSet_:
			st.sets[id] = t.InputSet
		case *spawnpb.ExecLogEntry_RunfilesTree_:
			st.runfiles[id] = t.RunfilesTree.GetInputSetId()
		case *spawnpb.ExecLogEntry_Spawn_:
			log.Spawns = append(log.Spawns, st.spawn(t.Spawn))
		}
	}
	for _, f := range st.used {
		log.Inputs = append(log.Inputs, f)
	}
	sort.Slice(log.Inputs, func(i, j int) bool {
		return log.Inputs[i].Path+log.Inputs[i].Digest.String() < log.Inputs[j].Path+log.Inputs[j].Digest.String()
	})
	return log, nil
}

func (st *state) spawn(p *spawnpb.ExecLogEntry_Spawn) Spawn {
	s := Spawn{
		Mnemonic:    p.GetMnemonic(),
		TargetLabel: p.GetTargetLabel(),
		Runner:      p.GetRunner(),
		CacheHit:    p.GetCacheHit(),
		Remotable:   p.GetRemotable(),
		Cacheable:   p.GetCacheable(),
		ExitCode:    p.GetExitCode(),
		Status:      p.GetStatus(),
	}
	if props := p.GetPlatform().GetProperties(); len(props) > 0 {
		s.Platform = make(map[string]string, len(props))
		for _, kv := range props {
			s.Platform[kv.GetName()] = kv.GetValue()
		}
	}
	if d := p.GetDigest(); d != nil {
		s.ActionDigest = Digest{Hash: d.GetHash(), SizeBytes: d.GetSizeBytes()}
	}
	m := p.GetMetrics()
	s.Timings = Timings{
		Total:          dur(m.GetTotalTime()),
		Parse:          dur(m.GetParseTime()),
		Network:        dur(m.GetNetworkTime()),
		Fetch:          dur(m.GetFetchTime()),
		Queue:          dur(m.GetQueueTime()),
		Setup:          dur(m.GetSetupTime()),
		Upload:         dur(m.GetUploadTime()),
		Execution:      dur(m.GetExecutionWallTime()),
		ProcessOutputs: dur(m.GetProcessOutputsTime()),
		Retry:          dur(m.GetRetryTime()),
	}
	if ts := m.GetStartTime(); ts != nil {
		s.Start = ts.AsTime()
	}
	s.InputBytes, s.InputFiles = m.GetInputBytes(), m.GetInputFiles()
	ids, tools := map[uint32]struct{}{}, map[uint32]struct{}{}
	st.collect(p.GetInputSetId(), ids)
	st.collect(p.GetToolSetId(), tools)
	for id := range tools {
		ids[id] = struct{}{}
	}
	for id := range ids {
		for _, f := range st.files[id].identities {
			_, f.Tool = tools[id]
			key := f.Path + "\x00" + f.Digest.String()
			if previous, ok := st.used[key]; ok {
				f.Tool = f.Tool || previous.Tool
			}
			st.used[key] = f
		}
	}
	if s.InputBytes == 0 && s.InputFiles == 0 {
		for id := range ids {
			fi := st.files[id]
			s.InputBytes += fi.size
			s.InputFiles += fi.files
		}
	}
	for _, o := range p.GetOutputs() {
		if fi, ok := st.files[o.GetOutputId()]; ok && o.GetOutputId() != 0 {
			s.OutputBytes += fi.size
			s.OutputFiles += fi.files
		}
	}
	return s
}

// collect adds the file-like entry ids reachable from input set `set` to ids.
// A spawn's own top-level set is walked without memoization (it is usually
// unique); the shared transitive sets below it (toolchains, headers) are
// memoized, which keeps Abseil-sized logs cheap.
func (st *state) collect(set uint32, ids map[uint32]struct{}) {
	is, ok := st.sets[set]
	if set == 0 || !ok {
		return
	}
	st.addDirect(is, ids, map[uint32]bool{set: true})
}

func (st *state) addDirect(is *spawnpb.ExecLogEntry_InputSet, ids map[uint32]struct{}, visiting map[uint32]bool) {
	for _, in := range is.GetInputIds() {
		if rs, ok := st.runfiles[in]; ok {
			for id := range st.closure(rs, visiting) {
				ids[id] = struct{}{}
			}
			continue
		}
		ids[in] = struct{}{}
	}
	for _, t := range is.GetTransitiveSetIds() {
		for id := range st.closure(t, visiting) {
			ids[id] = struct{}{}
		}
	}
}

// closure returns the memoized transitive file ids of a shared input set. The
// returned map must not be modified.
func (st *state) closure(set uint32, visiting map[uint32]bool) map[uint32]struct{} {
	if m, ok := st.memo[set]; ok {
		return m
	}
	is, ok := st.sets[set]
	if !ok || visiting[set] {
		return nil
	}
	visiting[set] = true
	out := map[uint32]struct{}{}
	st.addDirect(is, out, visiting)
	delete(visiting, set)
	st.memo[set] = out
	return out
}

func dur(d *durationpb.Duration) time.Duration {
	if d == nil {
		return 0
	}
	return d.AsDuration()
}

// RunnerCounts counts spawns by runner name (cache hits included under their
// runner name, e.g. "remote cache hit", "disk cache hit").
func (l *Log) RunnerCounts() map[string]int {
	out := map[string]int{}
	for _, s := range l.Spawns {
		out[s.Runner]++
	}
	return out
}

// FirstRemoteStart is the start time of the earliest remotely executed spawn
// (zero if none). With the build start from the BEP it gives "time to first
// remote action" (§10.4).
func (l *Log) FirstRemoteStart() time.Time {
	var first time.Time
	for _, s := range l.Spawns {
		if s.Remote() && !s.Start.IsZero() && (first.IsZero() || s.Start.Before(first)) {
			first = s.Start
		}
	}
	return first
}

// Percentile returns the p-th percentile (0 < p ≤ 100, nearest-rank) of ds; 0
// for an empty slice.
func Percentile(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	rank := int(p/100*float64(len(s)) + 0.999999999)
	if rank < 1 {
		rank = 1
	}
	if rank > len(s) {
		rank = len(s)
	}
	return s[rank-1]
}
