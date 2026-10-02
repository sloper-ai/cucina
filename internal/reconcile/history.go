// SPDX-License-Identifier: FSL-1.1-ALv2

package reconcile

import (
	"sync"
	"time"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
)

// HistoryEvent is one scale event of a pool (management API, `cucinactl pools history`).
type HistoryEvent struct {
	Time    time.Time
	Pool    domain.PoolName
	Type    string // launch | register | drain-acknowledged | terminate | stop | fail | ice; legacy drain is intent only
	Subject string // node ID
	Message string
}

// StartRecord is the measured start latency of one VM (R-OBS-1, §10.4).
type StartRecord struct {
	Pool          domain.PoolName
	VM            string
	Launched      time.Time
	ToRunning     time.Duration
	ToRegistered  time.Duration
	ToFirstAction time.Duration
	Path          string // ec2 | tart
}

const (
	historyEvents = 512
	historyStarts = 256
)

// history keeps the most recent events and start records in memory (the
// leader's view; a new leader starts empty — Kubernetes events carry the
// durable timeline).
type history struct {
	mu     sync.Mutex
	events []HistoryEvent
	starts []StartRecord
}

func (h *history) event(e HistoryEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, e)
	if len(h.events) > historyEvents {
		h.events = h.events[len(h.events)-historyEvents:]
	}
}

func (h *history) start(s StartRecord) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.starts {
		if h.starts[i].VM == s.VM && h.starts[i].Launched.Equal(s.Launched) {
			h.starts[i] = s
			return
		}
	}
	h.starts = append(h.starts, s)
	if len(h.starts) > historyStarts {
		h.starts = h.starts[len(h.starts)-historyStarts:]
	}
}

// History returns a pool's ("" = every pool) most recent events and start
// records, newest first, at most limit of each.
func (f *Fleet) History(pool domain.PoolName, limit int) ([]HistoryEvent, []StartRecord) {
	f.hist.mu.Lock()
	defer f.hist.mu.Unlock()
	var evs []HistoryEvent
	for i := len(f.hist.events) - 1; i >= 0 && (limit <= 0 || len(evs) < limit); i-- {
		if pool == "" || f.hist.events[i].Pool == pool {
			evs = append(evs, f.hist.events[i])
		}
	}
	var sts []StartRecord
	for i := len(f.hist.starts) - 1; i >= 0 && (limit <= 0 || len(sts) < limit); i-- {
		if pool == "" || f.hist.starts[i].Pool == pool {
			sts = append(sts, f.hist.starts[i])
		}
	}
	return evs, sts
}

// Worker is one VM with its private address (EC2).
type Worker struct {
	domain.VM
	PrivateIP string
}

// Workers returns every VM the loops track (including stopped Tart VMs),
// with EC2 private IPs from the last instance observation.
func (f *Fleet) Workers() []Worker {
	ips := map[string]string{}
	f.shared.mu.Lock()
	for _, in := range f.shared.instances.v {
		if in.PrivateIP.IsValid() && in.State != ports.InstanceTerminated {
			ips[in.ID] = in.PrivateIP.String()
		}
	}
	f.shared.mu.Unlock()
	var out []Worker
	for _, l := range f.sortedLoops() {
		for _, vm := range l.snapshot().VMs {
			out = append(out, Worker{VM: vm, PrivateIP: ips[vm.ID]})
		}
	}
	return out
}
