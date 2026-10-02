// SPDX-License-Identifier: FSL-1.1-ALv2

package config

import (
	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
)

// Tunables are the settings both the preferences and the controller can set.
type Tunables struct {
	Slots             int
	VMCPU             int // 0 = derive (cores-2)/slots
	VMMemoryGiB       int // 0 = derive (RAM-8GiB)/slots
	L2SizeGiB         int
	LogLevel          string
	CentralEndpoint   string // L2 upstream (controller-provided)
	SchedulerEndpoint string // relay upstream for VMs (controller-provided)
	// MaximumMessageSizeBytes is identical in every Buildbarn component (controller-provided; default 16 MiB).
	MaximumMessageSizeBytes uint64
}

// DefaultMaximumMessageSizeBytes is Buildbarn's message size when the controller sends none.
const DefaultMaximumMessageSizeBytes = 16 << 20

// Tunables overlays the controller's Welcome.slots and HostSettings on the
// preferences: non-zero controller values win ("overrides on top of the managed
// preferences", host.proto); identity and connection keys are never
// overridable. Slots are clamped to the hard 1..2 range.
func (c Config) Tunables(welcomeSlots uint32, hs *cucinav1.HostSettings) Tunables {
	t := Tunables{Slots: c.VMSlots, VMCPU: c.VMCPUCount, VMMemoryGiB: c.VMMemoryGiB, L2SizeGiB: c.L2SizeGiB, LogLevel: c.LogLevel,
		MaximumMessageSizeBytes: DefaultMaximumMessageSizeBytes}
	if welcomeSlots > 0 {
		t.Slots = int(welcomeSlots)
	}
	if hs != nil {
		if hs.GetVmCpu() > 0 {
			t.VMCPU = int(hs.GetVmCpu())
		}
		if hs.GetVmMemoryGib() > 0 {
			t.VMMemoryGiB = int(hs.GetVmMemoryGib())
		}
		if hs.GetL2SizeGib() > 0 {
			t.L2SizeGiB = int(hs.GetL2SizeGib())
		}
		switch hs.GetLogLevel() {
		case "debug", "info", "warn", "error":
			t.LogLevel = hs.GetLogLevel()
		}
		t.CentralEndpoint = hs.GetCentralEndpoint()
		t.SchedulerEndpoint = hs.GetSchedulerEndpoint()
		if n := hs.GetMaximumMessageSizeBytes(); n > 0 {
			t.MaximumMessageSizeBytes = n
		}
	}
	if t.Slots < 1 {
		t.Slots = 1
	}
	if t.Slots > 2 {
		t.Slots = 2
	}
	return t
}
