// SPDX-License-Identifier: FSL-1.1-ALv2

package reconcile

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
)

// TartNode is the node ID (worker-id label "node") of a Tart VM: "<serial>/<vm>".
// The host part is the hardware serial number: unique and stable, unlike host names.
func TartNode(serial, vm string) string { return serial + "/" + vm }

// SplitTartNode splits a "<host>/<vm>" node ID and resolves the host part to a
// serial number (accepting a host name too).
func SplitTartNode(node string, hosts []ports.HostState) (serial, vm string, ok bool) {
	i := strings.LastIndexByte(node, '/')
	if i <= 0 || i == len(node)-1 {
		return "", "", false
	}
	host, vm := node[:i], node[i+1:]
	for _, h := range hosts {
		if h.Serial == host || (h.Name != "" && h.Name == host) {
			return h.Serial, vm, true
		}
	}
	return host, vm, len(hosts) == 0
}

func vmName(id string) string {
	if i := strings.LastIndexByte(id, '/'); i >= 0 {
		return id[i+1:]
	}
	return id
}

// alive reports whether a hostd-reported VM occupies a host slot.
func occupiesSlot(s domain.VMState) bool {
	switch s {
	case domain.VMStopped, domain.VMTerminated, domain.VMFailed, domain.VMUnavailable:
		return false
	}
	return true
}

func placementRequest(rt *PoolRuntime, count int) PlacementRequest {
	return PlacementRequest{Selector: rt.Tart.HostSelector, VMsPerHost: rt.Tart.VMsPerHost, Count: count}
}

// placementHosts converts host reports into placement input for pool: its
// running VMs per host and its stopped VMs (current generation first, so a warm
// L1 of the right image is preferred; an old-generation VM is re-cloned by
// hostd under the same name, which keeps host disk usage bounded).
func placementHosts(pool domain.PoolName, rt *PoolRuntime, hosts []ports.HostState) []PlacementHost {
	out := make([]PlacementHost, 0, len(hosts))
	for _, h := range hosts {
		ph := PlacementHost{
			Serial: h.Serial, Labels: h.Labels, Online: h.Online, Approved: h.Approved, Cordoned: h.Cordoned,
			Slots: h.Slots, Running: h.RunningVMs,
		}
		if rt.Tart != nil && rt.Tart.Hosts != nil {
			pol, ok := rt.Tart.Hosts[strings.ToUpper(h.Serial)]
			ph.Approved = ok && pol.Approved
			ph.Cordoned = ph.Cordoned || pol.Cordoned
			if len(pol.Labels) > 0 {
				labels := maps.Clone(h.Labels)
				if labels == nil {
					labels = map[string]string{}
				}
				maps.Copy(labels, pol.Labels)
				ph.Labels = labels
			}
			if pol.Slots > 0 {
				ph.Slots = pol.Slots
			}
		}
		var current, old []string
		for _, vm := range h.VMs {
			if vm.Pool != pool {
				continue
			}
			switch {
			case occupiesSlot(vm.State):
				ph.PoolVMs++
			case vm.State == domain.VMStopped && vm.Generation == rt.Spec.Generation:
				current = append(current, vmName(vm.ID))
			case vm.State == domain.VMStopped:
				old = append(old, vmName(vm.ID))
			}
		}
		slices.Sort(current)
		slices.Sort(old)
		ph.Stopped = append(current, old...)
		out = append(out, ph)
	}
	return out
}

// placementHostsWithPending adds the VMs this loop started earlier in the same
// decision, which host reports do not show yet.
func (l *poolLoop) placementHostsWithPending(rt *PoolRuntime, hosts []ports.HostState) []PlacementHost {
	phs := placementHosts(l.name, rt, hosts)
	for _, node := range l.pendingTart {
		serial, vm, ok := SplitTartNode(node, hosts)
		if !ok {
			continue
		}
		for i := range phs {
			if phs[i].Serial != serial {
				continue
			}
			phs[i].Running++
			phs[i].PoolVMs++
			phs[i].Stopped = slices.DeleteFunc(phs[i].Stopped, func(s string) bool { return s == vm })
		}
	}
	return phs
}

// newVMName picks "<pool>-<n>" with the smallest n unused on the host
// (including VMs started earlier in this decision).
func (l *poolLoop) newVMName(serial string, hosts []ports.HostState) string {
	used := map[string]bool{}
	for _, h := range hosts {
		if h.Serial != serial {
			continue
		}
		for _, vm := range h.VMs {
			used[vmName(vm.ID)] = true
		}
	}
	for _, node := range l.pendingTart {
		if s, vm, ok := SplitTartNode(node, hosts); ok && s == serial {
			used[vm] = true
		}
	}
	for n := 1; ; n++ {
		name := fmt.Sprintf("%s-%d", l.name, n)
		if !used[name] {
			return name
		}
	}
}
