// SPDX-License-Identifier: FSL-1.1-ALv2

// Package profile summarises a Bazel --profile trace (Chrome trace JSON,
// optionally gzipped): the critical path, the slowest actions, build phases,
// Bazel's own memory and the host network counters (§10.4; `bazel
// analyze-profile` was removed in Bazel 9). The Go summariser always runs; if
// Perfetto's trace_processor is installed, TraceProcessor runs the same
// queries in SQL for cross-checking (R-LIB-4 "Profile analysis").
package profile

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// Component is one critical-path component.
type Component struct {
	Name     string        `json:"name"`
	Duration time.Duration `json:"duration"`
}

// Action is one "action processing" slice.
type Action struct {
	Name     string        `json:"name"`
	Mnemonic string        `json:"mnemonic"`
	Duration time.Duration `json:"duration"`
}

// MnemonicStat aggregates action slices per mnemonic.
type MnemonicStat struct {
	Count int           `json:"count"`
	Total time.Duration `json:"total"`
	Max   time.Duration `json:"max"`
}

// Summary is the analysis result.
type Summary struct {
	BazelVersion      string                  `json:"bazelVersion"`
	CriticalPath      []Component             `json:"criticalPath"`
	CriticalPathTotal time.Duration           `json:"criticalPathTotal"`
	TopActions        []Action                `json:"topActions"`
	Mnemonics         map[string]MnemonicStat `json:"mnemonics"`
	Phases            []Component             `json:"phases"`
	// PeakBazelMemoryMB is the maximum of the "Memory usage (Bazel)" counter.
	PeakBazelMemoryMB float64 `json:"peakBazelMemoryMB"`
	// NetworkUp/DownBytes integrate the host network counters (Mbps samples).
	NetworkUpBytes   float64        `json:"networkUpBytes"`
	NetworkDownBytes float64        `json:"networkDownBytes"`
	Categories       map[string]int `json:"categories"`
	Events           int            `json:"events"`
}

type event struct {
	Cat  string          `json:"cat"`
	Name string          `json:"name"`
	Ph   string          `json:"ph"`
	TS   float64         `json:"ts"`  // microseconds
	Dur  float64         `json:"dur"` // microseconds
	Args json.RawMessage `json:"args"`
}

// ReadFile analyses a profile file (.gz or plain JSON).
func ReadFile(path string, topN int) (*Summary, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	br := bufio.NewReaderSize(f, 1<<20)
	var r io.Reader = br
	if head, err := br.Peek(2); err == nil && head[0] == 0x1f && head[1] == 0x8b {
		zr, err := gzip.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("profile: %w", err)
		}
		defer func() { _ = zr.Close() }()
		r = zr
	}
	return Analyze(r, topN)
}

// Analyze streams a trace and summarises it, keeping the topN slowest actions.
func Analyze(r io.Reader, topN int) (*Summary, error) {
	dec := json.NewDecoder(r)
	s := &Summary{Mnemonics: map[string]MnemonicStat{}, Categories: map[string]int{}}
	if err := expectDelim(dec, '{'); err != nil {
		return nil, err
	}
	var markers []event
	var up, down []event
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("profile: %w", err)
		}
		switch tok {
		case "otherData":
			var od struct {
				BazelVersion string `json:"bazel_version"`
			}
			if err := dec.Decode(&od); err != nil {
				return nil, fmt.Errorf("profile: otherData: %w", err)
			}
			s.BazelVersion = strings.TrimPrefix(od.BazelVersion, "release ")
		case "traceEvents":
			if err := expectDelim(dec, '['); err != nil {
				return nil, err
			}
			for dec.More() {
				var e event
				if err := dec.Decode(&e); err != nil {
					return nil, fmt.Errorf("profile: event %d: %w", s.Events, err)
				}
				s.Events++
				s.add(e, &markers, &up, &down)
			}
			if _, err := dec.Token(); err != nil { // ']'
				return nil, fmt.Errorf("profile: %w", err)
			}
		default:
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, fmt.Errorf("profile: %w", err)
			}
		}
	}
	s.finish(markers, up, down, topN)
	return s, nil
}

func expectDelim(dec *json.Decoder, d json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("profile: %w", err)
	}
	if tok != d {
		return fmt.Errorf("profile: expected %q, got %v", d, tok)
	}
	return nil
}

func us(v float64) time.Duration { return time.Duration(v * float64(time.Microsecond)) }

func (s *Summary) add(e event, markers, up, down *[]event) {
	if e.Ph == "M" {
		return
	}
	if e.Cat != "" {
		s.Categories[e.Cat]++
	}
	switch {
	case e.Cat == "critical path component":
		s.CriticalPath = append(s.CriticalPath, Component{Name: e.Name, Duration: us(e.Dur)})
		s.CriticalPathTotal += us(e.Dur)
	case e.Cat == "action processing":
		var a struct {
			Mnemonic string `json:"mnemonic"`
		}
		_ = json.Unmarshal(e.Args, &a)
		d := us(e.Dur)
		s.TopActions = append(s.TopActions, Action{Name: e.Name, Mnemonic: a.Mnemonic, Duration: d})
		m := s.Mnemonics[a.Mnemonic]
		m.Count++
		m.Total += d
		if d > m.Max {
			m.Max = d
		}
		s.Mnemonics[a.Mnemonic] = m
	case e.Cat == "build phase marker" && e.Ph == "i":
		*markers = append(*markers, e)
	case e.Ph == "C" && e.Name == "Memory usage (Bazel)":
		var a struct {
			Memory float64 `json:"memory"`
		}
		if json.Unmarshal(e.Args, &a) == nil && a.Memory > s.PeakBazelMemoryMB {
			s.PeakBazelMemoryMB = a.Memory
		}
	case e.Ph == "C" && e.Name == "Network Up usage (total)":
		*up = append(*up, e)
	case e.Ph == "C" && e.Name == "Network Down usage (total)":
		*down = append(*down, e)
	}
}

func (s *Summary) finish(markers, up, down []event, topN int) {
	sort.SliceStable(s.TopActions, func(i, j int) bool { return s.TopActions[i].Duration > s.TopActions[j].Duration })
	if topN >= 0 && len(s.TopActions) > topN {
		s.TopActions = s.TopActions[:topN]
	}
	sort.SliceStable(markers, func(i, j int) bool { return markers[i].TS < markers[j].TS })
	for i := 0; i+1 < len(markers); i++ {
		s.Phases = append(s.Phases, Component{Name: markers[i].Name, Duration: us(markers[i+1].TS - markers[i].TS)})
	}
	s.NetworkUpBytes = integrateMbps(up)
	s.NetworkDownBytes = integrateMbps(down)
}

// integrateMbps turns Mbps samples (each covering the interval since the
// previous sample) into bytes.
func integrateMbps(samples []event) float64 {
	sort.SliceStable(samples, func(i, j int) bool { return samples[i].TS < samples[j].TS })
	var total float64
	for i := 1; i < len(samples); i++ {
		var a map[string]float64
		if json.Unmarshal(samples[i].Args, &a) != nil {
			continue
		}
		var mbps float64
		for _, v := range a { // one series per counter
			mbps += v
		}
		total += mbps * 1e6 / 8 * (samples[i].TS - samples[i-1].TS) / 1e6
	}
	return total
}
