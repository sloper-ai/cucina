// SPDX-License-Identifier: FSL-1.1-ALv2

package workeragent_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/workeragent"
)

// Guards: contracts §5.2 file locations that the images' units and dead-man
// timers depend on, on Linux and (computed on any host) Windows.
func TestDefaultPaths(t *testing.T) {
	cases := []struct {
		goos string
		want map[string]string
	}{
		{"linux", map[string]string{
			"worker": "/etc/cucina/bb/worker.json", "runner": "/etc/cucina/bb/runner.json",
			"cert": "/etc/cucina/pki/worker.crt", "key": "/etc/cucina/pki/worker.key", "ca": "/etc/cucina/pki/ca.crt",
			"env": "/etc/cucina/env", "state": "/var/lib/cucina/agent.state.json",
			"activity": "/run/cucina/last-activity", "contact": "/run/cucina/last-contact",
		}},
		{"windows", map[string]string{
			"worker": `C:\ProgramData\cucina\bb\worker.json`, "runner": `C:\ProgramData\cucina\bb\runner.json`,
			"cert": `C:\ProgramData\cucina\pki\worker.crt`, "key": `C:\ProgramData\cucina\pki\worker.key`,
			"ca": `C:\ProgramData\cucina\pki\ca.crt`, "env": `C:\ProgramData\cucina\env`,
			"state":    `C:\ProgramData\cucina\agent.state.json`,
			"activity": `C:\ProgramData\cucina\run\last-activity`, "contact": `C:\ProgramData\cucina\run\last-contact`,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.goos, func(t *testing.T) {
			p := workeragent.DefaultPaths(tc.goos)
			require.Equal(t, tc.want, map[string]string{
				"worker": p.WorkerConfig(), "runner": p.RunnerConfig(), "cert": p.CertFile(), "key": p.KeyFile(),
				"ca": p.CAFile(), "env": p.EnvFile, "state": p.StateFile,
				"activity": p.LastActivityFile(), "contact": p.LastContactFile(),
			})
		})
	}
	w := workeragent.Windows
	require.Equal(t, `C:\`, w.Join("C:"))
	require.Equal(t, `C:\bb\cache`, w.Join(`C:\bb\`, "/cache/"))
	require.Equal(t, `C:\bb`, w.Dir(`C:\bb\cache`))
	require.Equal(t, `D:\x\ProgramData\cucina\pki\worker.key`, workeragent.DefaultPaths("windows").Under(`D:\x`).KeyFile())
}
