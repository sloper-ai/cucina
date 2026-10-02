// SPDX-License-Identifier: FSL-1.1-ALv2

package reconcile_test

import (
	"fmt"
	"testing"

	"pgregory.net/rapid"

	"github.com/sloper-ai/cucina/internal/reconcile"
)

func genHosts(t *rapid.T) []reconcile.PlacementHost {
	n := rapid.IntRange(0, 6).Draw(t, "hosts")
	hosts := make([]reconcile.PlacementHost, n)
	for i := range hosts {
		slots := rapid.IntRange(0, 2).Draw(t, "slots")
		effective := slots
		if effective == 0 {
			effective = 2
		}
		running := rapid.IntRange(0, effective).Draw(t, "running")
		h := reconcile.PlacementHost{
			Serial:   fmt.Sprintf("S%02d", i),
			Labels:   map[string]string{"site": rapid.SampledFrom([]string{"a", "b"}).Draw(t, "site")},
			Online:   rapid.Float64Range(0, 1).Draw(t, "online") < 0.8,
			Approved: rapid.Float64Range(0, 1).Draw(t, "approved") < 0.9,
			Cordoned: rapid.Float64Range(0, 1).Draw(t, "cordoned") < 0.15,
			Slots:    slots,
			Running:  running,
			PoolVMs:  rapid.IntRange(0, running).Draw(t, "poolVMs"),
		}
		for j := range rapid.IntRange(0, 2).Draw(t, "stopped") {
			h.Stopped = append(h.Stopped, fmt.Sprintf("%s-vm%d", h.Serial, j))
		}
		hosts[i] = h
	}
	return hosts
}

// Guards R-POOL-6 and R-MAC-3 (and the R-TEST-7 invariant "at most 2 macOS VMs
// per host"): placement honours eligibility (online, approved, uncordoned,
// labels), host slots and the pool's per-host cap, spreads one VM per host
// before a second, fills all available room up to the request, and reuses
// stopped VMs (warm L1) without starting one twice.
func TestPlacementProperties(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		hosts := genHosts(t)
		req := reconcile.PlacementRequest{
			VMsPerHost: rapid.IntRange(1, 2).Draw(t, "vmsPerHost"),
			Count:      rapid.IntRange(0, 8).Draw(t, "count"),
		}
		if rapid.Bool().Draw(t, "selector") {
			req.Selector = map[string]string{"site": "a"}
		}
		got := reconcile.Place(req, hosts)

		byHost := map[string]int{}
		usedVM := map[string]bool{}
		for _, p := range got {
			byHost[p.Serial]++
			if p.VM != "" {
				if usedVM[p.VM] {
					t.Fatalf("stopped VM %s started twice", p.VM)
				}
				usedVM[p.VM] = true
			}
		}
		free := 0
		for _, h := range hosts {
			slots := h.Slots
			if slots == 0 {
				slots = 2
			}
			added := byHost[h.Serial]
			if added > 0 && !reconcile.Eligible(req, h) {
				t.Fatalf("placed on ineligible host %+v", h)
			}
			if h.Running+added > slots || h.Running+added > reconcile.MaxVMsPerHost {
				t.Fatalf("host %s exceeds its slots: running %d + %d > %d", h.Serial, h.Running, added, slots)
			}
			if added > 0 && h.PoolVMs+added > req.VMsPerHost {
				t.Fatalf("host %s exceeds vmsPerHost", h.Serial)
			}
			stoppedUsed := 0
			for _, vm := range h.Stopped {
				if usedVM[vm] {
					stoppedUsed++
				}
			}
			if stoppedUsed != min(added, len(h.Stopped)) {
				t.Fatalf("host %s: %d placements but %d of %d stopped VMs reused", h.Serial, added, stoppedUsed, len(h.Stopped))
			}
			if reconcile.Eligible(req, h) {
				free += max(0, min(req.VMsPerHost-h.PoolVMs, slots-h.Running))
			}
		}
		if len(got) != min(req.Count, free) {
			t.Fatalf("placed %d VMs, want min(count %d, free %d)", len(got), req.Count, free)
		}
		// Spread: a host that received VMs ends at most one above any eligible host that still has room.
		for _, h := range hosts {
			if byHost[h.Serial] == 0 {
				continue
			}
			for _, g := range hosts {
				slots := g.Slots
				if slots == 0 {
					slots = 2
				}
				gAfter := g.PoolVMs + byHost[g.Serial]
				hasRoom := reconcile.Eligible(req, g) && gAfter < req.VMsPerHost && g.Running+byHost[g.Serial] < slots
				if hasRoom && gAfter < h.PoolVMs+byHost[h.Serial]-1 {
					t.Fatalf("not spread: %s got a VM at level %d while %s (with room) stays at %d", h.Serial, h.PoolVMs+byHost[h.Serial], g.Serial, gAfter)
				}
			}
		}
		if c := reconcile.Capacity(req, hosts); c < len(got) {
			t.Fatalf("capacity %d below placements %d", c, len(got))
		}
	})
}
