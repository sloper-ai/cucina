// SPDX-License-Identifier: FSL-1.1-ALv2

package lifecycle

import (
	"errors"
	"time"
)

// ErrUnknownVM is returned for commands naming a VM the host does not have.
var ErrUnknownVM = errors.New("unknown vm")

// StartRequest is the desired state carried by a StartVM command.
type StartRequest struct {
	Name       string
	Pool       string
	Node       string
	Image      string
	Generation string
	CPU        int
	MemoryGiB  int
	DiskGiB    int
	MaxAge     time.Duration
}

// RequestStart upserts a VM with intent running (StartVM, R-MAC-6). A changed
// image or generation does not disturb a running VM; it is re-cloned at its
// next start (persistent VMs, R-MAC-3).
func RequestStart(h *Host, r StartRequest) {
	i := h.Find(r.Name)
	if i < 0 {
		h.VMs = append(h.VMs, VM{Name: r.Name, Phase: Absent})
		i = len(h.VMs) - 1
	}
	vm := &h.VMs[i]
	changed := vm.Intent != WantRunning || vm.Image != r.Image || vm.Generation != r.Generation
	vm.Pool, vm.Node = r.Pool, r.Node
	vm.Image, vm.Generation = r.Image, r.Generation
	vm.CPU, vm.MemoryGiB, vm.DiskGiB, vm.MaxAge = r.CPU, r.MemoryGiB, r.DiskGiB, r.MaxAge
	vm.Intent = WantRunning
	vm.StopReason = ""
	if changed {
		vm.RetryAt = time.Time{}
	}
}

// RequestStop sets intent stopped (StopVM). The VM keeps its disk.
func RequestStop(h *Host, name string, timeout time.Duration, reason string) error {
	i := h.Find(name)
	if i < 0 {
		return ErrUnknownVM
	}
	h.VMs[i].Intent = WantStopped
	h.VMs[i].StopTimeout = timeout
	h.VMs[i].StopReason = reason
	return nil
}

// RequestDelete sets intent deleted (DeleteVM): stop if needed, then delete.
func RequestDelete(h *Host, name string) error {
	i := h.Find(name)
	if i < 0 {
		return ErrUnknownVM
	}
	h.VMs[i].Intent = WantDeleted
	return nil
}

// RequestReimage marks a VM for re-cloning from the golden image at its next
// start, or now if it is stopped (ReimageVM). An empty image keeps the current one.
func RequestReimage(h *Host, name, image string) error {
	i := h.Find(name)
	if i < 0 {
		return ErrUnknownVM
	}
	h.VMs[i].Reimage = true
	if image != "" {
		h.VMs[i].Image = image
	}
	return nil
}
