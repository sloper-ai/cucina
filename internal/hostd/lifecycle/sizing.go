// SPDX-License-Identifier: FSL-1.1-ALv2

package lifecycle

// HostCapacity is the host hardware relevant to VM sizing.
type HostCapacity struct {
	Cores     int
	MemoryGiB int
}

// Reserved resources kept for macOS, hostd and the L2 cache (R-MAC-3).
const (
	ReservedCores     = 2
	ReservedMemoryGiB = 8
	MinVMMemoryGiB    = 4
)

// Size returns the vCPUs and memory (GiB) of one VM (R-MAC-3): each VM gets
// (cores − 2)/slots vCPUs and (RAM − 8 GiB)/slots memory unless overridden.
// The first non-zero of (request, override, derived) wins. vCPUs are capped at
// the host's cores; memory is capped at the derived per-slot share so that the
// slots together never exceed the host (overcommitting memory would swap).
func Size(c HostCapacity, slots, reqCPU, reqMemGiB, overrideCPU, overrideMemGiB int) (cpu, memGiB int) {
	if slots < 1 {
		slots = 1
	}
	if slots > HardMaxRunning {
		slots = HardMaxRunning
	}
	derivedCPU := (c.Cores - ReservedCores) / slots
	if derivedCPU < 1 {
		derivedCPU = 1
	}
	derivedMem := (c.MemoryGiB - ReservedMemoryGiB) / slots
	if derivedMem < MinVMMemoryGiB {
		derivedMem = MinVMMemoryGiB
	}
	cpu = firstPositive(reqCPU, overrideCPU, derivedCPU)
	if c.Cores > 0 && cpu > c.Cores {
		cpu = c.Cores
	}
	memGiB = firstPositive(reqMemGiB, overrideMemGiB, derivedMem)
	if memGiB > derivedMem {
		memGiB = derivedMem
	}
	return cpu, memGiB
}

func firstPositive(v ...int) int {
	for _, x := range v {
		if x > 0 {
			return x
		}
	}
	return 0
}
