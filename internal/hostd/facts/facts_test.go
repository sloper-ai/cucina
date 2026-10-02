// SPDX-License-Identifier: FSL-1.1-ALv2

package facts_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/fakes"
	"github.com/sloper-ai/cucina/internal/hostd/facts"
	"github.com/sloper-ai/cucina/internal/ports"
)

// Guards: T14 / R-MAC-5 — diagnostics identify unsupported nested macOS guests
// from actual host probes; metadata text and hv_support alone are not proof.
func TestGatherVirtualizationEvidence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		model      string
		profiler   string
		hv         string
		wantModel  string
		wantHV     any
		wantAvail  any
		wantReason string
	}{
		{name: "Tart guest", model: "VirtualMac2,1", hv: "0", wantModel: "VirtualMac2,1", wantHV: float64(0), wantAvail: false, wantReason: "nested-macos"},
		{name: "nested Linux support is not macOS support", model: "VirtualMac2,1", hv: "1", wantModel: "VirtualMac2,1", wantHV: float64(1), wantAvail: false, wantReason: "nested-macos"},
		{name: "guest despite unavailable hypervisor probe", model: "VirtualMac2,1", wantModel: "VirtualMac2,1", wantAvail: false, wantReason: "nested-macos"},
		{name: "profiler fallback", profiler: "VirtualMac2,1", hv: "0", wantModel: "Apple Virtual Machine (VirtualMac2,1)", wantHV: float64(0), wantAvail: false, wantReason: "nested-macos"},
		{name: "hypervisor disabled", model: "Macmini9,1", hv: "0", wantModel: "Macmini9,1", wantHV: float64(0), wantAvail: false, wantReason: "hypervisor-unavailable"},
		{name: "hypervisor alone proves no macOS support", model: "Macmini9,1", hv: "1", wantModel: "Macmini9,1", wantHV: float64(1), wantReason: "macos-support-unverified"},
		{name: "probes unavailable", wantReason: "probe-unavailable"},
		{name: "invalid hypervisor output", model: "Macmini9,1", hv: "unexpected", wantModel: "Macmini9,1", wantReason: "probe-unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := fakes.NewClock(time.Unix(0, 0))
			ex := fakes.NewExec(clock, fakes.NewRand(1))
			ex.Handle("/usr/sbin/system_profiler", func(context.Context, ports.Command) (ports.ExecResult, error) {
				hardware := map[string]string{
					"machine_model": tc.profiler, "serial_number": "TEST-SERIAL", "chip_type": "Apple M4", "physical_memory": "16 GB",
				}
				if tc.profiler != "" {
					hardware["machine_name"] = "Apple Virtual Machine"
				}
				b, err := json.Marshal(map[string]any{"SPHardwareDataType": []map[string]string{hardware}})
				return ports.ExecResult{Stdout: b}, err
			})
			ex.Handle("/usr/sbin/sysctl", func(_ context.Context, c ports.Command) (ports.ExecResult, error) {
				out := map[string]string{"-n hw.ncpu": "10", "-n hw.model": tc.model, "-n kern.hv_support": tc.hv}[strings.Join(c.Args, " ")]
				if out == "" {
					return ports.ExecResult{ExitCode: 1}, nil
				}
				return ports.ExecResult{Stdout: []byte(out + "\n")}, nil
			})
			f, err := (facts.Gatherer{Exec: ex}).Gather(t.Context())
			require.NoError(t, err)
			b, err := json.Marshal(f)
			require.NoError(t, err)
			var doc struct {
				Virtualization map[string]any `json:"virtualization"`
			}
			require.NoError(t, json.Unmarshal(b, &doc))
			require.NotEmpty(t, doc.Virtualization, "facts must carry explicit virtualization evidence into diagnostics")
			require.Equal(t, tc.wantAvail, doc.Virtualization["available"])
			require.Equal(t, tc.wantReason, doc.Virtualization["reason"])
			require.Equal(t, tc.wantModel, doc.Virtualization["model"])
			require.Equal(t, tc.wantHV, doc.Virtualization["hv_support"])
		})
	}
}
