// SPDX-License-Identifier: FSL-1.1-ALv2

package reconcile

import "slices"

// MaxVMsPerHost is Apple's limit of concurrently running macOS VMs per host
// (licence and Virtualization.framework, R-MAC-3).
const MaxVMsPerHost = 2

// PlacementHost is one Mac host as seen by Tart placement.
type PlacementHost struct {
	Serial   string
	Labels   map[string]string
	Online   bool
	Approved bool
	Cordoned bool
	// Slots is how many VMs the host may run (1–2); 0 means the default of 2.
	Slots int
	// Running counts the host's running (or starting) VMs of every pool.
	Running int
	// PoolVMs counts the running (or starting) VMs of the pool being placed.
	PoolVMs int
	// Stopped lists stopped VMs of this pool on the host, most preferred first;
	// starting one reuses its disk and therefore its warm L1 cache (R-CACHE-2).
	Stopped []string
}

// PlacementRequest asks for Count more VMs of one pool.
type PlacementRequest struct {
	// Selector must match the host's labels (TartSpec.HostSelector).
	Selector map[string]string
	// VMsPerHost caps the pool's VMs on one host (TartSpec.VMsPerHost, 1–2).
	VMsPerHost int
	Count      int
}

// Placement is where one VM starts: VM names a stopped VM to start again, or
// is empty to clone a new VM from the pool's image.
type Placement struct {
	Serial string
	VM     string
}

func hostSlots(h PlacementHost) int {
	if h.Slots <= 0 || h.Slots > MaxVMsPerHost {
		return MaxVMsPerHost
	}
	return h.Slots
}

func perHost(req PlacementRequest) int {
	if req.VMsPerHost <= 0 || req.VMsPerHost > MaxVMsPerHost {
		return MaxVMsPerHost
	}
	return req.VMsPerHost
}

// Eligible reports whether the pool may place VMs on h at all: online, approved,
// not cordoned, and matching the selector (R-POOL-6, R-MAC-6).
func Eligible(req PlacementRequest, h PlacementHost) bool {
	if !h.Online || !h.Approved || h.Cordoned {
		return false
	}
	for k, v := range req.Selector {
		if got, ok := h.Labels[k]; !ok || got != v {
			return false
		}
	}
	return true
}

func room(req PlacementRequest, h PlacementHost) int {
	if !Eligible(req, h) {
		return 0
	}
	return max(0, min(perHost(req)-h.PoolVMs, hostSlots(h)-h.Running))
}

// Capacity is the number of VMs of the pool the eligible hosts can run in total,
// counting the ones already running (R-POOL-6: the sum of eligible host slots;
// the pool's max caps it separately).
func Capacity(req PlacementRequest, hosts []PlacementHost) int {
	n := 0
	for _, h := range hosts {
		if Eligible(req, h) {
			n += max(0, min(perHost(req), hostSlots(h)-(h.Running-h.PoolVMs)))
		}
	}
	return n
}

// Place chooses hosts for up to req.Count new VMs of one pool. It is a pure
// function (property-tested) with these rules:
//   - only eligible hosts; a host never exceeds its slots (at most 2 running
//     VMs, R-MAC-3) nor VMsPerHost VMs of the pool;
//   - spread: a host gets another VM of the pool only when every eligible host
//     with room runs at least as many of the pool's VMs (one per host before a
//     second, R-POOL-6);
//   - among equally loaded hosts prefer one holding a stopped VM of the pool
//     (warm L1), then fewer running VMs, then the serial number (deterministic).
//
// It returns fewer placements than requested when the hosts are full.
func Place(req PlacementRequest, hosts []PlacementHost) []Placement {
	hs := make([]PlacementHost, len(hosts))
	for i, h := range hosts {
		h.Stopped = slices.Clone(h.Stopped) // in the caller's preference order
		hs[i] = h
	}
	var out []Placement
	for len(out) < req.Count {
		best := -1
		for i := range hs {
			if room(req, hs[i]) == 0 {
				continue
			}
			if best < 0 || better(hs[i], hs[best]) {
				best = i
			}
		}
		if best < 0 {
			break
		}
		h := &hs[best]
		p := Placement{Serial: h.Serial}
		if len(h.Stopped) > 0 {
			p.VM, h.Stopped = h.Stopped[0], h.Stopped[1:]
		}
		h.PoolVMs++
		h.Running++
		out = append(out, p)
	}
	return out
}

func better(a, b PlacementHost) bool {
	if a.PoolVMs != b.PoolVMs {
		return a.PoolVMs < b.PoolVMs
	}
	if (len(a.Stopped) > 0) != (len(b.Stopped) > 0) {
		return len(a.Stopped) > 0
	}
	if a.Running != b.Running {
		return a.Running < b.Running
	}
	return a.Serial < b.Serial
}
